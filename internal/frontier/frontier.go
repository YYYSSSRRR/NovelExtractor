// Package frontier 是爬虫的调度核心。
//
// 它用**一个结构**同时提供四个性质，这是整个爬虫设计里唯一值得细看的部分：
//
//	每域串行   一个 host 要么在堆里、要么在某个 worker 手里，不可能同时在两处，
//	           因此同一个域永远不会有两个并发请求
//	全局并行   ready 通道由 N 个 worker 消费，不同 host 天然并行
//	礼貌限速   时间门控下沉进最小堆（nextAt 是唯一真相源），业务代码里没有任何
//	           sleep 散落各处
//	背压       ready 有界，堆满即阻塞调度循环，不会无限堆积 URL
//
// 朴素做法是开 500 个 goroutine 抢一个 URL channel——那会同时向同一个站发
// 几十个并发请求，几分钟内被 ban。「全局并发」和「每域礼貌」是正交的两个
// 约束，必须分开管。
package frontier

import (
	"container/heap"
	"context"
	"hash/fnv"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Task 是交给 worker 的一次抓取。Host 一并带上，worker 完成后要凭它
// 把该域重新排回堆里。
type Task struct {
	Host string
	URL  string
}

type hostState uint8

const (
	stateIdle   hostState = iota // 队列空，不在堆里
	stateQueued                  // 在堆里等自己的时间片
	stateBusy                    // 在某个 worker 手里
)

type hostQueue struct {
	host   string
	urls   []string
	state  hostState
	nextAt time.Time // 该域下一次可以发起请求的时刻
	index  int       // 在堆中的下标，Swap 时维护
	// delay 是该域自己的间隔（robots.txt 的 Crawl-delay），0 表示用全局值。
	// 必须存成字段而不是只写进 nextAt：nextAt 每次 Done 都会被重算，
	// 只写时间戳的话覆盖值会在下一轮被全局间隔冲掉。
	delay time.Duration
}

// gap 返回该域实际使用的请求间隔。站点的声明只能让我们更慢。
func (q *hostQueue) gap(global time.Duration) time.Duration {
	if q.delay > global {
		return q.delay
	}
	return global
}

// hostHeap 按 nextAt 排序的最小堆：堆顶永远是「最该轮到」的那个域。
type hostHeap []*hostQueue

func (h hostHeap) Len() int           { return len(h) }
func (h hostHeap) Less(i, j int) bool { return h[i].nextAt.Before(h[j].nextAt) }

func (h hostHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}

func (h *hostHeap) Push(x any) {
	q := x.(*hostQueue)
	q.index = len(*h)
	*h = append(*h, q)
}

func (h *hostHeap) Pop() any {
	old := *h
	n := len(old)
	q := old[n-1]
	old[n-1] = nil
	q.index = -1
	*h = old[:n-1]
	return q
}

// Frontier 是调度器本体。所有导出方法并发安全。
type Frontier struct {
	delay      time.Duration // 同域两次请求的最小间隔
	perHostMax int           // 单域待抓 URL 上限，防止热门域吃掉全部内存

	mu       sync.Mutex
	hosts    map[string]*hostQueue
	heap     hostHeap
	seen     map[uint64]struct{}
	queued   int // 各域队列里待抓的 URL 总数
	inflight int // 已交给 worker、尚未 Done 的任务数
	total    int // 累计入队过的 URL 数，仅用于区分「尚未开工」与「已经爬完」

	ready   chan Task
	wake    chan struct{}
	drained chan struct{} // 队列自然排空
	stopped chan struct{} // Close 强制收工
	closed  bool
}

// New 构造调度器并启动唯一的调度协程。
//
// buffer 是 ready 通道容量，等于 worker 数——它就是背压的上界：
// 调度器最多领先 worker 这么多任务。
func New(delay time.Duration, perHostMax, buffer int) *Frontier {
	if perHostMax < 1 {
		perHostMax = 1
	}
	if buffer < 1 {
		buffer = 1
	}
	f := &Frontier{
		delay:      delay,
		perHostMax: perHostMax,
		hosts:      make(map[string]*hostQueue),
		seen:       make(map[uint64]struct{}),
		ready:      make(chan Task, buffer),
		wake:       make(chan struct{}, 1),
		drained:    make(chan struct{}),
		stopped:    make(chan struct{}),
	}
	go f.schedule()
	return f
}

// Add 把 URL 并入待抓队列，返回实际新增（去重后）的条数。
//
// 去重在入口一次做完：URL 规范化后取 hash 存集合，重复的直接丢弃。
// 这是全流程唯一需要记住「抓过什么」的地方，manifest 重建时也复用它。
func (f *Frontier) Add(urls ...string) int {
	now := time.Now()
	n := 0

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0
	}
	for _, raw := range urls {
		u, host, ok := normalize(raw)
		if !ok {
			continue
		}
		h := fnv.New64a()
		h.Write([]byte(u))
		sum := h.Sum64()
		if _, dup := f.seen[sum]; dup {
			continue
		}

		q := f.hosts[host]
		if q == nil {
			q = &hostQueue{host: host, index: -1}
			f.hosts[host] = q
		}
		if len(q.urls) >= f.perHostMax {
			continue // 该域已排满，本次不入队（也不记入 seen，将来还有机会）
		}
		f.seen[sum] = struct{}{}
		q.urls = append(q.urls, u)
		f.queued++
		f.total++
		n++

		if q.state == stateIdle {
			q.state = stateQueued
			// 新域立刻可抓；老域若还在冷却期则维持原 nextAt，礼貌性不被绕过
			if q.nextAt.Before(now) {
				q.nextAt = now
			}
			heap.Push(&f.heap, q)
		}
	}
	if n > 0 {
		f.signal()
	}
	return n
}

