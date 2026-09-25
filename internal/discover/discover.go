// Package discover 负责「找到这个站上哪些 URL 值得抓」。
//
// 这里用的是 URL 层面的通用约定（路径深度、路径段含义、是否含数字 id），
// 不涉及任何具体站点的模板。它与 internal/extract 的分工是：
//
//	discover  便宜的先筛——纯字符串运算，零网络成本，宁可放过不可错杀
//	extract   贵的后验——真正抓回来，用正文密度判据确认「这确实是一篇文章」
//
// 关键在于第二道闸门**复用抽取器本身**：同一个算法既抽正文、又当页面类型
// 分类器，不需要再引入任何站点知识。这比 URL 正则可靠得多，也和「泛化」
// 的主题自洽。
package discover

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Links 抽取页面内所有同域超链接，返回去重后的绝对 URL。
//
// 用 tokenizer 而非建 DOM 树：这里只要 <a href>，建树是纯浪费。
// 列表页动辄上千个链接，这一步的成本直接决定发现阶段的吞吐。
func Links(pageURL, htmlStr string) []string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	host := strings.ToLower(base.Host)

	seen := make(map[string]struct{}, 512)
	out := make([]string, 0, 512)

	z := html.NewTokenizer(strings.NewReader(htmlStr))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if string(name) != "a" || !hasAttr {
			continue
		}
		href := ""
		for {
			k, v, more := z.TagAttr()
			if string(k) == "href" {
				href = strings.TrimSpace(string(v))
			}
			if !more {
				break
			}
		}
		if href == "" || strings.HasPrefix(href, "#") {
			continue
		}
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		abs := base.ResolveReference(ref)
		if abs.Scheme != "http" && abs.Scheme != "https" {
			continue
		}
		if strings.ToLower(abs.Host) != host {
			continue // 只走站内，跨域交给种子表去覆盖
		}
		abs.Fragment = ""
		s := abs.String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// functionalSegment 是各站通用、且几乎必然不是文章正文的路径段。
// 这些是 URL 的通用约定，不是任何站点的模板名。
var functionalSegment = map[string]bool{
	"login": true, "logout": true, "register": true, "signup": true, "signin": true,
	"search": true, "tag": true, "tags": true, "category": true, "categories": true,
	"about": true, "contact": true, "help": true, "faq": true, "rss": true, "feed": true,
	"sitemap": true, "page": true, "index": true, "user": true, "account": true,
	"setting": true, "settings": true, "privacy": true, "terms": true, "ad": true,
	"ads": true, "comment": true, "comments": true, "print": true, "share": true,
	"api": true, "static": true, "assets": true, "css": true, "js": true,
	"img": true, "images": true, "upload": true, "download": true,
	"top": true, "hot": true, "rank": true, "all": true, "more": true, "list": true,
}

// IsFunctional 判断 URL 是否为功能页（登录、搜索、关于、标签、分页…）。
//
// seed 包在目录站里挑分类页时要排除它们，这里复用同一张表，
// 避免同一套判断在两个包里各写一份、日后再各自漂移。
func IsFunctional(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	for _, s := range strings.Split(strings.ToLower(u.Path), "/") {
		if s != "" && functionalSegment[s] {
			return true
		}
	}
	return false
}

