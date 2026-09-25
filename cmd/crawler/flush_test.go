package main

import (
	"bufio"
	"os"
	"path/filepath"
	"testing"

	"web-extract/internal/extract"
	"web-extract/internal/store"
)

// newTestCrawler 造一个只差网络部分的爬虫：sites 与 store 都在，flushSite
// 这条路径可以完整跑通。
func newTestCrawler(t *testing.T, noTemplates bool) (*crawler, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := &crawler{
		cfg:   config{noTemplates: noTemplates},
		st:    st,
		sites: make(map[string]*siteState),
	}
	return c, st, dir
}

// page 造一页待落盘的产物。
func page(url, content string) pageOut {
	return pageOut{
		entry: store.Entry{URL: url, MainContent: content},
		res:   extract.Result{MainContent: content, ContentChars: extract.CountContent(content)},
	}
}

// countLines 先 Flush 再数。Save 只写 bufio 缓冲，不刷盘就从文件里读，
// 数出来永远是 0——那是测试的错觉，不是产品行为。
func countLines(t *testing.T, st *store.Store, dir string) int {
	t.Helper()
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "pages.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if sc.Text() != "" {
			n++
		}
	}
	return n
}

// TestFlushSiteCanRunTwice 是一个真实丢数据事故的回归测试。
//
// 原来的 flushSite 用 `if s.flushed { return }` 做一次性闩锁，前提是
// 「一个域只会排空一次」。这个前提不成立：一个域先被 A 站发现、爬完、排空、
// 收尾，之后又被 B 站发现，于是再次入队、再抓一批、再次排空。第二批页面
// 照常 append 进 s.pages，到收尾时被闩锁挡回去——既没落盘也不报错，
// 因为 Save 根本没被调用，「落盘失败」计数自然是 0，整件事静默发生。
//
// 实测一轮 15584 页的爬取只落盘 315 行，而所有计数与日志都正常。
func TestFlushSiteCanRunTwice(t *testing.T) {
	c, st, dir := newTestCrawler(t, true)
	defer st.Close()

	const host = "a.example"
	s := c.site(host)

	// 第一轮：这个域被 A 站发现，抓了 2 页后排空
	s.pages = append(s.pages, page("http://a.example/1", "第一轮第一页的正文内容"),
		page("http://a.example/2", "第一轮第二页的正文内容"))
	c.flushSite(host)

	if got := countLines(t, st, dir); got != 2 {
		t.Fatalf("第一次收尾后落盘 %d 行，期望 2", got)
	}
	if got := st.Saved(); got != 2 {
		t.Fatalf("Saved() = %d，期望 2", got)
	}

	// 第二轮：同一个域又被 B 站发现，再抓 2 页
	s.pages = append(s.pages, page("http://a.example/3", "第二轮第一页的正文内容"),
		page("http://a.example/4", "第二轮第二页的正文内容"))
	c.flushSite(host)

	if got := countLines(t, st, dir); got != 4 {
		t.Fatalf("第二次收尾后落盘 %d 行，期望 4——"+
			"第二批页面被一次性闩锁丢掉了，这正是那个静默丢数据的 bug", got)
	}
	if got := st.Saved(); got != 4 {
		t.Fatalf("Saved() = %d，期望 4", got)
	}

	// 统计口径数的是「收过尾的域」，不是「收尾次数」
	if got := c.stats.sitesFlushed; got != 1 {
		t.Errorf("sitesFlushed = %d，期望 1", got)
	}
}

// TestFlushSiteSkipsEmpty 守住「手上没有页面时直接返回」：域在 frontier 里
// 排空，未必攒下过页面（全被去重跳过、或整页抓失败），那时不该凭空记一次收尾。
func TestFlushSiteSkipsEmpty(t *testing.T) {
	c, st, dir := newTestCrawler(t, true)
	defer st.Close()

	c.site("b.example")
	c.flushSite("b.example")

	if got := countLines(t, st, dir); got != 0 {
		t.Errorf("没有页面却落盘了 %d 行", got)
	}
	if c.stats.sitesFlushed != 0 {
		t.Errorf("sitesFlushed = %d，期望 0", c.stats.sitesFlushed)
	}
}

// TestFlushRemainingPicksUpUnflushed 守住兜底收尾：域已经收过尾，之后又攒下
// 新的一批，也必须被 flushRemaining 捡走，否则进程退出时这一批就没了。
func TestFlushRemainingPicksUpUnflushed(t *testing.T) {
	c, st, dir := newTestCrawler(t, true)
	defer st.Close()

	const host = "c.example"
	s := c.site(host)
	s.pages = append(s.pages, page("http://c.example/1", "收尾前的一页"))
	c.flushSite(host)

	s.pages = append(s.pages, page("http://c.example/2", "收尾后攒下的这一页"))
	c.flushRemaining()

	if got := countLines(t, st, dir); got != 2 {
		t.Fatalf("兜底收尾后落盘 %d 行，期望 2——"+
			"收过尾的域再攒下的页面被漏掉了", got)
	}
}

// TestTemplateLearnedOnceAndReused 钉住模板的复用语义：学到的模板存在
// siteState 上，第二批页面直接复用它，而不是等到下次收尾再学一遍
// （那批可能只有一两页，tmplMinPages 门槛下根本学不出东西）。
func TestTemplateLearnedOnceAndReused(t *testing.T) {
	c, st, dir := newTestCrawler(t, false)
	defer st.Close()

	const host = "d.example"
	s := c.site(host)

	// 三页共享一段尾巴，够 tmplMinPages 学出一个模板
	const body = "这是这一页自己的正文，讲的是完全不同的事情，用来把模板成分衬出来。"
	const tail = "\n版权所有 某某网站 京ICP备00000000号"
	s.pages = append(s.pages,
		page("http://d.example/1", body+"第一页"+tail),
		page("http://d.example/2", body+"第二页"+tail),
		page("http://d.example/3", body+"第三页"+tail))
	c.flushSite(host)

	if s.tm == nil {
		t.Fatal("三页共享尾巴，却没学出模板")
	}
	learned := s.tm

	// 第二批：这两页只有两页，单独学是学不出模板的（tmplMinPages = 3）
	s.pages = append(s.pages,
		page("http://d.example/4", body+"第四页"+tail),
		page("http://d.example/5", body+"第五页"+tail))
	c.flushSite(host)

	if s.tm != learned {
		t.Error("第二批又学了一遍模板，应当直接复用第一批学到的那个")
	}
	if got := countLines(t, st, dir); got != 5 {
		t.Fatalf("落盘 %d 行，期望 5", got)
	}
}
