package frontier

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestPerHostSerializationAndPoliteness 是调度器最核心的一条断言：
// 同一个域**永远不会**有两个任务同时在处理，且相邻两次的间隔不小于 delay。
//
// 这两件事必须一起测。只测串行会漏掉限速，只测限速会漏掉并发。
func TestPerHostSerializationAndPoliteness(t *testing.T) {
	const delay = 25 * time.Millisecond
	const perHost = 10

	f := New(delay, 100, 8)

	var urls []string
	for i := 0; i < perHost; i++ {
		urls = append(urls, fmt.Sprintf("http://a.example.com/p%d", i))
		urls = append(urls, fmt.Sprintf("http://b.example.com/p%d", i))
	}
	if n := f.Add(urls...); n != 2*perHost {
		t.Fatalf("Add 返回 %d，期望 %d", n, 2*perHost)
	}

	var mu sync.Mutex
	lastSeen := make(map[string]time.Time)
	running := make(map[string]int)
	got := make(map[string]int)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range f.Tasks() {
				mu.Lock()
				running[task.Host]++
				if running[task.Host] > 1 {
					t.Errorf("同域并发：%s 同时有 %d 个任务", task.Host, running[task.Host])
				}
				if prev, ok := lastSeen[task.Host]; ok {
					if gap := time.Since(prev); gap < delay {
						t.Errorf("%s 间隔 %s < %s，违反礼貌性", task.Host, gap, delay)
					}
				}
				lastSeen[task.Host] = time.Now()
				got[task.Host]++
				mu.Unlock()

				time.Sleep(2 * time.Millisecond) // 模拟请求耗时

				mu.Lock()
				running[task.Host]--
				mu.Unlock()
				f.Done(task.Host)
			}
		}()
	}

	// 超时保护：终止判定若写错，这里会挂住而不是静默通过
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		q, inf, hosts := f.Stats()
		t.Fatalf("超时：任务通道没有关闭。queued=%d inflight=%d hosts=%d", q, inf, hosts)
	}

	for _, h := range []string{"a.example.com", "b.example.com"} {
		if got[h] != perHost {
			t.Errorf("%s 收到 %d 个任务，期望 %d", h, got[h], perHost)
		}
	}
}

// TestGateThrottlesEveryRequest 锁住一条曾经真实违反过的不变量：
// 一个 worker 在自己的时间片内可能发多个请求（robots.txt + 页面），
// 这些请求之间的间隔同样不得小于 delay。
//
// 这个 bug 用 -trace 在真实抓取里量出来过：32 对相邻请求的间隔低于配置值，
// 全部是 robots.txt 与紧随其后的页面请求。修复方式是把「发请求」显式摆到
// Gate 前面，而不是在 robots 那条分支上打补丁——所以这里测的是 Gate 本身。
func TestGateThrottlesEveryRequest(t *testing.T) {
	const delay = 30 * time.Millisecond
	f := New(delay, 100, 4)
	f.Add("http://a.example.com/1")

	var mu sync.Mutex
	lastSent := make(map[string]time.Time)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for task := range f.Tasks() {
			// 模拟一个 worker 在一个时间片内连发三个请求
			for i := 0; i < 3; i++ {
				if err := f.Gate(context.Background(), task.Host); err != nil {
					t.Errorf("Gate: %v", err)
					return
				}
				mu.Lock()
				now := time.Now()
				if prev, ok := lastSent[task.Host]; ok {
					if gap := now.Sub(prev); gap < delay {
						t.Errorf("第 %d 个请求与前一个相隔 %s < %s，限速被绕过", i+1, gap, delay)
					}
				}
				lastSent[task.Host] = now
				mu.Unlock()
			}
			f.Done(task.Host)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("超时：Gate 可能永久阻塞")
	}
	if n := len(lastSent); n == 0 {
		t.Fatal("没有发出任何请求，用例本身失效")
	}
}

