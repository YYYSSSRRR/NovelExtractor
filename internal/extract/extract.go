// Package extract 从任意网页 HTML 中抽取 meta_title / real_title / main_content。
//
// 设计约束（贯穿全部实现，也是与 novel-extractor 的根本分歧）：
//
//  1. 算法**不读任何 class/id 字符串**。判据只用三类信息：HTML 元素自身的
//     语义（<article>、<nav> 在任何站点上含义一致）、文本统计量
//     （字符数、标点率、链接密度）、DOM 结构（祖先、兄弟、深度）。
//     class="ptm-content" 只在一个站点上有意义，据此抽取等于背诵模板。
//
//  2. 不针对体裁写规则。没有「第X章」这样的章节号正则，也没有
//     novel/book/chapter 这样的关键词表。小说、新闻、问答走同一条路径。
//
//  3. 单页可判。不依赖跨页统计也能出结果；跨页信息（站点模板前后缀）
//     是锦上添花的增强，不是前提。见 ChromeLearner。
package extract

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Options 用于消融实验：关掉某个信号后重跑同一批页面，用指标变化证明
// 它确实在起作用。零值即完整算法，生产路径不需要改动任何开关。
type Options struct {
	DisablePunctGate   bool // 关掉标点率因子
	DisableLinkPenalty bool // 关掉链接密度惩罚
	DisablePropagation bool // 关掉祖先累积，退化成「选单块得分最高者」
	KeepAttribution    bool // 保留「本文来自 xxx(http://...)」这类署名行
}

// gates 是消融开关的运行时形态。
//
// 这里刻意用结构体字段而不是 map[string]bool。两者读起来一样直白，但
// score 与 accumulate 是在整棵 DOM 上**逐节点**执行的：每个候选块查两次
// map、每个元素节点再查一次，而 Go 的 map 查找即便命中也要算哈希、比字符串。
// 换成字段访问后不仅省掉这一步，编译器还能把判断提到循环外。
type gates struct {
	noPunct     bool // 关掉标点率因子
	noLink      bool // 关掉链接密度惩罚
	noPropagate bool // 关掉祖先累积
}

func (o Options) gates() gates {
	return gates{
		noPunct:     o.DisablePunctGate,
		noLink:      o.DisableLinkPenalty,
		noPropagate: o.DisablePropagation,
	}
}

// Result 是抽取产物。除三个必需输出字段外还带上若干中间统计量：
// 爬虫的正文页精筛与评测脚本都要用，带上可避免重新解析一遍 DOM。
type Result struct {
	MetaTitle   string
	RealTitle   string
	MainContent string

	// ContentChars 是正文的非空白字符数。
	ContentChars int
	// LinkChars 是渲染文本中来自 <a> 的字符数。
	LinkChars int
	// LinkRate 是正文链接密度，在渲染文本上度量（尚未剔除署名行，
	// 但署名行通常只有几十字符，对上千字的正文影响可忽略）。
	// 这是判断「有没有把导航抽进来」最灵敏的指标。
	LinkRate float64
	// PageChars / PageLinkRate 描述整页可见文本，作为正文占比的分母。
	PageChars    int
	PageLinkRate float64
	// ContainerTag 是选中容器的标签名，排查个案时有用。
	ContainerTag string

	// NoArticle 表示这一页**根本没有正文**，而不是「正文质量差」。
	//
	// 判据见 looksLikeArticle，全是结构性的。为 true 时 MainContent 与
	// ContentChars 一律是零值——上面那点文字不是文章，是整页样板（菜单、列表、
	// 页眉），它就不该以「正文」的名义交出去。要弄清为什么被判没了，看
	// ContainerTag / LinkRate / PageChars 这三个统计量。
	NoArticle bool
}

// Extractor 可复用。内部无可变状态，可安全并发调用。
type Extractor struct {
	opt Options
	g   gates
}

// New 构造抽取器。传零值 Options 即完整算法。
func New(opt Options) *Extractor {
	return &Extractor{opt: opt, g: opt.gates()}
}

// Extract 抽取单页。pageURL 目前只用于诊断，算法本身不依赖 URL 结构
// ——真实标题与正文的判定不应受 URL 形态影响。
func (e *Extractor) Extract(pageURL, htmlStr string) (Result, error) {
	return e.ExtractWithChrome(pageURL, htmlStr, Chrome{})
}

// ExtractWithChrome 在跨页学到的站点模板成分（ch）已就绪时抽取单页。
// 单页模式下传零值 Chrome，strip 是恒等变换，行为与 Extract 完全一致。
func (e *Extractor) ExtractWithChrome(pageURL, htmlStr string, ch Chrome) (Result, error) {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return Result{}, err
	}

	// JSON-LD 在 <script> 里，而 <script> 会被 prune 删掉，必须先取走
	jsonLD := jsonLDHeadline(doc)

	prune(doc)

	var res Result
	res.MetaTitle = metaTitle(doc)

	stats := statsOf(doc)
	if body := firstElement(doc, atom.Body); body != nil {
		if st := stats[body]; st != nil {
			res.PageChars = st.chars
			res.PageLinkRate = st.linkRate()
		}
	}

	own := ownScores(stats, e.g)
	accum, best := accumulate(doc, own, e.g)
	container := pickContainer(accum, stats, best)

	var sb strings.Builder
	if container != nil {
		res.ContainerTag = container.Data
		render(&sb, container, stats, false, &res)
	}
	rendered := sb.String()

	if n := countContentChars(rendered); n > 0 {
		res.LinkRate = float64(res.LinkChars) / float64(n)
	}

	content := normalize(rendered)

	title := pickRealTitle(doc, content, res.MetaTitle, jsonLD, ch)
	if title == "" {
		title = ch.strip(res.MetaTitle)
	}
	res.RealTitle = title

	content = dropLeadingTitleLine(content, title)
	res.MainContent = content
	res.ContentChars = countContentChars(content)

	// 判定放在最后：要用的四个量（容器、链接密度、字符数）到这里才齐，
	// 而且必须在下面清零 ContentChars 之前做。
	if !looksLikeArticle(&res) {
		res.NoArticle = true
		// 契约：MainContent 是「这一页的正文」，这一页没有正文，它就是空的。
		//
		// 也可以选择留着那段样板文字、让调用方自己看 NoArticle 决定要不要——
		// 但那样每一个调用方都得记得判，忘掉的那个就会把整页菜单当成文章
		// 收进语料，而它恰恰是这条判据要防的事。诊断信息并没有丢：容器标签、
		// 链接密度、整页字符数都留在 Result 的统计字段里，排查时看那些就够。
		res.MainContent = ""
		res.ContentChars = 0
	}
	return res, nil
}
