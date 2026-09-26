package extract

import (
	"math"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// 打分与容器选择的全部可调常量。它们只在小说站上调过，随后冻结，
// 再拿到 990 多个未见域名上评测——见 README「对比实验」。
const (
	punctFullCredit  = 0.15  // 标点率达到此值即拿满分
	lengthFullCredit = 200.0 // 字符数达到此值即拿满分
	climbRatio       = 0.75  // 上溯阈值：父容器累积分达到自身的这个比例就取父
	climbLinkSlack   = 0.10  // 上溯时允许的链接密度恶化幅度
	minBlockChars    = 8     // 低于此字符数的块基本无信息量
	skipLinkRate     = 0.50  // 整块链接密度超过此值即判为导航，输出时跳过
	propagateDepth   = 2     // 向上汇入层数：直接子块 + 孙块
)

// isBlockCandidate 判定哪些元素可能承载正文。
// 判据是「块级 + 能独立成段」，不含任何站点模板信息。
func isBlockCandidate(a atom.Atom) bool {
	switch a {
	case atom.P, atom.Div, atom.Article, atom.Section, atom.Td, atom.Th,
		atom.Blockquote, atom.Li, atom.Pre, atom.Dd, atom.Figcaption:
		return true
	}
	return false
}

// score 计算单个块的正文得分。四个因子相乘，任一不满足即被压制：
//
//	textDensity  字符/标签——正文远高于导航（导航是标签多、文字少）
//	punctGate    标点率——正文有句读，导航条没有
//	1-linkRate   链接密度——直接把导航/列表/标签云打到接近 0
//	lengthGate   长度——太短的块不可能是正文
//
// 乘性组合而不是加权求和是刻意的：加权求和下，只要某一项极高就能
// 掩盖其余项的缺陷，而正文必须四项同时成立。
func score(st *textStats, g gates) float64 {
	if st.chars < minBlockChars {
		return 0
	}
	t := float64(st.chars)
	density := t / float64(st.elems+1)

	punctGate := 1.0
	if !g.noPunct {
		punctGate = math.Min((float64(st.punct)/t)/punctFullCredit, 1.0)
	}

	linkFactor := 1.0
	if !g.noLink {
		linkFactor = 1 - st.linkRate()
	}

	lengthGate := math.Min(t/lengthFullCredit, 1.0)

	return math.Log1p(density) * (0.2 + 0.8*punctGate) * linkFactor * lengthGate
}

// ownScores 预计算每个候选块的原始得分。累积与容器选择都会反复取用，
// 算一次存表。
func ownScores(stats map[*html.Node]*textStats, g gates) map[*html.Node]float64 {
	m := make(map[*html.Node]float64, len(stats))
	for n, st := range stats {
		if isBlockCandidate(n.DataAtom) {
			m[n] = score(st, g)
		}
	}
	return m
}

// accumulate 把子块得分汇入父容器，返回每个节点的累积分与全局最高分节点：
//
//	accum(n) = own(n) + Σ own(直接子块) + 0.5 × Σ own(孙块)
//
// 只向上汇入两层是刻意的。若沿祖先链无限累加，最外层的整页 wrapper
// 必然因「包含所有内容」而胜出，正文容器就永远选不出来——这是
// naive 实现的典型失败模式。
//
// 但两层折半只挡住「无限上溯」，挡不住「一轮聚合」：own 只对 blockCandidate
// 填过值，<body>/<main>/<ul> 这类结构元素的 own 恒为 0，可它们照样能从直接
// 子块拿满学分。于是当正文容器**没有块级子元素**时（正文用 <br> 分段而不是
// <p>，移动端小说站几乎都是这样），双方就调了个个儿：
//
//	<div class="content">  accum = own(自己)               = 3.32
//	<body>                 accum = 0 + own(content) + …    = 4.08   ← 赢了
//
// 正文容器以自己一己之力去比「自己 + 邻居 + 半层孙子」，必然输，然后
// looksLikeArticle 看到容器是 body 就判这页没有正文——正文其实好端端地在
// 第二名待着。在 data/novel.json 的 801 页抽样里，旧规则下 63.9% 的页面是被
// 这类结构容器赢走的（其中 <body> 独占 429 页），空结果率因此停在 66.4%。
//
// 所以当选资格与累计是两回事：**accum 谁都算，但只有自己就能承载正文的
// 元素（own > 0）才有资格当选**。结构容器一个都不参选，它们的作用是被
// pickContainer 上溯进去——那条路径本来就存在，而且当初能让结构容器赢的
// 页面，其 accum 必然高于任何一个子块，上溯的 0.75 倍阈值一定满足，
// 所以修完之后这些页面的容器与修之前完全相同，不会左右横跳。
func accumulate(root *html.Node, own map[*html.Node]float64, g gates) (map[*html.Node]float64, *html.Node) {
	accum := make(map[*html.Node]float64, len(own))
	var best *html.Node

	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		// 根节点是 DocumentNode 而非元素，必须继续下探而不是直接返回
		if n.Type != html.ElementNode {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		}
		var childSum, grandSum float64
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			childSum += own[c]
			for g := c.FirstChild; g != nil; g = g.NextSibling {
				if g.Type == html.ElementNode {
					grandSum += own[g]
				}
			}
			walk(c)
		}
		a := own[n]
		// 参选资格只看 own：own 恒为 0 的元素（结构容器、纯导航块）不管聚合到
		// 多少分都不能当选。见函数注释里的反例。
		eligible := own[n] > 0
		if !g.noPropagate {
			a += childSum + 0.5*grandSum
		}
		accum[n] = a
		if eligible && (best == nil || a > accum[best]) {
			best = n
		}
	}
	walk(root)
	return accum, best
}