// Done 由 worker 在**处理完一个任务、并把它发现的新 URL 全部 Add 之后**调用，
// 返回该域是否就此排空（队列里再无待抓 URL）。
//
// 这个顺序是终止判定的前提：先把新 URL 交回队列再销账，所以 queued 和
// inflight 不可能同时为 0 而仍有 worker 在路上——否则会出现「提前判定爬完」，
// 丢掉最后一批发现。
//
// 返回值给调用方一个**域的边界**。跨页统计（站点模板、模板残留）是域名级的量，
// 只有在这个域一页都不剩时才能算准；调度器本来就是唯一知道这件事的地方，
// 与其让上层去猜，不如在这里一并告诉它。判定必须在锁内做：「排空」这个事实
// 只在 Done 的那一刻成立，出了锁就可能被新的 Add 推翻。
func (f *Frontier) Done(host string) (drained bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.inflight--
	q := f.hosts[host]
	if q != nil {
		if len(q.urls) > 0 {
			q.state = stateQueued
			q.nextAt = time.Now().Add(q.gap(f.delay))
			heap.Push(&f.heap, q)
			f.signal()
		} else {
			q.state = stateIdle
			q.nextAt = time.Now().Add(q.gap(f.delay))
			drained = true
		}
	}
	f.checkDrainedLocked()
	return drained
}

// Tasks 返回任务通道。通道关闭意味着全部工作结束，worker 应当退出。
func (f *Frontier) Tasks() <-chan Task { return f.ready }

