package extract

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// titleSeparators 是标题里常见的分隔符。站点名与栏目名几乎总是被它们
// 隔在标题的首部或尾部。
//
// 刻意**不含**「：」「，」「、」「·」——它们是标题正文的一部分
// （「习近平：坚持和发展中国特色社会主义」「心·逍遥」），
// 当成分隔符会把标题切坏。
var titleSeparators = map[rune]bool{
	'|': true, '-': true, '–': true, '—': true, '_': true,
	'/': true, '\\': true, '»': true, '«': true, '~': true,
}

// titleCandidate 是 real_title 的一个候选来源。
type titleCandidate struct {
	text  string
	prior float64 // 来源自身的先验可信度，与内容无关
}

// metaTitle 取第一个 <title> 的文本，即浏览器标签页上显示的那个。
// 这是题目三个输出字段里唯一没有歧义的一个。
func metaTitle(doc *html.Node) string {
	if t := firstElement(doc, atom.Title); t != nil {
		return collapseSpaces(nodeText(t))
	}
	return ""
}

// splitTitleSegments 按分隔符把 <title> 切成若干段，返回各段原文。
//
// 返回「段」而不是「重新拼接的前缀」：真实标题几乎总是其中恰好一段
// （通常是第一段），而重新拼接会产生「标题-栏目」这种不存在的混合体。
func splitTitleSegments(title string) []string {
	rs := []rune(title)
	var out []string
	start := 0
	for i, r := range rs {
		if !titleSeparators[r] {
			continue
		}
		if seg := strings.TrimSpace(string(rs[start:i])); seg != "" {
			out = append(out, seg)
		}
		start = i + 1
	}
	if seg := strings.TrimSpace(string(rs[start:])); seg != "" {
		out = append(out, seg)
	}
	return out
}

// titleAffinity 衡量候选与 <title> 的从属关系。
// 站点名污染几乎总是「往 <title> 尾部追加」，所以「候选是 <title> 的前缀」
// 是很强的支持证据；「候选是 <title> 的子串」弱一些。
func titleAffinity(cand, metaTitle string) float64 {
	c, t := foldForCompare(cand), foldForCompare(metaTitle)
	if c == "" || t == "" {
		return 0
	}
	switch {
	case c == t:
		return 0.5
	case strings.HasPrefix(t, c):
		return 1.0
	case strings.Contains(t, c):
		return 0.6
	}
	return 0
}

// lengthFit 对长度落在常见标题区间内的候选给满分，过短过长递减。
func lengthFit(n int) float64 {
	switch {
	case n < 4:
		return 0
	case n <= 60:
		return 1
	case n <= 120:
		return 0.5
	}
	return 0
}

// scoreTitleCandidate 给候选打分。
//
// 权重最高的是与正文的重合度：站点名、栏目名不会出现在正文段落里，
// 而真实标题常在正文开头被重复一次，这是最难被模板伪造的信号。
func scoreTitleCandidate(c titleCandidate, metaTitle, contentHead string, ch Chrome) float64 {
	n := utf8.RuneCountInString(c.text)
	if n < 4 || n > 120 {
		return -1
	}
	s := c.prior
	s += 1.0 * bigramOverlap(c.text, contentHead)
	s += 0.5 * titleAffinity(c.text, metaTitle)
	s += 0.2 * lengthFit(n)
	// 跨页统计已判定为站点模板的字符串，不可能是本页标题
	f := foldForCompare(c.text)
	if f != "" && (f == foldForCompare(ch.Prefix) || f == foldForCompare(ch.Suffix)) {
		s -= 1.5
	}
	return s
}

// titleCandidates 汇总所有候选来源。
//
// 优先用跨站通用的公开标准（OpenGraph、Twitter Card、schema.org、HTML5 的
// <h1> 语义），它们存在时最可靠；再退到对 <title> 做切分。全程不依赖任何
// class/id：真实页面上「用户看到的标题」往往在 <div id="title"> 里，
// 但那个 id 是站点自造的，不可移植。
func titleCandidates(doc *html.Node, metaTitle, jsonLD string, ch Chrome) []titleCandidate {
	out := make([]titleCandidate, 0, 10)
	add := func(s string, prior float64) {
		if s = collapseSpaces(s); s != "" {
			out = append(out, titleCandidate{s, prior})
		}
	}

	// OpenGraph 的 og:title 按规范就是「本页标题」，可信度最高
	for _, s := range metaContents(doc, "og:title") {
		add(s, 1.00)
	}
	for _, s := range metaContents(doc, "twitter:title") {
		add(s, 0.95)
	}
	add(jsonLD, 0.90) // schema.org headline
	for i, h := range elementsWithTag(doc, atom.H1, 5) {
		add(nodeText(h), 0.85-0.05*float64(i))
	}

	// <title> 各段。先剥掉跨页学到的站点成分（前缀书名、后缀站名），
	// 剩下的才是每页变化的那部分，再按分隔符切段；靠前的段先验略高，
	// 中文站更常把标题放首位。
	base := ch.strip(metaTitle)
	for i, s := range splitTitleSegments(base) {
		add(s, 0.30-0.05*float64(i))
	}
	// 剥离后的整串仍是候选：有些站标题里不含分隔符，切不出段
	add(base, 0.25)
	if foldForCompare(base) != foldForCompare(metaTitle) {
		add(metaTitle, 0.10) // 未剥离的完整 <title> 兜底
	}

	return out
}

// pickRealTitle 在候选池里选一个。返回空字符串表示一个都没选出来，
// 调用方据此退回 metaTitle。
func pickRealTitle(doc *html.Node, content, metaTitle, jsonLD string, ch Chrome) string {
	cands := titleCandidates(doc, metaTitle, jsonLD, ch)
	head := headRunes(content, 200)

	best, bestScore := "", 0.0
	for _, c := range cands {
		if s := scoreTitleCandidate(c, metaTitle, head, ch); s > bestScore {
			best, bestScore = c.text, s
		}
	}
	return best
}

// headlineFromJSON 从一段 JSON-LD 里取 schema.org 的 headline。
// 只认 headline，不认 name——name 在 Organization/WebSite 节点里指的是
// 站点名，取它会得到站点名而不是文章标题。
func headlineFromJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return ""
	}
	return headlineFromValue(v, 0)
}

func headlineFromValue(v any, depth int) string {
	if depth > 6 {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t["headline"].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
		for _, k := range []string{"@graph", "mainEntity", "itemListElement"} {
			if s := headlineFromValue(t[k], depth+1); s != "" {
				return s
			}
		}
	case []any:
		for _, e := range t {
			if s := headlineFromValue(e, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}

// jsonLDHeadline 扫描页面里的 JSON-LD 块。必须在 prune 之前调用——
// <script> 会被 prune 删掉。
func jsonLDHeadline(doc *html.Node) string {
	for _, s := range elementsWithTag(doc, atom.Script, 20) {
		if !strings.Contains(strings.ToLower(attr(s, "type")), "ld+json") {
			continue
		}
		if h := headlineFromJSON(nodeText(s)); h != "" {
			return h
		}
	}
	return ""
}

// headRunes 取字符串前 n 个字符。用于拿正文开头做标题比对的参照。
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