// LooksLikeArticle 是漏斗第一级：只做便宜判断，决定一个同域 URL 值不值得
// 花一次抓取预算。判据全部来自 URL 结构，**不含任何站点或体裁先验**。
func LooksLikeArticle(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)

	// 静态资源、二进制与文档：按扩展名直接排除。
	//
	// 网银、网银助手这类页面上挂的 .exe 下载链一度被当成候选正文页抓了下来
	// （实测 4 个 .exe + 1 个 .xlsx），既浪费抓取预算，又会污染语料。
	for _, ext := range []string{
		".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".bmp", ".ico",
		".css", ".js", ".json", ".xml",
		".pdf", ".zip", ".rar", ".7z", ".tar", ".gz",
		".mp4", ".mp3", ".avi", ".mov", ".flv", ".wmv", ".mkv",
		".woff", ".woff2", ".ttf", ".eot",
		".exe", ".msi", ".dmg", ".pkg", ".apk", ".ipa", ".deb", ".rpm",
		".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".csv",
		".swf", ".iso", ".bin",
	} {
		if strings.HasSuffix(p, ext) {
			return false
		}
	}

	segs := make([]string, 0, 8)
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	for _, s := range segs {
		if functionalSegment[s] {
			return false
		}
		// 分页/排序参数常被写成路径段
		if strings.HasPrefix(s, "page") || strings.HasPrefix(s, "p_") {
			return false
		}
	}

	// 形态一：`/a/12345.html`、`/news/2024/01/15/xxx` 这类带层级的路径。
	// 要求深度 ≥ 2，因为根目录下的单段页多半是栏目首页。
	if len(segs) >= 2 {
		last := segs[len(segs)-1]
		last = strings.TrimSuffix(last, ".html")
		last = strings.TrimSuffix(last, ".htm")
		last = strings.TrimSuffix(last, ".shtml")
		if hasDigit(last) || len([]rune(last)) >= 8 {
			return true
		}
		for _, s := range segs {
			if len(s) == 4 && hasDigit(s) { // 日期式路径
				return true
			}
		}
	}

	// 形态二：`/x.asp?id=936222`、`/show.php?aid=123` 这类 query 带 id 的单段路径。
	// 中文站里这种形态极为常见，只按路径深度筛会整片漏掉——实测踩到过。
	// 判据是「参数值本身是数字 id」，这样 `/search?q=123` 之类仍然会被
	// 上面的功能段检查拦住。
	if hasNumericParam(u.RawQuery) {
		return true
	}
	return false
}

// hasNumericParam 判断查询串里是否有「值是一串数字」的参数，即 id 形态。
func hasNumericParam(rawQuery string) bool {
	for _, kv := range strings.Split(rawQuery, "&") {
		_, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) < 2 || len(v) > 18 {
			continue
		}
		allDigit := true
		for _, r := range v {
			if r < '0' || r > '9' {
				allDigit = false
				break
			}
		}
		if allDigit {
			return true
		}
	}
	return false
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// Candidate 是一个候选文章 URL 及其排序分。分数只用于决定「先抓谁」——
// 抓取预算有限，先花在更像正文页的 URL 上。
type Candidate struct {
	URL   string
	Score int
}

// Rank 对候选 URL 排序打分。分越高越像一篇具体内容。
func Rank(rawURL string) int {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	p := strings.Trim(u.Path, "/")
	segs := strings.Split(p, "/")
	last := segs[len(segs)-1]

	score := 0
	if hasDigit(last) {
		score += 3 // 数字 id 是「一条记录」最强的信号
	}
	if n := len([]rune(last)); n >= 6 && n <= 60 {
		score += 2 // slug 长度适中
	}
	score += len(segs) // 深一点更像内容页
	if strings.HasSuffix(last, ".html") || strings.HasSuffix(last, ".htm") || strings.HasSuffix(last, ".shtml") {
		score++
	}
	if u.RawQuery != "" {
		score++ // `?id=123` 这类同样指向具体内容
	}
	if strings.Contains(p, "/20") {
		score++ // 日期式路径
	}
	return score
}

// PickArticles 从同域链接里挑出候选文章 URL，按分数降序、最多 max 条。
func PickArticles(links []string, max int) []Candidate {
	cands := make([]Candidate, 0, len(links))
	for _, l := range links {
		if !LooksLikeArticle(l) {
			continue
		}
		cands = append(cands, Candidate{URL: l, Score: Rank(l)})
	}
	// 按分数降序；分数相同保持原顺序（文档序往往就是站点推荐序）
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0 && cands[j].Score > cands[j-1].Score; j-- {
			cands[j], cands[j-1] = cands[j-1], cands[j]
		}
	}
	if len(cands) > max {
		cands = cands[:max]
	}
	return cands
}