// SetHostDelay 覆盖某个域的请求间隔，供 robots.txt 的 Crawl-delay 使用。
//
// 之所以能这么简单地加进来，正是因为时间门控只有 nextAt 这一个真相源——
// 站点声明的间隔与其他限速走的是同一条路径，不存在第二套计时逻辑需要同步。
// 只接受比全局 delay 更大的值：站点的声明可以让我们更慢，但不能让我们
// 违反自己的保底礼貌间隔。
func (f *Frontier) SetHostDelay(host string, d time.Duration) {
	if d <= f.delay {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	q := f.hosts[host]
	if q == nil || d <= q.delay {
		return
	}
	q.delay = d

	switch q.state {
	case stateBusy:
		// 正在抓：Done 时会用 q.gap() 按新间隔重排，这里无需动作
	case stateQueued:
		// 把已排的时间往后推，但不能提前（只取更晚的那个）
		if t := time.Now().Add(d); t.After(q.nextAt) {
			q.nextAt = t
			heap.Fix(&f.heap, q.index)
			f.signal()
		}
	}
}

// Gate 阻塞到本域可以发起下一个请求，并立刻占用这个时间片。
//
// 存在的理由：调度器只给「任务」发放时间片，而一个 worker 在自己的时间片里
// 可能发出不止一个请求——某域的第一次访问要顺带拉 robots.txt，拉不到还得换
// 协议重试一次。这些请求过去不走闸门，于是同一域会出现相隔几十毫秒的两个
// 请求，每域限速的承诺就是假的（用 -trace 实测到过，32 对间隔低于配置值）。
//
// 修法不是补丁式地 sleep，而是把「发请求」这件事显式摆到闸门前：任何发往某域
// 的请求都先调用本方法，nextAt 依然是唯一的真相源。调用者必须已经持有该域的
// 时间片（即正在处理该域的 worker），同一域的 Gate 不允许并发调用——每域串行
// 保证了这一点。
func (f *Frontier) Gate(ctx context.Context, host string) error {
	for {
		f.mu.Lock()
		q := f.hosts[host]
		if q == nil {
			f.mu.Unlock()
			return nil
		}
		wait := time.Until(q.nextAt)
		if wait <= 0 {
			// 闸门已开：占用这个时间片，下一个请求必须再等 gap
			q.nextAt = time.Now().Add(q.gap(f.delay))
			f.mu.Unlock()
			return nil
		}
		f.mu.Unlock()

		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}

// Abandon 放弃某个域尚未发出的全部 URL，返回丢弃的条数。
//
// 给「这个站根本爬不动」用：域名解析不了、连接被拒、整站 5xx 的时候，
// 队列里剩下的十几条 URL 一条都拿不到数据，却要为每条各付一次 timeout。
// 调度器是唯一知道「这个域还排着多少条」的地方，所以由它来丢。
//
// 已经发出去的那个任务不受影响：它在某个 worker 手里，收尾时照常 Done。
// 而 Done 会看到空队列，于是返回 drained，域级收尾（含落盘）照常发生——
// 这个域已经抓到的页面不会因为放弃而被丢掉。
func (f *Frontier) Abandon(host string) (dropped int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	q := f.hosts[host]
	if q == nil {
		return 0
	}
	dropped = len(q.urls)
	f.queued -= dropped
	q.urls = nil

	if q.state == stateQueued {
		// 在堆里就得摘掉，否则调度器还会把它当候选，弹出后发一条不存在的任务
		heap.Remove(&f.heap, q.index)
		q.state = stateIdle
		q.index = -1
	}
	// stateBusy 不必处理：Done 时会看到空队列，自然转入 idle
	return dropped
}

// NextWait 返回还要等多久才会有下一个请求发出；堆空时返回 0。
//
// 存在理由是可观测性。调度器可能正安静地睡在堆顶那个域的 nextAt 上，而这时
// queued 与 inflight 都可能很小甚至归零——从外面看，这和「卡死了」一模一样。
// 实测一个声明了 10 分钟 Crawl-delay 的站就这样让整轮爬取停了一个多小时，
// 期间进度行每 10 秒照常打印，却不含任何能看出这件事的信息。
// 时间门控只有 nextAt 一个真相源，所以这里读它一个就够了。
func (f *Frontier) NextWait() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.heap) == 0 {
		return 0
	}
	if d := time.Until(f.heap[0].nextAt); d > 0 {
		return d
	}
	return 0
}

// Stats 返回待抓数、在途数与已知域数，供进度输出使用。
func (f *Frontier) Stats() (queued, inflight, hosts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queued, f.inflight, len(f.hosts)
}

// checkDrainedLocked 在「已经开工」且 queued 与 inflight 同时归零时关闭任务通道。
// 必须持锁调用。
//
// total > 0 这个条件不能省：调度协程在 New 里就启动了，若不加这一条，它会
// 抢在第一次 Add 之前看到「空堆 + 零在途」，当场判定爬完并关掉任务通道——
// 之后所有 Add 都被 f.closed 挡掉，worker 立刻退出，爬虫一页不抓却「正常结束」。
// 这是个真实的竞态，靠运气才不复现，必须有显式条件堵住。
// 返回是否真的判定为「爬完」——调用方必须据此决定要不要退出调度循环。
// 只看 queued/inflight 就退出是错的：那两者在第一次 Add 之前本来就是 0。
func (f *Frontier) checkDrainedLocked() bool {
	if f.closed || f.total == 0 || f.queued > 0 || f.inflight > 0 {
		return false
	}
	f.closed = true
	close(f.drained) // 只发信号；ready 交给调度协程关，理由见 schedule 的注释
	return true
}