// TestGateRespectsContextCancel 保证 ctx 取消时 Gate 会立刻返回而不是睡满间隔——
// 否则 Ctrl-C 之后进程还要被每个在途请求拖住一个 delay。
func TestGateRespectsContextCancel(t *testing.T) {
	f := New(time.Hour, 10, 2)
	f.Add("http://a.example.com/1")

	// 先占用掉第一个时间片，使闸门关上
	if err := f.Gate(context.Background(), "a.example.com"); err != nil {
		t.Fatalf("首次 Gate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := f.Gate(ctx, "a.example.com"); err == nil {
		t.Fatal("闸门关闭时 Gate 应返回 ctx 错误")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Gate 等了 %s 才返回，没有响应取消", el)
	}
}

// TestLateDiscoveryNotLost 验证「先 Add 再 Done」这个顺序约定。
//
// worker 发现的新 URL 必须在销账之前归还队列，否则可能出现：最后一批发现
// 刚 Add 完、queued 归零的瞬间被判定爬完，新 URL 永远留在队列里。
func TestLateDiscoveryNotLost(t *testing.T) {
	f := New(0, 100, 4)
	f.Add("http://a.example.com/1")

	var mu sync.Mutex
	seen := []string{}

	for task := range f.Tasks() {
		mu.Lock()
		seen = append(seen, task.URL)
		n := len(seen)
		mu.Unlock()

		if n == 1 {
			// 模拟：第一页里发现了第二页
			f.Add("http://b.example.com/2")
		}
		f.Done(task.Host)
	}

	if len(seen) != 2 {
		t.Fatalf("只处理了 %d 个任务 %v，期望 2 个（后发现的 URL 被丢了）", len(seen), seen)
	}
}

func TestDedup(t *testing.T) {
	f := New(0, 100, 2)
	n := f.Add(
		"http://a.example.com/x",
		"http://a.example.com/x",     // 完全重复
		"http://a.example.com/x#top", // 只差片段，规范化后同一资源
		"HTTP://A.example.com/x",     // 大小写
		"http://a.example.com/y?id=1",
		"http://a.example.com/y?id=2", // 查询串不同 = 不同资源，必须保留
	)
	if n != 3 {
		t.Errorf("Add 去重后 %d 条，期望 3 条", n)
	}
}

// TestPerHostCap 验证单域队列上限：热门域不能吃掉全部内存。
func TestPerHostCap(t *testing.T) {
	f := New(0, 3, 2)
	n := f.Add(
		"http://a.example.com/1", "http://a.example.com/2",
		"http://a.example.com/3", "http://a.example.com/4",
		"http://a.example.com/5",
	)
	if n != 3 {
		t.Errorf("单域上限 3，实际入队 %d", n)
	}
}

// TestSetHostDelayOnlySlows 验证 robots.txt 的 Crawl-delay 只能让我们更慢。
// 站点声明一个比全局间隔更小的值时应被忽略，否则「礼貌」就成了空话。
func TestSetHostDelayOnlySlows(t *testing.T) {
	const global = 60 * time.Millisecond
	f := New(global, 10, 2)
	f.Add("http://a.example.com/1", "http://a.example.com/2")
	// 比全局间隔小：应当被忽略
	f.SetHostDelay("a.example.com", 5*time.Millisecond)

	var gaps []time.Duration
	prev := time.Now()
	for task := range f.Tasks() {
		gaps = append(gaps, time.Since(prev))
		prev = time.Now()
		f.Done(task.Host)
	}

	if len(gaps) != 2 {
		t.Fatalf("收到 %d 个任务，期望 2 个", len(gaps))
	}
	if gaps[1] < global {
		t.Errorf("第二个任务间隔 %s，小于保底间隔 %s——更小的 Crawl-delay 不应生效", gaps[1], global)
	}
}

// TestSetHostDelaySlowsFurther 验证站点声明的更大间隔确实生效。
func TestSetHostDelaySlowsFurther(t *testing.T) {
	const global = 20 * time.Millisecond
	const slow = 120 * time.Millisecond

	f := New(global, 10, 2)
	f.Add("http://a.example.com/1", "http://a.example.com/2")
	f.SetHostDelay("a.example.com", slow)

	var gaps []time.Duration
	prev := time.Now()
	for task := range f.Tasks() {
		gaps = append(gaps, time.Since(prev))
		prev = time.Now()
		f.Done(task.Host)
	}
	if len(gaps) != 2 {
		t.Fatalf("收到 %d 个任务，期望 2 个", len(gaps))
	}
	if gaps[1] < slow {
		t.Errorf("第二个任务间隔 %s，小于站点声明的 %s", gaps[1], slow)
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		ok       bool
	}{
		{"http://a.com/x", "a.com", true},
		{"HTTPS://A.CoM/x", "a.com", true},
		{"ftp://a.com/x", "", false},
		{"javascript:void(0)", "", false},
		{"", "", false},
		{"http://a.com/x#frag", "a.com", true},
	}
	for _, c := range cases {
		_, host, ok := normalize(c.in)
		if ok != c.ok || host != c.wantHost {
			t.Errorf("normalize(%q) = (%q, %v)，期望 (%q, %v)", c.in, host, ok, c.wantHost, c.ok)
		}
	}
}
