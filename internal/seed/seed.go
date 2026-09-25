// Package seed 解决「1000 多个站从哪来」。
//
// 这一环不能靠手敲域名，但也不能放任扩散——扩得太远会漂到与语料无关的站点上。
// 所以策略是三条：多个高质量来源并联、站外只扩散一层、入队前先探可达性。
package seed

import (
	"bufio"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/html"

	"web-extract/internal/discover"
)

// blocked 是主动跳过的域名后缀，分两类：
//
//	robots 全面禁止爬取  知乎、微博、小红书
//	强反爬 / 登录墙      抖音、快手、淘宝、微信公众号
//
// 有 1000+ 候选域做冗余，单域被拒的成本可以忽略，因此不做 UA 轮换、代理池
// 这类规避动作——那是负期望，也会让爬虫的对外行为变得不诚实。
var blocked = []string{
	"zhihu.com", "weibo.com", "xiaohongshu.com", "douyin.com", "kuaishou.com",
	"taobao.com", "tmall.com", "jd.com", "pinduoduo.com", "alipay.com",
	"weixin.qq.com", "mp.weixin.qq.com", "qq.com",
	"bilibili.com", "douban.com", "tieba.baidu.com",
	"google.com", "baidu.com", "bing.com", "so.com", "sogou.com", "sm.cn",
	"facebook.com", "twitter.com", "x.com", "youtube.com", "instagram.com",
}

// Blocked 判断域名是否在黑名单内（含子域）。
func Blocked(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, b := range blocked {
		if host == b || strings.HasSuffix(host, "."+b) {
			return true
		}
	}
	return false
}

// Normalize 把一行输入规整成裸域名。接受完整 URL、带协议的主机名或裸域名，
// 非 http(s) 的一律拒绝；端口与大小写一并抹平，避免同一站点被当成两个。
func Normalize(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", false
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	// Hostname() 而非 Host：顺手剥掉端口，否则 host:8080 与 host 会各占一个名额
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || !strings.Contains(host, ".") {
		return "", false
	}
	if Blocked(host) {
		return "", false
	}
	return host, true
}

// Dedupe 去重并排序。排序是为了让同一批输入永远产出同样的种子表，
// 这样实验可复现，diff 也可读。
func Dedupe(hosts []string) []string {
	seen := make(map[string]struct{}, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// FromJSONL 从 {url, html, title} 格式的 JSONL 里提取全部域名。
//
// 这是最廉价的一批种子：基础题自带的数据集里就有几十个真实站点，
// 零网络成本，且天然覆盖「小说」这一体裁。
func FromJSONL(r io.Reader) ([]string, error) {
	var hosts []string
	sc := bufio.NewScanner(r)
	// 数据集里单行可以很大（html 字段），默认 64KB 上限不够
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var rec struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue // 单行坏数据不该带崩整批
		}
		if h, ok := Normalize(rec.URL); ok {
			hosts = append(hosts, h)
		}
	}
	return Dedupe(hosts), sc.Err()
}

// FromLines 读「一行一个域名或 URL」的清单，空行与 # 注释跳过。
// 人工兜底清单和目录站捞回来的中间结果都走这里。
func FromLines(r io.Reader) ([]string, error) {
	var hosts []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for sc.Scan() {
		if h, ok := Normalize(sc.Text()); ok {
			hosts = append(hosts, h)
		}
	}
	return Dedupe(hosts), sc.Err()
}

// PickSections 从站内链接里挑出最像「栏目页/分类页」的若干条。
//
// 目录站的价值几乎全在分类页上：首页通常只链几十个站，一个分类页却链几百个。
// 所以扩散时要在站内先走一层。这里按路径深度升序挑（栏目页通常浅），
// 并排除功能页——登录、搜索、关于这些页面上没有外链。
func PickSections(links []string, max int) []string {
	type cand struct {
		url   string
		depth int
	}
	cands := make([]cand, 0, len(links))
	seen := make(map[string]struct{}, len(links))

	for _, l := range links {
		u, err := url.Parse(l)
		if err != nil || discover.IsFunctional(l) {
			continue
		}
		p := strings.Trim(u.Path, "/")
		if p == "" {
			continue // 首页已经单独抓过
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		cands = append(cands, cand{l, strings.Count(p, "/")})
	}

	// 浅路径优先，稳定排序保证可复现
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].depth < cands[j].depth })
	if len(cands) > max {
		cands = cands[:max]
	}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.url)
	}
	return out
}

// HarvestHosts 从页面里捞出所有**站外**域名。
//
// 与 discover.Links 恰好相反：那里只走站内（找这个站自己的文章页），
// 这里只走站外——目录站的价值恰恰在于把流量导向别的站，
// 而站内链接在这里纯属噪声。
func HarvestHosts(pageURL, htmlStr string) []string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	self := strings.ToLower(base.Hostname())

	seen := make(map[string]struct{}, 256)
	out := make([]string, 0, 256)

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
		if href == "" {
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
		h := strings.ToLower(abs.Hostname())
		if h == "" || h == self || Blocked(h) {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	return out
}