// pickContainer 从累积分最高点出发向上修正。
//
// 正文常被一两层无语义的 wrapper 包住，或（更常见的）被 CSS 分栏拆成
// 多个并列 div——后一种情况下各个 div 单独看分数都不高，而它们的共同
// 父容器累积后最高。上溯同时检查链接密度不恶化，避免爬进靠导航撑起
// 分数的整页容器。
func pickContainer(accum map[*html.Node]float64, stats map[*html.Node]*textStats, best *html.Node) *html.Node {
	if best == nil {
		return nil
	}
	base := accum[best]
	for p := best.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode || p.DataAtom == atom.Body || p.DataAtom == atom.Html {
			break
		}
		if accum[p] < climbRatio*base {
			break
		}
		cur, par := stats[best], stats[p]
		if cur != nil && par != nil && par.linkRate() > cur.linkRate()+climbLinkSlack {
			break
		}
		best = p
	}
	return best
}

// nonProseAtom 列出「即便得分最高也不可能是正文」的元素。
//
// 与 dropSubtree 同源：这些标签的含义由 HTML 规范定义，任何站点上都一致，
// 用它们不违反「算法不读 class/id」的约束。列表、表格、页眉是导航与版式的
// 载体——正文可以是列表里的**一项**，但正文本身不会是整个 <ul>。
var nonProseAtom = map[atom.Atom]bool{
	atom.Ul: true, atom.Ol: true, atom.Dl: true, atom.Dd: true, atom.Dt: true,
	atom.Table: true, atom.Tbody: true, atom.Thead: true, atom.Tr: true,
	atom.Header: true, atom.Footer: true, atom.Nav: true, atom.Aside: true,
	atom.Form: true, atom.Select: true,
}

// minArticleChars 是「短到不成句」的下限，不是「够不够进语料」的门槛。
//
// 取 80 这个量级是为了排除「整页只抽出来 2 个字符」「36 个字符的页眉」
// 这类显然不是正文的结果。「正文要多长才收」是语料策略，属于调用方
// （爬虫的 -min-content），不该由算法替它定。
const minArticleChars = 80

