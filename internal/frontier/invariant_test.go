package frontier

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// checkInvariants 把调度器的核心不变量一次性核完。
//
// 这些不变量合起来恰好是「不会死锁」的充分条件，而它们全都是内部的——
// 从 Stats() 那三个计数上看不出来。曾经有过一次生产事故：日志停在
// queued=1 inflight=0 连续二十多分钟不动，三个计数怎么读都自洽，
// 出问题的是「那 1 条 URL 躺在一个 state=idle 的域里」——它不在堆中，
// 调度器永远不会再发它，而 queued 却在诚实地数着它。
//
// 四条不变量：
//
//	queued          == 各域 urls 长度之和（账要平）
//	state=queued    ⟹ 恰好在堆中出现一次（在排队，且只排一次）
//	state=busy      ⟹ 不在堆中（正被 worker 持有，不该被再次调度）
//	state=idle      ⟹ urls 为空且不在堆中（空闲的域不能藏活）
//
// 最后一条是关键：state=idle 却有 URL，就是上面那次事故的形状。
func checkInvariants(t *testing.T, f *Frontier, tag string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()

	inHeap := map[*hostQueue]int{}
	for _, q := range f.heap {
		inHeap[q]++
	}

	sum := 0
	for h, q := range f.hosts {
		sum += len(q.urls)
		switch q.state {
		case stateQueued:
			if inHeap[q] != 1 {
				t.Errorf("[%s] host %s state=queued 却在堆中出现 %d 次", tag, h, inHeap[q])
			}
		case stateBusy:
			if inHeap[q] != 0 {
				t.Errorf("[%s] host %s state=busy 却还在堆里 %d 次", tag, h, inHeap[q])
			}
		case stateIdle:
			if len(q.urls) > 0 {
				t.Errorf("[%s] host %s state=idle 却有 %d 条 URL —— 永远不会被调度（死锁）",
					tag, h, len(q.urls))
			}
			if inHeap[q] != 0 {
				t.Errorf("[%s] host %s state=idle 却还在堆里 %d 次", tag, h, inHeap[q])
			}
		}
	}
	if sum != f.queued {
		t.Errorf("[%s] queued=%d 但各域 URL 实际总数=%d", tag, f.queued, sum)
	}
	for q, n := range inHeap {
		if n != 1 {
			t.Errorf("[%s] 堆里有重复条目 host=%s ×%d", tag, q.host, n)
		}
	}
}

// TestInvariantsAcrossRandomConcurrency 用随机的 Add/Abandon/Done 交错去撞
// 上面那四条不变量。
//
// 之所以要随机：这些不变量的破坏方式是「某个操作序列下的状态迁移漏了一种」，
// 而漏掉的那一种通常写代码的人当时没想到——按想的路径去测，测不到。
// 每个迭代换一个随机种子，覆盖的是不同的交错顺序。
//
// worker 数（4）超过域数（6）是被允许的：调度器只保证**同域**串行，
// 跨域并发度由 worker 池决定，这两件事互不相关，混在一起测才有意义。
func TestInvariantsAcrossRandomConcurrency(t *testing.T) {
	for iter := 0; iter < 300; iter++ {
		f := New(time.Millisecond, 4, 4)
		const hosts = 6
		for i := 0; i < hosts; i++ {
			for j := 0; j < 4; j++ {
				f.Add(fmt.Sprintf("http://h%d.example/%d", i, j))
			}
		}

		rng := rand.New(rand.NewSource(int64(iter)))
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					var task Task
					select {
					case tk, ok := <-f.Tasks():
						if !ok {
							return
						}
						task = tk
					case <-time.After(300 * time.Millisecond):
						return // 超时退出，是否死锁交给主协程判定
					}
					// 模拟 handle：先可能 Add 同域新 URL，再（可能）Abandon，最后销账
					if rng.Intn(3) == 0 {
						f.Add(fmt.Sprintf("http://%s/n%d", task.Host, rng.Intn(1000)))
					}
					if rng.Intn(5) == 0 {
						f.Abandon(task.Host)
					}
					f.Done(task.Host)
				}
			}()
		}

		waited := make(chan struct{})
		go func() { wg.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			// 卡住时把每个域的状态打出来——不变量报告只能指出「哪个域不对」，
			// 而卡住的原因通常要连 index（堆内位置）一起看才明白。
			checkInvariants(t, f, fmt.Sprintf("iter=%d 卡死", iter))
			queued, inflight, hosts := f.Stats()
			dump := describeHosts(f)
			t.Fatalf("iter=%d 死锁: queued=%d inflight=%d hosts=%d\n各域: %v",
				iter, queued, inflight, hosts, dump)
		}
		checkInvariants(t, f, fmt.Sprintf("iter=%d", iter))
		f.Close()
	}
}

// describeHosts 打印每个域的状态，只在断言失败时调用。
func describeHosts(f *Frontier) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.hosts))
	for h, q := range f.hosts {
		out = append(out, fmt.Sprintf("%s(state=%d,urls=%d,idx=%d)", h, q.state, len(q.urls), q.index))
	}
	return out
}
