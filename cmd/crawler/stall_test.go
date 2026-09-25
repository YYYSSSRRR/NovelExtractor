package main

import (
	"context"
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

func newStallCrawler(t *testing.T, srv *httptest.Server, maxHostFailures int) (*crawler, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	host := srv.Listener.Addr().String()
	c := &crawler{
		cfg: config{
			workers: 4, delay: 5 * time.Millisecond, budget: 100, perHost: 100,
			minContent: 80, maxHostFailures: maxHostFailures, timeout: 300 * time.Millisecond,
		},
		client:        fetch.New("test", 300*time.Millisecond, 1<<20),
		fr:            frontier.New(5*time.Millisecond, 100, 4),
		st:            st,
		ext:           extract.New(extract.Options{}),
		learner:       extract.NewChromeLearner(),
		robots:        make(map[string]*fetch.Robots),
		sites:         make(map[string]*siteState),
		start:         time.Now(),
		delayReported: make(map[string]bool),
	}
	_ = host
	return c, st, dir
}

// TestHangingHostDoesNotStallTheCrawl 是生产事故的回归测试。
//
// 现场：一轮 1067 个域的爬取跑完 15584 页之后，日志里停在 queue=1 inflight=0，
// 连续 22 分钟一行进度都不动，直到进程被杀。那 22 分钟里进度行从未打印
// 「下次请求还要等」（NextWait 只在 ≥30s 时打印），说明**堆是空的**——
// 有 URL 躺在某个域的队列里，而那个域不在堆中，调度器永远不会再发它。
//
// 日志里有 29 条 www.xcar.com.cn 的超时失败，却**一条**「连续失败 N 次，
// 放弃该域剩余 M 条」都没有，而 -max-host-failures 默认是 2。也就是说
// noteHostFailure 的放弃分支从未生效过。这条测试就守这件事：一个只会超时的域，
// 必须在有限次失败之后被放弃，整轮爬取必须能自己结束。
func TestHangingHostDoesNotStallTheCrawl(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		<-release // 永远不回响应，逼客户端超时
	}))
	defer srv.Close()
	defer close(release)

	c, st, _ := newStallCrawler(t, srv, 2)
	defer st.Close()
	defer c.fr.Close()

	host := srv.Listener.Addr().String()
	for i := 0; i < 6; i++ {
		c.add([]string{"http://" + host + "/n/" + string(rune('0'+i))})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.stopOnCancel(ctx)

	var wg sync.WaitGroup
	for i := 0; i < c.cfg.workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.work(ctx) }()
	}

	returned := make(chan struct{})
	go func() { wg.Wait(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(20 * time.Second):
		q, inf, hosts := c.fr.Stats()
		t.Fatalf("只会超时的域把整轮爬取卡死了：queued=%d inflight=%d hosts=%d "+
			"hostAbandoned=%d failed=%d",
			q, inf, hosts, c.stats.hostAbandoned, c.stats.failed)
	}

	if got := c.stats.hostAbandoned; got == 0 {
		t.Errorf("一个只会超时的域始终没被放弃（hostAbandoned=0）："+
			"queue=%d，失败 %d 次", func() int { q, _, _ := c.fr.Stats(); return q }(), c.stats.failed)
	}
	if q, inf, _ := c.fr.Stats(); q != 0 || inf != 0 {
		t.Errorf("爬取结束了但队列没排空：queued=%d inflight=%d", q, inf)
	}
}
