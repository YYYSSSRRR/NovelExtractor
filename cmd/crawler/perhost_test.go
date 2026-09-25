package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"web-extract/internal/extract"
	"web-extract/internal/fetch"
	"web-extract/internal/frontier"
	"web-extract/internal/store"
)

// newLinkFarm 起一个「每页都链出一堆同域候选」的站，让漏斗有活可干。
//
// 链接指向 /p/i 而不是自己所在的路径，是为了不跟「种子页 URL 已去重」这件事
// 纠缠：种子放在 /seed，候选固定 perPage 条，于是「这一轮本该抓多少页」是一个
// 可以精确断言的值，测试不必靠上下界去猜。
func newLinkFarm(t *testing.T, perPage int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		for i := 0; i < perPage; i++ {
			fmt.Fprintf(w, `<a href="/p/%d">第%d页</a>`, i, i)
		}
		w.Write([]byte(articlePage("城市轨道交通建设的三个关键问题")))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newNetCrawler 造一个除了种子之外什么都有了的爬虫：frontier、store、
// 抽取器、robots 缓存都在，handle 这条路径可以完整跑通。
func newNetCrawler(t *testing.T, cfg config) (*crawler, *store.Store, string) {
	t.Helper()
	if cfg.delay <= 0 {
		cfg.delay = time.Millisecond
	}
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := &crawler{
		cfg:           cfg,
		client:        fetch.New("test", 5*time.Second, 1<<20),
		fr:            frontier.New(cfg.delay, 100, 8),
		st:            st,
		ext:           extract.New(extract.Options{}),
		learner:       extract.NewChromeLearner(),
		robots:        make(map[string]*fetch.Robots),
		sites:         make(map[string]*siteState),
		start:         time.Now(),
		delayReported: make(map[string]bool),
	}
	t.Cleanup(func() {
		c.fr.Close()
		st.Close()
	})
	return c, st, dir
}

// crawlToCompletion 跑一轮爬取，直到 worker 自己全部退出。
//
// 不设「跑够多少页就取消」的兜底：这一轮必须由 frontier 判定排空来结束，
// 那正是这些测试要守的性质。超时就报出当时的队列状态，而不是悄悄收工。
func crawlToCompletion(t *testing.T, c *crawler, seeds []string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.stopOnCancel(ctx)
	c.add(seeds)

	var wg sync.WaitGroup
	for i := 0; i < c.cfg.workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.work(ctx) }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		queued, inflight, _ := c.fr.Stats()
		t.Fatalf("爬取没有自己结束：queued=%d inflight=%d 已抓 %d 页",
			queued, inflight, c.stats.pages)
	}
}

// TestPerHostTargetStopsTheHostAndKeepsWhatItGot 守住 -per-host 真的起作用。
//
// 这个参数此前只是被解析、被打印在启动横幅里，然后就没有下文了：handle 数了
// s.articles，却从来没有拿它跟任何东西比过。于是每个域一律把 budget 条候选
// 全部抓完，哪怕早就拿到了目标数量的正文页——而每条都要占该域一个时间片
// （默认 1.5 秒的礼貌间隔）。一轮爬取的耗时正是「域数 × 每域请求数 × 间隔
// ÷ worker 数」，这是其中最容易被省掉的一段。
//
// 关键在于「丢弃的只是队列」：已经抓到的页面必须照常留在内存里等着收尾落盘，
// 否则省下的时间会以丢数据为代价——那正是前面几个 bug 的形状。
//
// 这条走的是真实的 handle 路径（真发 HTTP、真跑漏斗、真落盘），不是把
// stopHostIfSatisfied 的判断条件在测试里再抄一遍——那样证明的只是抄写没写错。
func TestPerHostTargetStopsTheHostAndKeepsWhatItGot(t *testing.T) {
	const perPage = 12
	srv := newLinkFarm(t, perPage)
	host := srv.Listener.Addr().String()

	c, st, dir := newNetCrawler(t, config{
		workers: 2, budget: 50, perHost: 2,
		minContent: 80, maxHostFailures: 2,
	})
	crawlToCompletion(t, c, []string{"http://" + host + "/seed"})

	if got := c.stats.targetMet; got != 1 {
		t.Errorf("收手次数 = %d，期望 1（够 %d 篇正文后本域应当收手一次）", got, c.cfg.perHost)
	}
	if got := c.stats.dropped; got == 0 {
		t.Error("一条候选都没丢——-per-host 没有真的生效，" +
			"这个域仍会把 budget 条候选全部抓完")
	}
	// 每域串行 + 第 2 篇就收手，所以抓的页数是个确定值：种子页 1 篇，
	// 够数的那 1 篇。多出来的每一页都意味着 -per-host 迟了一步。
	if got := c.stats.pages; got != 2 {
		t.Errorf("已抓 %d 页，期望 2（%d 条候选本该在第 2 篇正文之后就丢掉）", got, perPage)
	}

	// 省下的时间不能以丢数据为代价：那两页必须都落盘。
	if got := countLines(t, st, dir); got != 2 {
		t.Errorf("落盘 %d 行，期望 2——收手时把已经抓到的页面一起丢了", got)
	}
	if queued, inflight, _ := c.fr.Stats(); queued != 0 || inflight != 0 {
		t.Errorf("爬取结束了但队列没排空：queued=%d inflight=%d", queued, inflight)
	}
}

// TestPerHostZeroMeansNoTarget 盯住收手这件事可以被关掉。
//
// 0 表示「不限」，是与「设一个很小的目标」不同的语义：用户想尽可能多收时
// 不该被默认值拦下。这类「0 即关闭」的约定在本项目里到处都是
// （-max-pages、-retries、-max-host-failures），值得钉一条。
//
// 与上一条的对照组意义在于：同样的站、同样的种子，只把 perHost 从 2 改成 0，
// 抓的页数就从 2 变成「预算抓满」。两条一起才说明收手是被 perHost 控制的，
// 而不是被别的什么碰巧拦下的。
func TestPerHostZeroMeansNoTarget(t *testing.T) {
	const perPage = 5
	const budget = 6
	srv := newLinkFarm(t, perPage)
	host := srv.Listener.Addr().String()

	c, _, _ := newNetCrawler(t, config{
		workers: 2, budget: budget, perHost: 0,
		minContent: 80, maxHostFailures: 2,
	})
	crawlToCompletion(t, c, []string{"http://" + host + "/seed"})

	if got := c.stats.targetMet; got != 0 {
		t.Errorf("perHost = 0 却收手了 %d 次", got)
	}
	if got := c.stats.dropped; got != 0 {
		t.Errorf("perHost = 0 却丢了 %d 条候选", got)
	}
	// 预算抓满：种子页 1 页 + 漏斗给出的 perPage 条候选。
	if got := c.stats.pages; got != budget {
		t.Errorf("已抓 %d 页，期望 %d（perHost = 0 时应当把预算抓满）", got, budget)
	}
}
