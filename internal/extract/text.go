package extract

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// textStats 是一个 DOM 子树内用于判别正文的文本统计量。
// 刻意全部用整数计数表达：这些量在整棵 DOM 上被反复计算，
// 保持在计数器层面意味着热路径上不构造任何中间字符串。
type textStats struct {
	chars     int // 去空白后的字符数
	punct     int // 中英文标点字符数
	linkChars int // <a> 后代中的字符数
	elems     int // 后代元素个数
}

func (s *textStats) add(o textStats) {
	s.chars += o.chars
	s.punct += o.punct
	s.linkChars += o.linkChars
	s.elems += o.elems
}

// linkRate 是链接密度：子树里有多少比例的字符落在 <a> 里。
// 导航栏、列表页、标签云的这个值接近 1，正文接近 0，
// 是全文区分度最高的单一信号。
func (s *textStats) linkRate() float64 {
	if s.chars == 0 {
		return 0
	}
	return float64(s.linkChars) / float64(s.chars)
}

// punctASCII 覆盖 ASCII 标点。非 ASCII 标点在 isPunct 的 switch 里处理。
// 用查表 + switch 而不是 regexp：标点计数在最内层逐字符执行，
// 是整个抽取流程里调用最密集的函数。
var punctASCII = [128]bool{}

func init() {
	for _, c := range []byte(",.!?;:") {
		punctASCII[c] = true
	}
}

func isPunct(r rune) bool {
	if r < 128 {
		return punctASCII[r]
	}
	switch r {
	case '，', '。', '！', '？', '；', '：', '、',
		'（', '）', '《', '》', '〈', '〉', '【', '】', '〔', '〕',
		'“', '”', '‘', '’', '…', '—', '～',
		'·', '•', '«', '»', '¡', '¿':
		return true
	}
	return false
}

// countText 累加一段文本的字符/标点统计。isLink 为真时同时计入链接字符，
// 使调用方可以事后算出任意子树的链接密度。
func countText(s string, isLink bool, st *textStats) {
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		st.chars++
		if isLink {
			st.linkChars++
		}
		if isPunct(r) {
			st.punct++
		}
	}
}

// foldForCompare 把字符串归一到"只剩文字"，用于标题与正文的相似度比较。
// 去掉空白与标点，使「第十章 村子的防卫力」与「第十章村子的防卫力」判为相同。
func foldForCompare(s string) string {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		if unicode.IsSpace(r) || isPunct(r) {
			continue
		}
		b = utf8.AppendRune(b, unicode.ToLower(r))
	}
	return string(b)
}

// bigramOverlap 返回 a 的字符 bigram 有多大比例出现在 b 中。
// 用 bigram 而不是单字：中文里单字重合太容易偶然命中，bigram 才代表
// 真实短语级别的共现。
func bigramOverlap(a, b string) float64 {
	a, b = foldForCompare(a), foldForCompare(b)
	ra := []rune(a)
	if len(ra) == 0 {
		return 0
	}
	if len(ra) == 1 {
		if strings.Contains(b, a) {
			return 1
		}
		return 0
	}
	hit, total := 0, 0
	for i := 0; i+2 <= len(ra); i++ {
		total++
		if strings.Contains(b, string(ra[i:i+2])) {
			hit++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hit) / float64(total)
}

// collapseSpaces 把行内连续空白折叠成单个空格并去掉首尾空白。
// 中文页面里大量空白来自 HTML 源码缩进，属于排版噪声；但行内单个空格
// 可能是真实的分词边界（尤其是中英混排），所以折叠而不是删除。
func collapseSpaces(s string) string {
	b := make([]byte, 0, len(s))
	pending := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if len(b) > 0 {
				pending = true
			}
			continue
		}
		if pending {
			b = append(b, ' ')
			pending = false
		}
		b = utf8.AppendRune(b, r)
	}
	return string(b)
}

// isAttribution 判断一行是不是「本文来自 xxx(http://...)」这类署名/出处行。
// 判据是「短行 + 含链接」：正常正文句子即使引用链接也不会短到这个程度，
// 而署名行几乎必然带一个自我指涉的 URL。这条规则与站点无关。
func isAttribution(line string) bool {
	if utf8.RuneCountInString(line) > 120 {
		return false
	}
	return strings.Contains(line, "://") || strings.Contains(line, "www.")
}

// normalize 把渲染出的原始文本整理成最终输出：逐行折叠空白、丢弃空行、
// 删掉署名行，最后用 \n 连接。输出要求「包含换行」，因此只在段落边界
// 保留 \n，行内不留多余空白。
func normalize(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = collapseSpaces(ln)
		if ln == "" || isAttribution(ln) {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// dropLeadingTitleLine 删掉正文开头与标题重复的行。
//
// 判据是归一化之后的包含关系——不用任何体裁正则，因此对小说、新闻、
// 问答一视同仁。这正是与原项目 novel-extractor 的关键分歧：那边用的是
// 「第X章/节/卷/回」章节号正则，换到新闻站上完全失效。
func dropLeadingTitleLine(content, title string) string {
	t := foldForCompare(title)
	if t == "" {
		return content
	}
	lines := strings.Split(content, "\n")
	drop := 0
	for drop < len(lines) && drop < 3 { // 只检查开头几行，避免误删正文中段的正常句子
		l := foldForCompare(lines[drop])
		if l == "" {
			drop++
			continue
		}
		// 三种重复形态：完全相同、是标题的片段（如标题被拆行）、包含标题
		dup := l == t ||
			(strings.Contains(t, l) && utf8.RuneCountInString(l) >= 4) ||
			strings.Contains(l, t)
		if !dup {
			break
		}
		drop++
	}
	return strings.Join(lines[drop:], "\n")
}

// countContentChars 数正文里的非空白字符数，用作长度类质量判据。
func countContentChars(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}
