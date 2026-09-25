package extract

import (
	"math"
	"strings"
)

// 跨页模板过滤：把「同一域名下反复出现」的文本判为模板残留并剥掉。
//
// 这是抽取流程里唯一一处**不看单个页面**的清洗。理由很直接：一页里出现
// 「转载请注明出处」无法判定它是正文还是页脚，但当某域名 12 页里有 9 页
// 都出现同一串字符时，它按定义就不是这篇文章的内容。
//
// 手段是统计而不是规则——不维护「广告/版权/声明」这类词表，因为词表既写不全
// 也随站点漂移。代价是必须手里同时有同域的多篇正文，所以它只在语料模式
// （cmd/extractor -data）下生效：那种模式下整个语料都在盘上，同域页面可以
// 一次处理完。流式模式下单页不知道同域其它页写了什么，只能不做。
//
// 它同时是一把尺子：剥掉的字符占比就是「跨页模板残留率」，可作为无人工标注
// 情况下的抽取质量自检指标。
const (
	tmplN        = 5   // n-gram 长度。取 5 是因为 5 个连续汉字基本构成一个词组，再短会频繁偶然共现
	tmplMinFrac  = 0.5 // 出现在这个比例以上的页面里才算模板
	tmplMinPages = 3   // 同域至少这么多页才敢下判断
	tmplMinSpan  = 8   // 连续被判定为模板的字符数达到这个长度才动手切
	tmplMaxStrip = 0.5 // 单页最多剥掉这个比例的字符，超过就整页放弃剥离
)

// Templates 是一个域名学到的模板 n-gram 集合。零值/ nil 表示「没学到」，
// Strip 退化为恒等变换。
type Templates struct {
	grams map[string]bool
}

// LearnTemplates 从同一域名的多篇正文里统计模板片段。
//
// 判据是**文档频率**而不是词频：一个片段在某一页里出现 100 次说明它在重复
// 自己（那可能正是正文的修辞），在 100 页里的 60 页各出现 1 次才说明它是
// 站点模板。所以这里统计的是「出现该 n-gram 的页面数」。
func LearnTemplates(contents []string) *Templates {
	pages := 0
	df := make(map[string]int, 1024)
	seen := make(map[string]struct{}, 256)
	var buf []rune

	for _, c := range contents {
		if c == "" {
			continue
		}
		buf = buf[:0]
		for _, r := range c {
			if !isSpaceRune(r) {
				buf = append(buf, r)
			}
		}
		if len(buf) < tmplN {
			continue
		}
		pages++
		clear(seen)
		for i := 0; i+tmplN <= len(buf); i++ {
			g := string(buf[i : i+tmplN])
			if _, dup := seen[g]; dup {
				continue // 一页之内只记一次，压成文档频率
			}
			seen[g] = struct{}{}
			df[g]++
		}
	}

	if pages < tmplMinPages {
		return nil
	}
	need := int(math.Ceil(tmplMinFrac * float64(pages)))
	if need < 2 {
		need = 2 // 两页的语料里「过半」等于「两页都有」，不是统计，是巧合
	}

	grams := make(map[string]bool, len(df)/4)
	for g, n := range df {
		if n >= need {
			grams[g] = true
		}
	}
	if len(grams) == 0 {
		return nil
	}
	return &Templates{grams: grams}
}

// Strip 切掉正文里的模板片段。
//
// 按行处理而不是整篇处理：正文的段落边界就是换行，模板片段总是整段或
// 段末的固定尾巴，按行切不会把两段之间的正常文字连坐。
func (t *Templates) Strip(content string) string {
	if t == nil || len(t.grams) == 0 || content == "" {
		return content
	}

	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, t.stripLine(ln))
	}
	res := strings.Join(out, "\n")

	// 安全闸：如果剥掉了一大半，说明要么这个域名的页面本身就高度重复
	// （分页版本、同一篇的多个 URL），要么统计出了问题。这两种情况下
	// 「保留原文」都比「交一份残缺正文」更可辩护。
	if countContentChars(res)*2 < countContentChars(content) {
		return content
	}
	return res
}

// stripLine 切掉一行里的模板片段，返回切完并去掉首尾空白的行。
func (t *Templates) stripLine(line string) string {
	r := []rune(line)
	if len(r) < tmplN {
		return strings.TrimSpace(line)
	}

	covered := make([]bool, len(r))
	for i := 0; i+tmplN <= len(r); i++ {
		if t.grams[string(r[i:i+tmplN])] {
			for j := i; j < i+tmplN; j++ {
				covered[j] = true
			}
		}
	}

	var sb strings.Builder
	sb.Grow(len(line))
	for i := 0; i < len(r); {
		if !covered[i] {
			sb.WriteRune(r[i])
			i++
			continue
		}
		j := i
		for j < len(r) && covered[j] {
			j++
		}
		// 边界处总会有几个字符被相邻 n-gram 带着标上，孤立的一小段不算模板
		if j-i < tmplMinSpan {
			sb.WriteString(string(r[i:j]))
		}
		i = j
	}
	return strings.TrimSpace(sb.String())
}