// Close 强制收工。两种情形会用到：一个 URL 都没入队的退化情形（total 始终为 0，
// 自动判定不会触发，worker 会永远等下去），以及外部要求立刻停机。
//
// 它只关闭 stopped 这个信号通道，**不碰 ready**。原因见 schedule：调度协程可能
// 正卡在往 ready 发送的那一步，而 select 在多个 case 同时就绪时是随机选的——
// 选中发送就是 send on closed channel，整个进程 panic。
func (f *Frontier) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return // 已经收过工，重复调用不该 panic（close 二次会）
	}
	f.closed = true
	close(f.stopped)
}

// signal 非阻塞地唤醒调度协程。任何可能让堆顶变早的操作之后都要调用。
func (f *Frontier) signal() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// schedule 是唯一的调度协程。它自己从不发起请求，只负责决定「下一个该抓谁」，
// 然后交给 worker——把决策与执行分开，调度逻辑就不必关心网络。
func (f *Frontier) schedule() {
	// ready 只由本协程关闭，且是最后一件做的事。
	//
	// 别处一律不许 close(f.ready)：本协程可能正卡在下面那个 `f.ready <- ...`
	// 上，此时若有人从外部 close，select 的两个 case 会同时就绪，Go 随机挑一个，
	// 挑中发送就 panic。实测就是这样炸的——
	// `panic: send on closed channel` @ frontier.go:440，触发者是 Close()。
	// 把「关通道」收敛到唯一的调度协程、并且排在它自己不再发送之后，这个竞态
	// 就不存在了：信号的传递交给 stopped / drained 两个通道。
	defer close(f.ready)

	for {
		f.mu.Lock()
		if len(f.heap) == 0 {
			// 堆空：要么真的爬完了，要么还有在途任务在等 worker 交回新 URL。
			// checkDrainedLocked 会区分这两种情况——不能一看到 0 就退出，
			// 否则调度协程会在第一次 Add 之前就结束，整个爬虫一页不抓。
			if f.checkDrainedLocked() {
				f.mu.Unlock()
				return
			}
			f.mu.Unlock()
			select {
			case <-f.wake:
			case <-f.drained:
				return
			case <-f.stopped:
				return
			}
			continue
		}

		q := f.heap[0]
		wait := time.Until(q.nextAt)
		if wait > 0 {
			f.mu.Unlock()
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-f.wake: // 有新域入堆，可能比当前堆顶更早，重新判定
				t.Stop()
			case <-f.drained:
				t.Stop()
				return
			case <-f.stopped:
				t.Stop()
				return
			}
			continue
		}

		heap.Pop(&f.heap)
		u := q.urls[0]
		q.urls = q.urls[1:]
		q.state = stateBusy
		f.queued--
		f.inflight++
		f.mu.Unlock()

		select {
		case f.ready <- Task{Host: q.host, URL: u}:
		case <-f.drained:
			return
		case <-f.stopped:
			return
		}
	}
}

// normalize 规范化 URL 并取出 host。
//
// 只做与「是否同一个资源」有关的归一：去片段、主机名小写、去默认端口、
// 去主机名末尾的点。**不删查询参数**——大量站点的文章 id 就在 query 里
// （`?id=936222`），删掉会把不同文章折叠成同一篇。
//
// 去默认端口这一步不只是去重问题，它直接关系到礼貌性：host 同时是
// hostQueue 的键，`api.chinaz.com` 与 `api.chinaz.com:443` 若被当成两个域，
// 就会各自持有一个时间片、并发请求同一台服务器，每域限速的承诺当场失效。
// 实测在 eval/sample_urls.tsv 里出现过 4 条 `:443` 的 URL，就是踩到了这里。
func normalize(raw string) (norm, host string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", "", false
	}

	// Hostname() 而非 Host：顺手剥掉端口，再由下面决定要不要装回去。
	// 末尾的点是 FQDN 的合法写法（example.com. ≡ example.com），不去掉
	// 会让同一个站占两个域名额。
	name := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if name == "" {
		return "", "", false
	}
	port := u.Port()
	if port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" {
		port = ""
	}
	switch {
	case port != "":
		host = net.JoinHostPort(name, port) // JoinHostPort 会给 IPv6 补方括号
	case strings.Contains(name, ":"):
		host = "[" + name + "]"
	default:
		host = name
	}

	u.Fragment = ""
	u.Host = host
	return u.String(), host, true
}
