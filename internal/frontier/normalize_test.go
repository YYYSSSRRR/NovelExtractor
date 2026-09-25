package frontier

import (
	"testing"
	"time"
)

// TestNormalizeStripsDefaultPort 盯的是 host 同时兼作限速键这件事：
// `example.com` 与 `example.com:80` 若被当成两个域，就会各自持有一个时间片
// 并发请求同一台服务器，每域间隔的承诺直接作废。
func TestNormalizeStripsDefaultPort(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantURL  string
	}{
		{"http://example.com:80/a", "example.com", "http://example.com/a"},
		{"https://example.com:443/a", "example.com", "https://example.com/a"},
		{"http://example.com/a", "example.com", "http://example.com/a"},
		{"http://Example.COM/A", "example.com", "http://example.com/A"},
		// 末尾的点是 FQDN 的合法写法，同一个站不该占两个名额
		{"http://example.com./a", "example.com", "http://example.com/a"},
		// 非默认端口必须原样保留，否则会把不同服务折叠成一个
		{"http://example.com:8080/a", "example.com:8080", "http://example.com:8080/a"},
		{"https://example.com:80/a", "example.com:80", "https://example.com:80/a"},
		{"http://example.com:443/a", "example.com:443", "http://example.com:443/a"},
		// 查询串不能删：文章 id 常常就在 query 里
		{"http://example.com/x.asp?id=936222", "example.com", "http://example.com/x.asp?id=936222"},
	}
	for _, c := range cases {
		gotURL, gotHost, ok := normalize(c.in)
		if !ok {
			t.Errorf("normalize(%q) 被拒绝，应当接受", c.in)
			continue
		}
		if gotHost != c.wantHost {
			t.Errorf("normalize(%q) host = %q, 期望 %q", c.in, gotHost, c.wantHost)
		}
		if gotURL != c.wantURL {
			t.Errorf("normalize(%q) url = %q, 期望 %q", c.in, gotURL, c.wantURL)
		}
	}
}

func TestNormalizeRejectsNonHTTP(t *testing.T) {
	for _, in := range []string{"", "   ", "ftp://example.com/a", "javascript:void(0)", "mailto:a@b.c", "/relative/path"} {
		if _, _, ok := normalize(in); ok {
			t.Errorf("normalize(%q) 应当被拒绝", in)
		}
	}
}

// TestDefaultPortDoesNotSplitHost 是上面那条的端到端形态：同一个站的四种
// 写法必须合并成**一个** host 队列，而不是四个。
func TestDefaultPortDoesNotSplitHost(t *testing.T) {
	f := New(time.Hour, 100, 8)
	defer f.Close()

	n := f.Add(
		"http://example.com:80/a",
		"http://example.com/a", // 与上一条是同一个资源，应当去重
		"https://example.com:443/b",
		"https://example.com/b", // 同上
	)
	if n != 2 {
		t.Fatalf("Add 新增 %d 条，期望 2 条（四种写法归并成两个 URL）", n)
	}
	if _, _, hosts := f.Stats(); hosts != 1 {
		t.Fatalf("产生了 %d 个 host 队列，期望 1 个——"+
			"多个队列意味着同一台服务器会被并发请求，限速失效", hosts)
	}
}

// TestCloseIsIdempotent 守住一个真实的 panic 路径：Close 里是无条件的
// close(f.ready)，若调度器已经自行判定爬完并关过通道，再 Close 就会
// 二次 close 崩溃。
func TestCloseIsIdempotent(t *testing.T) {
	f := New(time.Millisecond, 10, 4)
	f.Close()
	f.Close() // 不该 panic
}

