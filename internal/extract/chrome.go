package extract

import (
	"math"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// Chrome 是某个域名上学到的标题模板成分——站点名、栏目名、书名这一类
// 「每页都一样」的字符串。
type Chrome struct {
	Prefix string
	Suffix string
}

// strip 从标题两端剥掉学到的模板成分。
//
// 刻意要求剥完还剩至少一个字符：宁可少剥，也不能把一个标题剥成空串。
// 剥完还会清掉残留在两端的分隔符与空白（"书名_标题" 剥掉"书名"后
// 会剩下 "_标题"）。
func (c Chrome) strip(s string) string {
	r := []rune(s)
	if p := []rune(c.Prefix); len(p) > 0 && len(r) > len(p) && strings.HasPrefix(s, c.Prefix) {
		r = r[len(p):]
	}
	if suf := []rune(c.Suffix); len(suf) > 0 && len(r) > len(suf) && strings.HasSuffix(string(r), c.Suffix) {
		r = r[:len(r)-len(suf)]
	}
	r = trimTitleEdges(r)
	return string(r)
}

// trimTitleEdges 去掉两端的空白与分隔符。剥掉模板成分后常会留下
// 孤立的分隔符，不清掉会污染后续的长度与相似度判断。
func trimTitleEdges(r []rune) []rune {
	for len(r) > 0 && (isSpaceRune(r[0]) || titleSeparators[r[0]]) {
		r = r[1:]
	}
	for len(r) > 0 && (isSpaceRune(r[len(r)-1]) || titleSeparators[r[len(r)-1]]) {
		r = r[:len(r)-1]
	}
	return r
}

func isSpaceRune(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ' ' || r == '　'
}

// ChromeLearner 从同一域名的多页 <title> 里学出站点模板成分。
//
// 核心观察：站点名、栏目名、书名这类成分是**域名级常量**，而用户想看的
// 标题是每页变化的那个部分。于是
//
//	学习 = 求该域名所有 <title> 的最长公共前缀与最长公共后缀
//	应用 = 从标题两端剥掉它们
//
// 这条路径不需要站点名单、不需要正则、也不需要知道体裁。而且页数越多越准：
// 从 5 页涨到 100 页时公共前后缀的估计只会更稳。这是「用数据规模换泛化」的
// 直接体现——单页看不出「我的明星老婆免费章节(花枫年华)」是书名（它和章节名
// 之间连分隔符都没有，任何切分规则都切不开），但跨页一看它每页都在，立刻暴露。
//
// 并发安全：爬虫侧多 worker 会并发调用 Observe。
type ChromeLearner struct {
	minPages int     // 至少观察到这么多页才开始学
	minFrac  float64 // 一个字符要出现在多大比例的页面里才算模板成分
	maxKeep  float64 // 剥掉的部分不得超过标题总长的这个比例

	mu      sync.Mutex
	seen    map[string]*domainTitles
	learned map[string]Chrome
}

// domainTitles 保存一个域名已见到的标题。
//
// 存 []rune 而不是 string：公共前后缀要逐字符比较，若每次重算都重新做一遍
// []rune 转换，光是这一步就占掉整个学习开销的可观比例。
type domainTitles struct {
	runes [][]rune
	// at 是上次重算时的样本数。重算按几何间隔（3、6、12、24…）触发，
	// 于是总重算量是 O(N) 而不是 O(N²)：每来一页就把全部历史重扫一遍，
	// 在每域 100 页的规模下是纯粹重复劳动，而前后缀估计几页之后就基本稳定。
	at int
}

// maxTitlesPerDomain 是单域名保留的样本上限：模板成分的估计早已收敛，
// 再攒只是白占内存。
const maxTitlesPerDomain = 256

// NewChromeLearner 返回默认参数的学习器：3 页起学、60% 页面共现才算数、
// 最多剥掉标题的 70%。
func NewChromeLearner() *ChromeLearner {
	return &ChromeLearner{
		minPages: 3,
		minFrac:  0.6,
		maxKeep:  0.7,
		seen:     make(map[string]*domainTitles),
		learned:  make(map[string]Chrome),
	}
}

// Observe 记录一页的 <title>，必要时重新学习该域名的模板成分。
func (l *ChromeLearner) Observe(domain, metaTitle string) {
	if domain == "" || metaTitle == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	d := l.seen[domain]
	if d == nil {
		d = &domainTitles{}
		l.seen[domain] = d
	}
	if len(d.runes) >= maxTitlesPerDomain {
		return
	}
	d.runes = append(d.runes, []rune(metaTitle))

	n := len(d.runes)
	if n < l.minPages || n < 2*d.at {
		return
	}
	d.at = n

	pre := delimit(commonEdge(d.runes, l.minFrac, l.maxKeep, true), true)
	suf := delimit(commonEdge(d.runes, l.minFrac, l.maxKeep, false), false)
	if utf8.RuneCountInString(pre) < minAffix {
		pre = ""
	}
	if utf8.RuneCountInString(suf) < minAffix {
		suf = ""
	}
	l.learned[domain] = Chrome{Prefix: pre, Suffix: suf}
}

// minAffix 是学到的模板成分的最短长度。单字符的「公共前后缀」几乎必然
// 是巧合，不足以支撑一次剥离。
const minAffix = 2

// delimit 把一个学到的公共前后缀收缩到分隔符边界上。
//
// 逐位投票只知道「哪些字符每页都一样」，不知道词边界在哪。不收缩的话边界会
// 落在词内部——实测踩到的坑：某域所有章节标题都以「第」开头，于是「第」被
// 当成前缀剥掉，「第三百八十四章大凶」变成「三百八十四章大凶」。
//
// 收缩规则：前缀保留到最后一个分隔符为止，后缀从第一个分隔符开始，也就是
// **让模板成分靠在标题一侧的边界必定是分隔符**。整段里一个分隔符都没有就
// 整段弃用——宁可少剥（DOM 邻接候选会补上），也不能把标题切坏。
func delimit(affix string, fromHead bool) string {
	r := []rune(affix)
	idx := -1
	for i, c := range r {
		if !titleSeparators[c] && !isSpaceRune(c) {
			continue
		}
		if fromHead {
			idx = i // 前缀取最后一个分隔符
		} else if idx < 0 {
			idx = i // 后缀取第一个分隔符
		}
	}
	if idx < 0 {
		return ""
	}
	if fromHead {
		return string(r[:idx+1])
	}
	return string(r[idx:])
}

// Chrome 返回该域名已学到的模板成分。未学过则返回零值（strip 是恒等变换）。
func (l *ChromeLearner) Chrome(domain string) Chrome {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.learned[domain]
}

// ScanTitle 只取 <title> 的纯文本，用于批量输入的第一遍扫描。
//
// 这一遍**不建 DOM 树**，边走 tokenizer 边在 </title> 处收工，因此比完整
// 解析便宜得多。批处理管道要先用它把同域的所有 <title> 收齐才能学到模板
// 成分，若第一遍就建树，等于每页解析两次。
func ScanTitle(htmlStr string) string {
	z := html.NewTokenizer(strings.NewReader(htmlStr))
	inTitle, depth := false, 0
	var sb strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return collapseSpaces(sb.String())
		case html.StartTagToken:
			name, _ := z.TagName()
			if string(name) != "title" {
				continue
			}
			if inTitle {
				depth++
			} else {
				inTitle = true
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) != "title" || !inTitle {
				continue
			}
			if depth == 0 {
				return collapseSpaces(sb.String())
			}
			depth--
		case html.TextToken:
			if inTitle {
				sb.Write(z.Text())
			}
		}
	}
}

