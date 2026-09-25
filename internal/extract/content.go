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
func score(st *textStats, disabled map[string]bool) float64 {
	if st.chars < minBlockChars {
		return 0
	}
	t := float64(st.chars)
	density := t / float64(st.elems+1)

	punctGate := 1.0
	if !disabled["punct"] {
		punctGate = math.Min((float64(st.punct)/t)/punctFullCredit, 1.0)
	}

	linkFactor := 1.0
	if !disabled["link"] {
		linkFactor = 1 - st.linkRate()
	}

	lengthGate := math.Min(t/lengthFullCredit, 1.0)

	return math.Log1p(density) * (0.2 + 0.8*punctGate) * linkFactor * lengthGate
}

// ownScores 预计算每个候选块的原始得分。累积与容器选择都会反复取用，
// 算一次存表。
func ownScores(stats map[*html.Node]*textStats, disabled map[string]bool) map[*html.Node]float64 {
	m := make(map[*html.Node]float64, len(stats))
	for n, st := range stats {
		if isBlockCandidate(n.DataAtom) {
			m[n] = score(st, disabled)
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
func accumulate(root *html.Node, own map[*html.Node]float64, disabled map[string]bool) (map[*html.Node]float64, *html.Node) {
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
		if !disabled["propagate"] {
			a += childSum + 0.5*grandSum
		}
		accum[n] = a
		if best == nil || a > accum[best] {
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
// 整块链接密度超标的子树直接跳过——正文区里残留的「相关阅读」「上一篇」
// 「标签云」正是在这里被吃掉，全程不需要知道任何 class 名。
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
		if st := stats[n]; st != nil && st.chars > 0 && st.linkRate() > skipLinkRate {
			return
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
