package seed

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	ok := []struct{ in, want string }{
		{"https://www.gov.cn/", "www.gov.cn"},
		{"http://a.com:8080/x", "a.com"}, // 端口要抹掉，否则同站占两个名额
		{"A.CoM/x", "a.com"},
		{"https://m.eqixs.com/eqixs.asp?id=1", "m.eqixs.com"},
		{"  example.com  ", "example.com"},
	}
	for _, c := range ok {
		got, valid := Normalize(c.in)
		if !valid || got != c.want {
			t.Errorf("Normalize(%q) = (%q, %v)，期望 (%q, true)", c.in, got, valid, c.want)
		}
	}

	bad := []string{
		"", "   ", "# 注释", "ftp://a.com/x", "javascript:void(0)",
		"localhost", "https://zhihu.com/people/x", // 黑名单
	}
	for _, c := range bad {
		if got, valid := Normalize(c); valid {
			t.Errorf("Normalize(%q) = (%q, true)，期望被拒绝", c, got)
		}
	}
}

func TestBlockedSubdomain(t *testing.T) {
	for _, h := range []string{"zhihu.com", "www.zhihu.com", "m.weibo.com", "mp.weixin.qq.com"} {
		if !Blocked(h) {
			t.Errorf("%s 应被黑名单拦住", h)
		}
	}
	// 后缀匹配不能误伤：notzhihu.com 的结尾是 "zhihu.com" 但不是它的子域
	for _, h := range []string{"notzhihu.com", "zhihu.com.cn", "gov.cn"} {
		if Blocked(h) {
			t.Errorf("%s 不应被黑名单拦住", h)
		}
	}
}

func TestFromJSONL(t *testing.T) {
	const data = `
{"url":"https://m.eqixs.com/eqixs.asp?id=1","title":"a","html":"x"}
{"url":"https://m.eqixs.com/eqixs.asp?id=2","title":"b","html":"y"}
{"url":"http://www.mhuaxs.com/x","title":"c","html":"z"}
{"url":"https://www.zhihu.com/question/1","title":"d","html":"w"}
not json at all
{"url":"ftp://bad.example/x"}
`
	got, err := FromJSONL(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"m.eqixs.com", "www.mhuaxs.com"}
	if len(got) != len(want) {
		t.Fatalf("得到 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("得到 %v，期望 %v", got, want)
		}
	}
}

// TestHarvestHostsOnlyExternal 验证目录页扩散只捞站外域名——
// 站内链接在这里是噪声，而且会让「扩散一层」变成在同站里打转。
func TestHarvestHostsOnlyExternal(t *testing.T) {
	const page = `<html><body>
	<a href="/local/1.html">站内</a>
	<a href="https://other.com/a">站外</a>
	<a href="http://third.cn/">站外二</a>
	<a href="https://www.zhihu.com/x">黑名单</a>
	<a href="https://other.com/b">重复</a>
	<a href="javascript:void(0)">无效</a>
	</body></html>`

	got := HarvestHosts("https://dir.com/list", page)
	if len(got) != 2 {
		t.Fatalf("得到 %v，期望 2 个站外域名", got)
	}
	seen := map[string]bool{got[0]: true, got[1]: true}
	if !seen["other.com"] || !seen["third.cn"] {
		t.Errorf("得到 %v，期望含 other.com 与 third.cn", got)
	}
}

// TestPickSections 验证栏目页挑选：排除功能页、排除首页、浅路径优先。
// 目录站的产出几乎全来自栏目页，挑错了这一批种子就白跑了。
func TestPickSections(t *testing.T) {
	links := []string{
		"https://dir.com/",
		"https://dir.com/login",
		"https://dir.com/news/",
		"https://dir.com/tech/it/",
		"https://dir.com/about",
		"https://dir.com/novel/",
	}
	got := PickSections(links, 2)
	if len(got) != 2 {
		t.Fatalf("得到 %v，期望 2 条", got)
	}
	// /news/ 与 /novel/ 深度为 0，应排在 /tech/it/ 之前
	for _, g := range got {
		if strings.Contains(g, "login") || strings.Contains(g, "about") || g == "https://dir.com/" {
			t.Errorf("功能页或首页混进了栏目列表：%v", got)
		}
		if strings.Contains(g, "tech/it") {
			t.Errorf("深路径应排在浅路径之后：%v", got)
		}
	}
}

func TestDedupeSorted(t *testing.T) {
	got := Dedupe([]string{"b.com", "a.com", "b.com", ""})
	if len(got) != 2 || got[0] != "a.com" || got[1] != "b.com" {
		t.Errorf("得到 %v，期望 [a.com b.com]（去重且有序）", got)
	}
}