// looksLikeArticle 判断选中的容器是不是真的承载正文。
//
// 存在的理由：pickContainer 阻止的是「向上爬到 <body>」，但阻止不了 best
// **一开始就是 <body>**。于是一个纯菜单页、列表页或工具页同样会被判出
// 「正文」，整页样板文字就这样进了语料——实测 263 企业邮（body，2164 字符）、
// 汽车之家车型列表（dd，130 字符）、aizhan 的链接站（ul）都是这么来的。
//
// 四条判据全是结构性的，没有一条是对着某个站点调的：
//
//	容器是 html/body   没有任何子区域在竞争中胜出，说明这一页没有正文主体
//	容器是非散文元素    胜出的是列表/表格/页眉，即导航或版式
//	链接密度过高        抽出来的文字以链接为主，那是导航不是文章
//	短到不成句          见 minArticleChars
//
// 链接密度这一条刻意复用 skipLinkRate：渲染阶段「整块链接密度超过它即跳过」
// 用的就是这个阈值，容器级的判断不该另立一套口径。
//
// 在 44 页人工标注样本（14 篇真文章 + 30 页导航/列表/工具页）上：
// 误杀 0 篇，拦下 9 页（另有 9 页本就抽不出字符）。剩下 12 页「一个 div
// 确实装着全页大部分文字」的站点拦不下来——那需要的是正文边界识别，
// 不是再加一个阈值去凑，所以这里不凑。
func looksLikeArticle(res *Result) bool {
	switch res.ContainerTag {
	case "", "html", "body":
		return false
	}
	if nonProseAtom[atom.Lookup([]byte(res.ContainerTag))] {
		return false
	}
	if res.LinkRate > skipLinkRate {
		return false
	}
	return res.ContentChars >= minArticleChars
}

// isBlockElement 判定元素是否为块级，即「能独立成段」。
//
// 输出阶段的处理分两类：块级元素做「整块跳过」判断（导航、相关阅读、标签云
// 都是整块出现的），行内元素（<a>、<span>、<em>…）不能套用同一条规则——
// 它们自身的链接密度天然接近 1，一挡就把正文里的内联链接连文字一起删掉，
// 句子会从中间断掉。
func isBlockElement(a atom.Atom) bool {
	return isBlockBoundary[a] || isBlockCandidate(a)
}

// isBlockBoundary 列出会在视觉上产生换行的标签，用于在输出文本里
// 还原段落边界。输出要求「包含换行」，靠的就是这一步。
var isBlockBoundary = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Br: true, atom.Tr: true, atom.Hr: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Li: true, atom.Blockquote: true, atom.Pre: true, atom.Section: true,
	atom.Article: true, atom.Header: true, atom.Table: true, atom.Ul: true, atom.Ol: true,
	atom.Dl: true, atom.Dt: true, atom.Dd: true, atom.Figcaption: true, atom.Caption: true,
}

// render 按文档序输出容器内的可见文本，块级边界插入换行。
//
// 「整块跳过」只对块级元素生效。行内元素（<a>、<span>…）不能套用同一条规则：
// 它们自身的链接密度天然接近 1，一挡就会把正文段落里的内联链接连文字一起删掉，
// 句子从中间断掉——而块级层面已经把导航、相关阅读、标签云拦住了，行内无需再拦。
func render(sb *strings.Builder, n *html.Node, stats map[*html.Node]*textStats, inLink bool, res *Result) {
	switch n.Type {
	case html.TextNode:
		sb.WriteString(n.Data)
		if inLink {
			for _, r := range n.Data {
				if !unicode.IsSpace(r) {
					res.LinkChars++
				}
			}
		}
		return
	case html.ElementNode:
		if isBlockElement(n.DataAtom) {
			if st := stats[n]; st != nil && st.chars > 0 && st.linkRate() > skipLinkRate {
				return
			}
		}
		if n.DataAtom == atom.A {
			inLink = true
		}
		boundary := isBlockBoundary[n.DataAtom]
		if boundary {
			sb.WriteByte('\n')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			render(sb, c, stats, inLink, res)
		}
		if boundary {
			sb.WriteByte('\n')
		}
		return
	default:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			render(sb, c, stats, inLink, res)
		}
	}
}
