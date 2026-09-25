package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"web-extract/internal/extract"
	"web-extract/internal/fetch"
	"web-extract/internal/frontier"
	"web-extract/internal/store"
)

// articlePage 是一篇结构清晰的短文：容器是 div、链接密度低、够长，
// 保证会被当成正文页收下（否则这条测试会因为「没有可落盘的页面」而失去意义）。
func articlePage(title string) string {
	return `<!DOCTYPE html><html><head><title>` + title + ` - 某某新闻网</title></head><body>
<nav><a href="/n/1">首页</a><a href="/n/2">要闻</a></nav>
<div id="main"><h1>` + title + `</h1>
<p>第一个问题是资金。轨道交通的造价极高，一公里地下线路的投入通常在数亿元量级，单靠财政难以支撑。</p>
<p>第二个问题是客流预测。预测偏高会让线路长期亏损，偏低又会在建成后立刻饱和，两个方向的失误代价都不小。</p>
<p>第三个问题是沿线开发。把站点周边的土地收益纳入项目平衡，是目前多数城市正在尝试的做法。</p>
</div></body></html>`
}

// TestShutdownFlushesWhatWasAlreadyCrawled 验证停机时「已经抓到的页面会落盘」。
//
// 注意它抓不到修复前那个 bug：取消之后 handle 会在 doGet 处提前返回，新 URL
// 不再被发现，队列于是很快自然排空，worker 走的是正常退出路径。它守的是结果
// （落盘数对得上），不是机制——机制由下面那条长间隔的测试守。
func TestShutdownFlushesWhatWasAlreadyCrawled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		// 每页都留一条指向下一页的链接，队列因此永远有活——这正是
		// 「停机时队列非空」的形状，也是修复前卡死的成因。
		fmt.Fprintf(w, `<a href="%s">下一页</a>`, r.URL.Path+"x")
		w.Write([]byte(articlePage("城市轨道交通建设的三个关键问题")))
	}))
	defer srv.Close()

	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	host := srv.Listener.Addr().String()
	c := &crawler{
		cfg:           config{workers: 4, delay: 10 * time.Millisecond, budget: 100, perHost: 100, minContent: 80},
		client:        fetch.New("test", 5*time.Second, 1<<20),
		fr:            frontier.New(10*time.Millisecond, 100, 4),
		st:            st,
		ext:           extract.New(extract.Options{}),
		learner:       extract.NewChromeLearner(),
		robots:        make(map[string]*fetch.Robots),
		sites:         make(map[string]*siteState),
		start:         time.Now(),
		delayReported: make(map[string]bool),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c.stopOnCancel(ctx)
	c.add([]string{"http://" + host + "/n/0"})

	var wg sync.WaitGroup
	for i := 0; i < c.cfg.workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.work(ctx) }()
	}

	// 等它先抓到一些东西，再模拟 SIGTERM。等的是抓取计数而不是落盘计数——
	// 页面是先攒在内存里、等这个域排空才落盘的，而下面那条测试要构造的恰恰是
	// 「这个域一直不排空」。只等固定时长也不行：机器一慢就会在还没抓到任何页面时
	// 停机，那样测试会变成「什么都没抓到，于是什么都没丢」而恒绿。
	waitCrawled(t, c)

	cancel()

	// 停机必须在有限时间内完成。这里给足余量：要测的是「会不会永久挂住」，
	// 不是调度有多快；修复前是永不返回，不是慢。
	returned := make(chan struct{})
	go func() { wg.Wait(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(15 * time.Second):
		t.Fatal("取消之后 worker 没有退出：收尾代码永远跑不到，内存里的页面会全丢")
	}

	c.flushRemaining()
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	if got := countLines(t, st, dir); got == 0 {
		t.Error("停机后落盘 0 行——已经抓到的页面全没了，这正是要修的事故")
	}
	if got, want := countLines(t, st, dir), c.st.Saved(); got != want {
		t.Errorf("落盘 %d 行，store 里记着 %d 页——收尾没把内存里的页面全部交出去", got, want)
	}
}

// TestShutdownEscapesAFarFutureTimeslot 才是那个「SIGTERM 后挂住不退」事故的回归测试。
//
// 要复现它，光「队列非空」还不够：取消之后每条 URL 都会被立刻处理掉
// （handle 在 doGet 处看到 ctx 已取消就返回），队列很快自己就空了，
// 进程照样能退。真正卡住的是**调度器正睡在一个远期的时间片上**——
// 这时 queued 与 inflight 都可能很小甚至归零，worker 全部安静地等在
// `range fr.Tasks()` 上，通道却要到队列排空才关，而队列要等到那个时间片
// 到期才动。于是一挂就是几十分钟。
//
// 这正是生产上真实发生过的形状：某站声明了小时级的 Crawl-delay，
// 调度协程睡在它的 nextAt 上，收到 SIGTERM 后进程二十多分钟一页没落盘。
//
// 30 秒是刻意选的：它远大于本测试 5 秒的等待上限，又远小于一小时，
// 这样即使断言失败，测试也只会慢 30 秒而不是慢一小时。
func TestShutdownEscapesAFarFutureTimeslot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nCrawl-delay: 30\n"))
			return
		}
		// 留一条同域链接，这样请求完之后该域的队列里还有活，
		// 调度器会把它排到 30 秒之后——而不是干脆排空。
		fmt.Fprintf(w, `<a href="%s">下一页</a>`, r.URL.Path+"x")
		w.Write([]byte(articlePage("城市轨道交通建设的三个关键问题")))
	}))
	defer srv.Close()

	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	host := srv.Listener.Addr().String()
	c := &crawler{
		cfg:           config{workers: 4, delay: 10 * time.Millisecond, budget: 100, perHost: 100, minContent: 80},
		client:        fetch.New("test", 5*time.Second, 1<<20),
		fr:            frontier.New(10*time.Millisecond, 100, 4),
		st:            st,
		ext:           extract.New(extract.Options{}),
		learner:       extract.NewChromeLearner(),
		robots:        make(map[string]*fetch.Robots),
		sites:         make(map[string]*siteState),
		start:         time.Now(),
		delayReported: make(map[string]bool),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.stopOnCancel(ctx)
	c.add([]string{"http://" + host + "/n/0"})

	var wg sync.WaitGroup
	for i := 0; i < c.cfg.workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.work(ctx) }()
	}

	// 等首页真的抓到——此时该域才刚被排到 30 秒之后，
	// 也就是「调度器睡在远期时间片上」这个状态刚刚成立。
	waitCrawled(t, c)
	if w := c.fr.NextWait(); w < 10*time.Second {
		t.Fatalf("前提不成立：NextWait = %s，期望调度器正睡在远期时间片上", w)
	}

	cancel()

	returned := make(chan struct{})
	go func() { wg.Wait(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("调度器睡在远期时间片上，取消之后 worker 退不出来：" +
			"收尾代码永远跑不到，内存里的页面全丢，只能 kill -9")
	}

	c.flushRemaining()
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := countLines(t, st, dir); got == 0 {
		t.Error("停机后落盘 0 行——已经抓到的页面全没了，这正是要修的事故")
	}
}

// waitCrawled 等到至少处理完一页为止。用 stats.pages 而不是 store 的计数：
// 页面先攒在内存里、等域排空才落盘，而停机测试构造的正是「域一直不排空」。
func waitCrawled(t *testing.T, c *crawler) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&c.stats.pages) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("10 秒内一页都没抓到，测试前提不成立")
}