// TestNextWaitReportsTheScheduledSleep 守住「停滞要看得见」。
//
// 调度器可能正安静地睡在堆顶那个域的 nextAt 上：queued 与 inflight 都可能
// 很小甚至归零，从外面看和卡死没有区别。实测一个声明了 10 分钟 Crawl-delay
// 的站让整轮爬取停了一个多小时，而进度行照常打印、不含任何相关线索。
func TestNextWaitReportsTheScheduledSleep(t *testing.T) {
	f := New(time.Hour, 10, 4) // 全局间隔一小时，入队后必然要等
	defer f.Close()

	if w := f.NextWait(); w != 0 {
		t.Errorf("堆还是空的，NextWait = %s，期望 0", w)
	}

	// 两条 URL：第一条立刻可抓，抓完之后域里还剩一条，这时才会排进堆里等自己的
	// 时间片。只放一条的话域一抓就空了，堆里什么都没有，也就没有「还要等多久」。
	f.Add("http://slow.example/a", "http://slow.example/b")
	// 新域是立刻可抓的：间隔只约束「同一个域的两次请求之间」，不约束第一次。
	if w := f.NextWait(); w != 0 {
		t.Errorf("新域应当立刻可抓，NextWait = %s，期望 0", w)
	}

	select {
	case task := <-f.Tasks():
		f.Done(task.Host)
	case <-time.After(2 * time.Second):
		t.Fatal("任务没有发出来")
	}

	if w := f.NextWait(); w <= 30*time.Minute {
		t.Errorf("NextWait = %s，期望接近一小时的等待被报出来", w)
	}
}

// TestAbandonRemovesHostFromSchedule 守住「放弃一个域」的两个动作：丢掉它
// 排队的 URL，并且把它从调度堆里摘掉。
//
// 只丢队列不摘堆是不够的——调度器会照常等到它的时间片，然后去发一条已经
// 不存在的任务。构造上利用「处理完一个域之后它要等满一个间隔才轮到」这一点，
// 让 dead.example 停在「有排队、但一小时内不会被发出」的状态，测试因此
// 不依赖调度协程的具体时序。
func TestAbandonRemovesHostFromSchedule(t *testing.T) {
	f := New(time.Hour, 10, 1) // 全局间隔一小时：销账后该域必然要等
	defer f.Close()

	f.Add("http://dead.example/1", "http://dead.example/2", "http://dead.example/3",
		"http://alive.example/1")

	// 收任务并销账，直到 dead.example 的那一条被处理过
	seenDead := false
	for i := 0; i < 2 && !seenDead; i++ {
		select {
		case task := <-f.Tasks():
			seenDead = task.Host == "dead.example"
			f.Done(task.Host)
		case <-time.After(2 * time.Second):
			t.Fatal("任务没有发出来")
		}
	}
	if !seenDead {
		t.Fatal("没等到 dead.example 的任务，测试前提不成立")
	}

	queuedBefore, _, _ := f.Stats()
	if queuedBefore != 2 {
		t.Fatalf("dead.example 应当还剩 2 条排队，实际 queued = %d", queuedBefore)
	}

	if dropped := f.Abandon("dead.example"); dropped != 2 {
		t.Fatalf("Abandon 丢了 %d 条，期望 2", dropped)
	}
	if queuedAfter, _, _ := f.Stats(); queuedAfter != 0 {
		t.Errorf("放弃之后 queued = %d，期望 0", queuedAfter)
	}
	if w := f.NextWait(); w != 0 {
		t.Errorf("放弃之后还在等 %s——说明这个域仍留在调度堆里，调度器会去发一条"+
			"已经不存在的任务", w)
	}
}

// TestAbandonLeavesOtherHostsAlone 确认放弃是按域进行的，不牵连别人。
func TestAbandonLeavesOtherHostsAlone(t *testing.T) {
	f := New(time.Hour, 10, 1)
	defer f.Close()

	f.Add("http://dead.example/1", "http://dead.example/2")
	f.Add("http://alive.example/1")

	select {
	case task := <-f.Tasks():
		f.Done(task.Host)
	case <-time.After(2 * time.Second):
		t.Fatal("任务没有发出来")
	}

	f.Abandon("dead.example")

	// alive.example 的那条必须还在：它的队列不该被别人的失败清掉
	if _, _, hosts := f.Stats(); hosts != 2 {
		t.Errorf("已知域数 = %d，期望 2", hosts)
	}
	if w := f.NextWait(); w != 0 {
		t.Errorf("NextWait = %s，仍有待发的任务却没等多久？", w)
	}

	select {
	case task := <-f.Tasks():
		if task.Host != "alive.example" {
			t.Errorf("收到的是 %s 的任务，期望 alive.example", task.Host)
		}
		f.Done(task.Host)
	case <-time.After(2 * time.Second):
		t.Error("alive.example 的任务被连坐丢掉了")
	}
}
