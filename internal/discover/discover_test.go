package discover

import "testing"

func TestLooksLikeArticle(t *testing.T) {
	yes := []string{
		"http://a.com/news/2024/01/15/abc.html",
		"http://a.com/a/12345.html",
		"http://a.com/novel/1234/5678.shtml",
		"http://a.com/post/hello-world-today",
		"http://a.com/x.asp?id=936222",
	}
	for _, u := range yes {
		if !LooksLikeArticle(u) {
			t.Errorf("LooksLikeArticle(%q) = false，期望 true", u)
		}
	}

	no := []string{
		"http://a.com/",
		"http://a.com/news",             // 深度不足
		"http://a.com/login",            // 功能页
		"http://a.com/search?q=x",       // 功能页
		"http://a.com/tag/123",          // 功能页
		"http://a.com/static/img/1.png", // 静态资源
		"http://a.com/css/main.css",     // 静态资源
		"http://a.com/news/page2",       // 分页
		"http://a.com/list/12345",       // 功能页
	}
	for _, u := range no {
		if LooksLikeArticle(u) {
			t.Errorf("LooksLikeArticle(%q) = true，期望 false", u)
		}
	}
}

func TestLinksSameHostOnly(t *testing.T) {
	const page = `<html><body>
		<a href="/a/1.html">1</a>
		<a href="http://a.com/a/2.html">2</a>
		<a href="http://other.com/a/3.html">3</a>
		<a href="#frag">4</a>
		<a href="javascript:void(0)">5</a>
		<a href="/a/1.html">dup</a>
	</body></html>`

	got := Links("http://a.com/list", page)
	if len(got) != 2 {
		t.Fatalf("得到 %d 条链接 %v，期望 2 条（同域、去重、去片段、去 javascript）", len(got), got)
	}
	if got[0] != "http://a.com/a/1.html" || got[1] != "http://a.com/a/2.html" {
		t.Errorf("链接内容不对: %v", got)
	}
}

// TestPickArticlesPrefersNumericID 验证排序把「更像具体一篇」的排在前面。
// 抓取预算是硬的，顺序直接决定这轮爬取的产出率。
func TestPickArticlesPrefersNumericID(t *testing.T) {
	links := []string{
		"http://a.com/news/2024/01/15/abcdefgh.html",
		"http://a.com/a/12345.html",
		"http://a.com/login",
	}
	got := PickArticles(links, 10)
	if len(got) != 2 {
		t.Fatalf("候选 %d 条，期望 2 条（login 应被滤掉）", len(got))
	}
	if got[0].Score < got[1].Score {
		t.Errorf("未按分数降序：%v", got)
	}
}

func TestPickArticlesCap(t *testing.T) {
	links := []string{
		"http://a.com/a/1.html", "http://a.com/a/2.html",
		"http://a.com/a/3.html", "http://a.com/a/4.html",
	}
	if got := PickArticles(links, 2); len(got) != 2 {
		t.Errorf("上限 2，实际 %d 条", len(got))
	}
}
