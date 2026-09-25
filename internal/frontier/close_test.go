package frontier

import (
	"fmt"
	"testing"
	"time"
)

// TestCloseWhileDispatchingDoesNotPanic 守住「强制收工」与「正在派发」撞车的那条路径。
//
// 这不是假想的时序：Close 原本直接 close(f.ready)，而调度协程可能正卡在
// `f.ready <- ...` 上。此时 drained 与 ready 两个 case 同时就绪，Go 在多个就绪
// case 之间**随机**挑一个——挑中发送那一支，就是 send on closed channel，
// 整个进程 panic。
//
// 触发它的现实场景是 Ctrl-C / SIGTERM：停机要求立刻收工，而那一瞬间调度器
// 大概率正在派发任务。结果是「想让爬虫干净退出」反而把它炸掉，比不退出更糟。
//
// 构造上把窗口撑到必然出现：ready 容量给 1，一次入队 50 条，调度协程发完第一条
// 之后必然卡在第二条的发送上。反复多轮是因为 select 的随机性——单轮只有约一半
// 概率选中发送，几轮就能确定性地逼出来。
func TestCloseWhileDispatchingDoesNotPanic(t *testing.T) {
	for i := 0; i < 200; i++ {
		f := New(time.Millisecond, 1000, 1)

		urls := make([]string, 50)
		for j := range urls {
			urls[j] = fmt.Sprintf("http://dispatch.example/%d", j)
		}
		f.Add(urls...)

		// 让调度协程把缓冲填满、停在第二条的发送上
		time.Sleep(time.Millisecond)
		f.Close()

		// 通道必须仍然会被关闭：ready 现在由调度协程在退出前关，收工时
		// 若忘了叫醒它，worker 就永远退不出来——那正是要修的另一个 bug。
		done := make(chan struct{})
		go func() {
			for range f.Tasks() {
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 轮：Close 之后任务通道没有关闭，worker 会永远等下去", i)
		}
	}
}

// TestCloseStopsTheScheduledSleep 盯住「睡在远期时间片上的调度器也要被叫醒」。
//
// 调度协程等待时用的是一段可能很长的定时器（正是 Crawl-delay 那个 bug 的形状），
// 若收工信号没有接进这个 select，Close 就得等定时器自然到期才生效——停机会
// 卡在这里，看起来和没收到信号一样。
func TestCloseStopsTheScheduledSleep(t *testing.T) {
	f := New(time.Hour, 10, 4) // 间隔一小时，销账后必然要长睡
	f.Add("http://slow.example/a", "http://slow.example/b")

	select {
	case task := <-f.Tasks():
		f.Done(task.Host) // 队列还剩一条，该域要等满一小时才会再被排上
	case <-time.After(2 * time.Second):
		t.Fatal("任务没有发出来")
	}
	if w := f.NextWait(); w <= 30*time.Minute {
		t.Fatalf("前提不成立：NextWait = %s，期望它正睡在远期时间片上", w)
	}

	f.Close()

	done := make(chan struct{})
	go func() {
		for range f.Tasks() {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("调度器睡在远期时间片上，Close 没能把它叫醒")
	}
}