// commonEdge 求一组标题的公共前缀（fromHead=true）或公共后缀。
//
// 用逐位多数投票而不是「两两求最长公共前缀」：后者只要有一页标题是异类
// （比如错误页、专题页）就会把结果拉到很短。逐位投票要求某个字符在
// ≥minFrac 比例的页面里都出现，对离群值免疫。
func commonEdge(runes [][]rune, minFrac, maxKeep float64, fromHead bool) string {
	minLen := math.MaxInt
	for _, r := range runes {
		if len(r) < minLen {
			minLen = len(r)
		}
	}
	need := int(math.Ceil(minFrac * float64(len(runes))))
	limit := int(float64(minLen) * maxKeep) // 不许剥掉超过这个比例
	if limit > minLen {
		limit = minLen
	}

	var out []rune
	counts := make(map[rune]int, 8) // 复用同一张表，逐个位置 clear
	for k := 0; k < limit; k++ {
		clear(counts)
		for _, r := range runes {
			idx := k
			if !fromHead {
				idx = len(r) - 1 - k
			}
			if idx >= 0 && idx < len(r) {
				counts[r[idx]]++
			}
		}
		bestCh, bestN := rune(0), 0
		for ch, n := range counts {
			if n > bestN {
				bestCh, bestN = ch, n
			}
		}
		if bestN < need {
			break
		}
		out = append(out, bestCh)
	}

	if !fromHead {
		// 后缀是倒着收集的，翻回来
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	// 此处刻意不做 trimTitleEdges：分隔符正是 delimit 用来定位边界的依据
	return string(out)
}
