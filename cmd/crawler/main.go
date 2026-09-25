// Command crawler 是并发爬虫：从种子域名出发，每域抓若干篇正文页，边抓边抽，
// 最终产出 data/pages.jsonl——一行一页的 {url, meta_title, real_title, main_content}。
//
// 结构上它就是四个部件的接线：
//
//	frontier   决定「下一个抓谁」——每域串行、全局并行、礼貌限速、背压
//	fetch      把 URL 变成 UTF-8 HTML——编码统一、体积上限、诚实重试
//	discover   便宜的 URL 粗筛——找出哪些链接像是具体一篇内容
//	extract    贵的正文判定——既抽正文，又兼作页面类型分类器
//
// 每域的抓取走一条四级漏斗：首页 → 抽同域链接 → URL 粗筛排序 → 抓回来用
// 抽取器精筛。预算是硬约束，所以先把钱花在最像正文页的 URL 上。
//
// **抽取在抓取时就地完成，原始 HTML 不落盘**：题目要的交付物是标题与正文，
// HTML 只是中间态；把它留成磁盘上的 700MB 换来的唯一好处是「不重爬就能换
// 算法重抽」，代价与收益不成比例。代价是有意接受的，README 的取舍一节写明。
//
// 唯一需要跨页上下文的是站点模板与模板残留，它们都是**域名级**统计量。
// 调度器保证同一域串行，一个域的页面因此会在内存里自然聚齐，等 frontier
// 报告该域排空时统一收尾即可——不需要磁盘上的原文做中转。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"web-extract/internal/discover"
	"web-extract/internal/extract"
	"web-extract/internal/fetch"
	"web-extract/internal/frontier"
	"web-extract/internal/store"
)

type config struct {
	dataDir    string
	seedsPath  string
	workers    int
	delay      time.Duration
	timeout    time.Duration
	maxBytes   int64
	perHost    int // 每域目标正文页数
	budget     int // 每域抓取总预算（含首页）
	maxPages   int // 全局页数上限，0 表示不限
	minContent int // 判为正文页的最小字符数
	maxLinkRt  float64

	// retries 是单个 URL 的失败重试次数，默认 0：抓不到就跳过。
	retries int
	// maxHostFailures 是「连续这么多次抓取失败就放弃整个域」的阈值。
	maxHostFailures int

	ua string

	// noTemplates 关闭跨页模板过滤，仅用于消融实验：关掉后指标应当变差
	// （正文里混进站点页脚），否则说明这套机制没起作用。
	noTemplates bool

	// tracePath 非空时，把每次请求的发起时刻逐行记下来。
	//
	// 「每域请求间隔 ≥ delay」是这套调度器最该被验证的性质，而它无法从结果
	// 反推——只能把请求时刻本身记下来再算间隔。日志打在 stderr 上会被进度
	// 信息淹没，所以单独写一个文件。
	tracePath string
}

func main() {
	var c config
	flag.StringVar(&c.dataDir, "data", "data", "数据目录：抽取产物 pages.jsonl 与待抓队列落在这里")
	flag.StringVar(&c.seedsPath, "seeds", "", "种子文件，一行一个 URL 或域名")
	flag.IntVar(&c.workers, "workers", 32, "并发 worker 数（= 同时在抓的不同域数）")
	flag.DurationVar(&c.delay, "delay", 1500*time.Millisecond, "同一域名两次请求的最小间隔")
	flag.DurationVar(&c.timeout, "timeout", 20*time.Second, "单次请求超时")
	flag.Int64Var(&c.maxBytes, "max-bytes", 5<<20, "响应体上限")
	flag.IntVar(&c.perHost, "per-host", 5, "每域目标正文页数")
	flag.IntVar(&c.budget, "budget", 12, "每域抓取总预算（含首页与列表页）")
	flag.IntVar(&c.maxPages, "max-pages", 0, "全局页数上限，0 为不限")
	flag.IntVar(&c.minContent, "min-content", 300, "判为正文页的最小正文长度")
	flag.Float64Var(&c.maxLinkRt, "max-link-rate", 0.3, "判为正文页的最大正文链接密度")
	flag.IntVar(&c.retries, "retries", 0, "单个 URL 的失败重试次数，0 表示抓不到就跳过")
	flag.IntVar(&c.maxHostFailures, "max-host-failures", 2,
		"同一域连续失败这么多次就放弃该域剩余队列；0 表示不放弃")
	flag.StringVar(&c.ua, "ua", fetch.DefaultUA, "User-Agent（单一、诚实，不轮换）")
	flag.BoolVar(&c.noTemplates, "no-templates", false, "消融：关闭跨页模板过滤")
	flag.StringVar(&c.tracePath, "trace", "", "把每次请求的发起时刻写入该文件，用于验证每域请求间隔")
	flag.Parse()

	if c.workers < 1 {
		c.workers = runtime.GOMAXPROCS(0)
	}
	if err := run(c); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "crawler: %v\n", err)
		os.Exit(1)
	}
}

func run(c config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(c.dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	trace, err := openTrace(c.tracePath)
	if err != nil {
		return err
	}
	defer trace.Close()

	// 续爬：先种子、再读回上次没抓完的队列。两边都交给 frontier 去重，
	// 已完成的页面由 store 的 manifest 集合跳过。
	seeds, err := readSeeds(c.seedsPath)
	if err != nil {
		return err
	}
	queued, err := st.LoadQueue()
	if err != nil {
		return err
	}
	if len(seeds) == 0 && len(queued) == 0 {
		return errors.New("没有种子，也没有可续的队列：用 -seeds 指定种子文件")
	}

	cr := &crawler{
		cfg:           c,
		client:        fetch.New(c.ua, c.timeout, c.maxBytes).SetRetries(c.retries),
		fr:            frontier.New(c.delay, c.budget, c.workers),
		st:            st,
		ext:           extract.New(extract.Options{}),
		learner:       extract.NewChromeLearner(),
		robots:        make(map[string]*fetch.Robots),
		sites:         make(map[string]*siteState),
		start:         time.Now(),
		delayReported: make(map[string]bool),
		trace:         trace,
	}
	cr.stats.queuedAt = make([]int64, 0)

	fmt.Fprintf(os.Stderr, "seed=%d resume-queue=%d workers=%d delay=%s budget/host=%d target/host=%d\n",
		len(seeds), len(queued), c.workers, c.delay, c.budget, c.perHost)
	if n := cr.add(seeds); n > 0 {
		fmt.Fprintf(os.Stderr, "已入队 %d 个种子\n", n)
	}
	if n := cr.add(queued); n > 0 {
		fmt.Fprintf(os.Stderr, "已从队列续入 %d 个 URL\n", n)
	}

	cr.stopOnCancel(ctx)

	var wg sync.WaitGroup
	for i := 0; i < c.workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); cr.work(ctx) }()
	}

	progressDone := make(chan struct{})
	go cr.report(progressDone)

	wg.Wait()
	close(progressDone)
	cr.flushRemaining()
	if err := st.Flush(); err != nil {
		return err
	}
	cr.checkPersistence()
	cr.printFinal(os.Stderr)
	return ctx.Err()
}

// checkPersistence 核对「处理过的页面」与「落盘的页面」是否对得上，对不上就吼。
//
// 加这道检查是因为踩过一次静默丢数据：一轮 15584 页的爬取只落盘 315 行，
// 而所有计数、所有日志都正常——丢失发生在「本该落盘的那些页面根本没走到
// Save」这条路径上，它不产生任何错误，也就没有任何一处会报。
//
// 判据刻意取得很宽（差 2% 以上才报）：多域并发时，被 kill 的那一刻总会有
// 几页正在手上、来不及落盘，这属于正常损耗，不该天天报警把真正的信号淹掉。
// 要抓的是「数量级对不上」这种量级的异常。
func (c *crawler) checkPersistence() {
	handled := atomic.LoadInt64(&c.stats.pages)
	saved := int64(c.st.Saved())
	skipped := atomic.LoadInt64(&c.stats.skipped)
	// 去重跳过的页面本来就该没有产物，把它们从分母里去掉再比
	want := handled - skipped
	if want <= 0 || saved*100 >= want*98 {
		return
	}
	fmt.Fprintf(os.Stderr,
		"\n!! 落盘数对不上：处理 %d 页（另有 %d 页因去重跳过），落盘只有 %d 行，"+
			"缺失 %d 页。这不是正常损耗，是页面在内存里被丢掉了——"+
			"检查 flushSite 的调用时机。\n\n",
		handled, skipped, saved, want-saved)
}

// crawler 把所有部件攥在一起。除 frontier 与 store 自带锁外，
// 这里的共享状态（sites / robots / stats）由 mu 保护。
type crawler struct {
	cfg     config
	client  *fetch.Client
	fr      *frontier.Frontier
	st      *store.Store
	ext     *extract.Extractor
	learner *extract.ChromeLearner
	start   time.Time

	mu     sync.Mutex
	sites  map[string]*siteState
	robots map[string]*fetch.Robots
	// delayReported 记住哪些域已因 Crawl-delay 过长报过信：allowed 是每个
	// URL 调一次，不去重就会把日志淹掉，而这条信息恰是排查停滞时先要看的
	delayReported map[string]bool
	stats         stats

	trace *traceWriter
}

// traceWriter 逐行记请求时刻。多 worker 并发写，故自带锁。
type traceWriter struct {
	mu sync.Mutex
	f  *os.File
	bw *bufio.Writer
}

func openTrace(path string) (*traceWriter, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &traceWriter{f: f, bw: bufio.NewWriter(f)}, nil
}

func (t *traceWriter) req(host, url string, at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.bw, "%s\t%s\t%s\n", at.Format(time.RFC3339Nano), host, url)
}

func (t *traceWriter) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.bw.Flush(); err != nil {
		t.f.Close()
		return err
	}
	return t.f.Close()
}

type siteState struct {
	fetched   int  // 本域已抓页数
	articles  int  // 本域已确认的正文页数
	seeded    bool // 首页是否已处理过（漏斗只跑一次）
	flushed   bool // 是否**至少**收过一次尾，只用于统计口径
	targetMet bool // 已够 perHost 篇正文，本域剩余队列已丢弃（只丢一次）
	// failStreak 是该域连续抓取失败的次数，成功一次即清零。见 noteHostFailure。
	failStreak int

	// tm 是本域学到的模板，学过就留着给后续每一批复用。
	//
	// 一个域的一生不只有一次收尾：先被别的站发现、爬完、排空、收尾，之后
	// 又被另一个站发现，于是再次入队、再抓一批、再次排空。模板是域名级的
	// 量，第二批页面没有理由用不上第一批学到的它。
	tm *extract.Templates

	// pages 攒着本域已抽取、尚未落盘的页面。
	//
	// 之所以不逐页落盘：跨页模板过滤是**域名级统计量**——「这段文字是这篇文章
	// 的内容还是这个站的页脚」，单看一页永远判断不了，必须等同域的多页都到手。
	// 而调度器保证同一域串行，攒着几乎不占内存（每域十余页 × 每页千余字符）。
	//
	// 但**不能只靠「排空」这一个时机落盘**。见 flushSite 的说明。
	pages []pageOut
}

// pageOut 是一页的抽取产物，等待域级收尾。
type pageOut struct {
	entry store.Entry
	res   extract.Result
}

type stats struct {
	pages, articles, empty, failed, skipped, robotsBlocked int64
	delayBlocked, hostAbandoned                            int64
	targetMet, dropped                                     int64
	sitesFlushed                                           int64
	bytes                                                  int64
	contentChars, linkChars, pageChars                     int64
	queuedAt                                               []int64
}

// add 入队并持久化。先写 queue 文件再交给 frontier：万一此刻被 kill，
// 最坏是队列里多几条重复 URL（会被去重），不会丢。
func (c *crawler) add(urls []string) int {
	urls = normalizeSeeds(urls)
	if err := c.st.AppendQueue(urls); err != nil {
		fmt.Fprintf(os.Stderr, "warn: 队列持久化失败: %v\n", err)
	}
	return c.fr.Add(urls...)
}

// stopOnCancel 让 ctx 一取消就主动关掉任务通道，不要等队列自然排空。
//
// worker 是在 `range c.fr.Tasks()` 上等的，而这个通道只在队列排空时才关；
// 队列里完全可能排着某个域几十分钟后的下一次请求——收到 SIGTERM 时最常见
// 的恰恰是这个状态。于是 wg.Wait() 永远不返回、flushRemaining 永远不执行、
// 进程挂着不退，只能 kill -9，内存里那批页面陪葬。
//
// 实测：一个已经抓到 15584 页的进程收到 SIGTERM 后原地停了二十多分钟、
// 一页没落盘，最后是 SIGKILL 收的场。「能优雅退出」不是锦上添花——只存
// 抽取结果、不存原始 HTML 这个取舍，全部押在「收尾那一步一定会跑到」上。
//
// 单独拎成方法是为了能被测试直接调用：停机这条路径的价值全在「真的会跑到」，
// 测试里重抄一遍接线就只能证明那份抄写是对的。
func (c *crawler) stopOnCancel(ctx context.Context) {
	go func() {
		<-ctx.Done()
		c.fr.Close()
	}()
}

func (c *crawler) work(ctx context.Context) {
	for t := range c.fr.Tasks() {
		c.handle(ctx, t)
	}
}

// handle 处理单个任务。discovered 里的 URL 必须在 Done 之前归还给 frontier，
// 否则可能被误判为「全部爬完」而丢掉这最后一批发现。
func (c *crawler) handle(ctx context.Context, t frontier.Task) {
	var discovered []string
	defer func() {
		if len(discovered) > 0 {
			c.add(discovered)
		}
		// 必须先把新发现的 URL 交回队列再销账：Done 返回「是否排空」，
		// 顺序颠倒会让本域刚发现的候选还没入队就被判为爬完，然后被收尾落盘，
		// 那批候选就永远留在队列里没人处理。
		if c.fr.Done(t.Host) {
			c.flushSite(t.Host)
		}
	}()

	if c.cfg.maxPages > 0 && atomic.LoadInt64(&c.stats.pages) >= int64(c.cfg.maxPages) {
		return
	}
	if c.st.Has(t.URL) {
		atomic.AddInt64(&c.stats.skipped, 1)
		return
	}

	if ok, path := c.allowed(ctx, t.Host, t.URL); !ok {
		if c.delayBlockedHost(t.Host) {
			// 原因已经在 noteDelayBlocked 里逐域报过一次，这里不再重复刷屏
			atomic.AddInt64(&c.stats.delayBlocked, 1)
		} else {
			atomic.AddInt64(&c.stats.robotsBlocked, 1)
			fmt.Fprintf(os.Stderr, "robots 禁止，跳过: %s%s\n", t.Host, path)
		}
		return
	}

	res, err := c.doGet(ctx, t.Host, t.URL)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		atomic.AddInt64(&c.stats.failed, 1)
		fmt.Fprintf(os.Stderr, "抓取失败 %s: %v\n", t.URL, err)
		c.noteHostFailure(t.Host)
		return
	}
	c.clearHostFailure(t.Host)

	// 抓取时就应用跨页学到的站点模板；随后把本页的 meta_title 交给学习器，
	// 供同域后续页面使用。这是一个在线学习过程：同域越往后越准。
	ch := c.learner.Chrome(t.Host)
	out, exErr := c.ext.ExtractWithChrome(res.FinalURL, res.HTML, ch)
	if exErr == nil {
		c.learner.Observe(t.Host, out.MetaTitle)
	}

	entry := store.Entry{
		URL:       t.URL,
		FinalURL:  res.FinalURL,
		Host:      t.Host,
		Status:    res.StatusCode,
		Bytes:     res.Bytes,
		HTMLSha1:  store.Digest(res.HTML),
		ElapsedMS: res.Elapsed.Milliseconds(),
		FetchedAt: time.Now(),
	}
	if exErr == nil {
		entry.MetaTitle = out.MetaTitle
		entry.RealTitle = out.RealTitle
		entry.MainContent = out.MainContent
		entry.Chars = out.ContentChars
		entry.LinkRate = out.LinkRate
	}

	// 判「是不是正文页」用的是**清洗前**的长度：这一步要即时反馈给漏斗，
	// 决定本域还要不要再抓候选，等不到域级收尾。
	//
	// 两个条件回答的是两个不同的问题，缺一不可：
	//   !NoArticle  算法层面「这一页有没有正文」——纯中文阈值与站点规则
	//               做不了这个判断，所以它归 extract 包，那里能同时看到
	//               选中容器的语义、链接密度与字符数。
	//   minContent  语料层面「够不够长才值得收」——这是策略，随用途变，
	//               所以它留在命令行参数里。
	// 过去只有后者，等于拿一个长度阈值去兼任两件事：菜单页只要够长就被当成
	// 文章收进来（263 企业邮 2164 字符就是这么进来的）。
	isArticle := exErr == nil &&
		!out.NoArticle &&
		out.ContentChars >= c.cfg.minContent &&
		out.LinkRate <= c.cfg.maxLinkRt

	atomic.AddInt64(&c.stats.pages, 1)
	atomic.AddInt64(&c.stats.bytes, int64(res.Bytes))
	if isArticle {
		atomic.AddInt64(&c.stats.articles, 1)
	}

	// 漏斗：每个域只在首页跑一次发现，其余页面只计数并继续爬已排队的候选。
	s := c.site(t.Host)
	s.fetched++
	if isArticle {
		s.articles++
	}
	s.pages = append(s.pages, pageOut{entry: entry, res: out})
	if !s.seeded {
		s.seeded = true
		discovered = c.funnel(t.Host, t.URL, res.HTML, s)
	}

	c.stopHostIfSatisfied(t.Host, s)
}

// stopHostIfSatisfied 在本域已收够正文页时丢掉它剩下的候选队列。
//
// 过去 -per-host 只是被解析、被打印在启动横幅里，然后就被忘了：
// 每个域一律把 budget（默认 12）条候选全部抓完，哪怕早就拿到了目标数量的
// 正文页。这是纯粹的浪费——每条都要占该域一个时间片（默认 1.5 秒的礼貌
// 间隔），而这一轮的瓶颈恰恰就是「域数 × 每域请求数 × 间隔 ÷ worker 数」。
//
// 丢弃的是**队列**，已经抓到的页面照常收尾落盘：Abandon 之后队列变空，
// 紧接着的 Done 就会返回「本域已排空」，flushSite 照常跑。
//
// 单独拎出来是为了让它可测：这一段的判断条件（perHost>0、已够数、还没收过手）
// 与副作用（Abandon + 计数）如果散在 handle 尾部，测试就只能把条件再抄一遍，
// 那样证明的只是抄写没写错。
func (c *crawler) stopHostIfSatisfied(host string, s *siteState) {
	if c.cfg.perHost <= 0 || s.articles < c.cfg.perHost || s.targetMet {
		return
	}
	s.targetMet = true
	if dropped := c.fr.Abandon(host); dropped > 0 {
		atomic.AddInt64(&c.stats.targetMet, 1)
		atomic.AddInt64(&c.stats.dropped, int64(dropped))
	}
}

// flushSite 把一个域**当前攒下的**页面收尾落盘：学跨页模板、清洗正文、写文件。
//
// 这是「只存抽取结果、不存原始 HTML」能成立的关键一步——跨页统计所需的
// 上下文（同域多页的正文）此刻全在内存里，用完即弃，不必落盘再读回来。
//
// 落盘的数值一律按**清洗后**的正文重算，保证产物文件里 main_content 的长度
// 与 content_chars 对得上；若照搬抓取时的旧值，读产物的人会发现两者矛盾。
//
// **它可以被同一个域调用多次，而且必须可以。** 曾经这里用 `s.flushed` 做
// 一次性闩锁，理由是「一个域只排空一次」——这个前提是错的：一个域先被 A 站
// 发现、爬完、排空、收尾，之后又被 B 站发现，于是再次入队、再抓一批、再次
// 排空。第二批页面在第 289 行照常 append 进 s.pages，到收尾时却被闩锁挡回去，
// 既没落盘、也不报错（Save 根本没被调用，所以「落盘失败」计数是 0）——
// 数据静默消失。实测一轮 15584 页的爬取只落盘 315 行，账面上却一切正常。
//
// 现在的语义是「把手上这批写完」，模板只学一次、之后每批复用；统计口径上的
// sitesFlushed 仍然只在第一次收尾时加一，它数的是「收过尾的域」而不是「收尾次数」。
func (c *crawler) flushSite(host string) {
	c.mu.Lock()
	s := c.sites[host]
	if s == nil || len(s.pages) == 0 {
		c.mu.Unlock()
		return
	}
	first := !s.flushed
	s.flushed = true
	pages := s.pages
	s.pages = nil // 尽早交还给 GC：收尾期间该域不会有新页面进来
	tm := s.tm
	c.mu.Unlock()

	if !c.cfg.noTemplates {
		if tm == nil {
			contents := make([]string, len(pages))
			for i := range pages {
				contents[i] = pages[i].res.MainContent
			}
			// LearnTemplates 自己带门槛（tmplMinPages），页数不够会返回 nil，
			// 那时不缓存，留到页数够了的那一批再学。
			if tm = extract.LearnTemplates(contents); tm != nil {
				c.mu.Lock()
				s.tm = tm
				c.mu.Unlock()
			}
		}
		if tm != nil {
			for i := range pages {
				cleaned := tm.Strip(pages[i].res.MainContent)
				if cleaned == pages[i].res.MainContent {
					continue
				}
				pages[i].res.MainContent = cleaned
				pages[i].res.ContentChars = extract.CountContent(cleaned)
				pages[i].entry.MainContent = cleaned
				pages[i].entry.Chars = pages[i].res.ContentChars
			}
		}
	}

	var empty, contentChars, linkChars, pageChars int64
	for i := range pages {
		e := &pages[i].entry
		if err := c.st.Save(*e); err != nil {
			fmt.Fprintf(os.Stderr, "落盘失败 %s: %v\n", e.URL, err)
		}
		if e.MainContent == "" {
			empty++
		}
		contentChars += int64(pages[i].res.ContentChars)
		linkChars += int64(pages[i].res.LinkChars)
		pageChars += int64(pages[i].res.PageChars)
	}
	atomic.AddInt64(&c.stats.empty, empty)
	atomic.AddInt64(&c.stats.contentChars, contentChars)
	atomic.AddInt64(&c.stats.linkChars, linkChars)
	atomic.AddInt64(&c.stats.pageChars, pageChars)
	if first {
		atomic.AddInt64(&c.stats.sitesFlushed, 1)
	}
}

// flushRemaining 在所有 worker 退出后兜底收尾。
//
// 正常路径下每个域都在 frontier.Done 返回「排空」时收过尾了，这里是保险：
// 万一某个域的排空信号没走到（例如未来改动引入新的提前返回分支），
// 兜底至少保证它的页面不会**静默丢失**——宁可晚一点落盘，不可丢数据。
func (c *crawler) flushRemaining() {
	c.mu.Lock()
	hosts := make([]string, 0, len(c.sites))
	for h, s := range c.sites {
		if len(s.pages) > 0 { // 不看 s.flushed：收过尾的域也可能又攒下了新的一批
			hosts = append(hosts, h)
		}
	}
	c.mu.Unlock()

	for _, h := range hosts {
		c.flushSite(h)
	}
	if len(hosts) > 0 {
		fmt.Fprintf(os.Stderr, "兜底收尾 %d 个域\n", len(hosts))
	}
}

// funnel 是四级漏斗的前三级，返回本域要继续抓的候选 URL。
//
// 第四级（抽取器精筛）不在这里——它就是每个页面进来时跑的那次 Extract，
// 结果已经在 handle 里判过了。这是本设计里最省事的一处：同一个抽取算法
// 既抽正文又当页面类型分类器，没有引入第二套站点知识。
func (c *crawler) funnel(host, pageURL, htmlStr string, s *siteState) []string {
	links := discover.Links(pageURL, htmlStr)
	remain := c.cfg.budget - s.fetched
	if remain <= 0 {
		return nil
	}
	cands := discover.PickArticles(links, remain)

	out := make([]string, 0, len(cands))
	for _, cand := range cands {
		out = append(out, cand.URL)
	}
	if len(out) > 0 {
		fmt.Fprintf(os.Stderr, "发现 %s: 同域链接 %d 条 → 候选 %d 条\n", host, len(links), len(out))
	}
	return out
}

// doGet 是爬虫发出**任何** HTTP 请求的唯一出口：先过本域的时间闸，再记 trace，
// 最后才真正发出去。
//
// 把「过闸」和「记 trace」绑进同一个函数，是为了让「每个请求都受每域限速约束、
// 且每个请求都留痕」成为结构上的保证，而不是靠调用方自觉。robots.txt 曾经就是
// 这样漏掉的：它由 handle 在页面请求之前顺手发出，看起来还在该域独占的时间片里，
// 实际上完全没等间隔，换协议重试时更是两个请求背靠背。-trace 一量就露馅。
func (c *crawler) doGet(ctx context.Context, host, rawURL string) (*fetch.Result, error) {
	if err := c.fr.Gate(ctx, host); err != nil {
		return nil, err
	}
	c.trace.req(host, rawURL, time.Now())
	return c.client.Get(ctx, rawURL)
}

// maxHostDelay 是愿意照做的 Crawl-delay 上限，超过就放弃整个域。
//
// 这个值只影响极少数站点：常见的 Crawl-delay 是 1~30 秒，都在上限之内，
// 照做即可。设上限针对的是那些声明了小时级间隔的站——照做意味着这一轮爬虫
// 永远结束不了，而不照做又不诚实，所以第三条路是不去。
//
// 顺带一提，「为什么这轮爬取不结束」这类问题过去在日志里完全看不出来，
// 因为调度器只是安静地睡在堆顶那个域的 nextAt 上，没有任何一处会报。
const maxHostDelay = 60 * time.Second

// noteDelayBlocked 记录一个因为 Crawl-delay 过长而放弃的域，同一域只报一次。
//
// 去重是必要的：allowed 每个 URL 都要调一次，1067 个域里哪怕只有几个命中，
// 不去重也会把日志淹掉，而这条信息恰恰是排查停滞时首先要看的。
func (c *crawler) noteDelayBlocked(host string, d time.Duration) {
	c.mu.Lock()
	if c.delayReported[host] {
		c.mu.Unlock()
		return
	}
	c.delayReported[host] = true
	c.mu.Unlock()

	fmt.Fprintf(os.Stderr, "Crawl-delay %s 超过上限 %s，放弃该域: %s\n", d, maxHostDelay, host)
}

// noteHostFailure 记一次抓取失败；连续失败到阈值就放弃这个域的剩余队列。
//
// 域名解析不了、整站 5xx、连接被拒的时候，队列里剩下的每条 URL 都注定失败，
// 却各自要付一次 timeout。与其一条条撞过去，不如认账走人——这是「抓不到就
// 跳过」在域一级的形式，比只跳过单条 URL 省得多。
//
// 阈值默认 2 而不是 1：单次失败不足定罪，网络抖动很常见；连续两次基本就能
// 说明这个域此刻确实不可达。放弃的是**队列**，已经抓到的页面照常落盘。
func (c *crawler) noteHostFailure(host string) {
	if c.cfg.maxHostFailures <= 0 {
		return
	}
	c.mu.Lock()
	s := c.sites[host]
	if s == nil {
		s = &siteState{}
		c.sites[host] = s
	}
	s.failStreak++
	n := s.failStreak
	c.mu.Unlock()

	if n < c.cfg.maxHostFailures {
		return
	}
	if dropped := c.fr.Abandon(host); dropped > 0 {
		atomic.AddInt64(&c.stats.hostAbandoned, 1)
		fmt.Fprintf(os.Stderr, "连续失败 %d 次，放弃该域剩余 %d 条: %s\n", n, dropped, host)
	}
}

// clearHostFailure 在一页抓成功之后清零连续失败计数。
func (c *crawler) clearHostFailure(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.sites[host]; s != nil {
		s.failStreak = 0
	}
}

// delayBlockedHost 报告某个域是否因 Crawl-delay 过长被放弃。
// 调用方据此把「我们主动不去」和「robots 不许去」分开计数——两者都是跳过，
// 但原因不同，混在一起就再也分不清一轮爬取里各占多少。
func (c *crawler) delayBlockedHost(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.delayReported[host]
}

// allowed 查 robots.txt。结果按域缓存；首次访问该域时才真正拉取。
// 拉取走 doGet，和页面请求一样过闸、一样留痕。
func (c *crawler) allowed(ctx context.Context, host, rawURL string) (bool, string) {
	rb := c.robotsFor(ctx, host)
	if rb == nil {
		return true, ""
	}
	if d := rb.Delay(); d > 0 {
		// 站点声明的间隔超过我们能接受的上限，就**整个域都不去**，而不是照做。
		// 两条路都礼貌，但「照做一个 3600 秒的间隔」会让整轮爬取停在这一台上
		// 等它——实测就是这样挂了一个多小时：queue=6、inflight=0、日志一行不响，
		// 因为调度协程只是安静地睡在堆顶那个域的 nextAt 上。
		// 与其把「一轮爬取能不能结束」交给 1067 个站里最保守的那个声明，
		// 不如尊重它的意思：它不想被爬得比这更快，那我们就不爬。
		if d > maxHostDelay {
			c.noteDelayBlocked(host, d)
			return false, "/"
		}
		c.fr.SetHostDelay(host, d)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return true, ""
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	return rb.Allowed(p), p
}

func (c *crawler) robotsFor(ctx context.Context, host string) *fetch.Robots {
	c.mu.Lock()
	if rb, ok := c.robots[host]; ok {
		c.mu.Unlock()
		return rb
	}
	// 先占坑，避免同域并发重复拉取（虽然每域串行已保证了这点，但保持幂等更稳）
	c.robots[host] = nil
	c.mu.Unlock()

	rb := c.fetchRobots(ctx, host)

	c.mu.Lock()
	c.robots[host] = rb
	c.mu.Unlock()
	return rb
}

func (c *crawler) fetchRobots(ctx context.Context, host string) *fetch.Robots {
	u := "http://" + host + "/robots.txt"
	res, err := c.doGet(ctx, host, u)
	if err != nil || res.StatusCode != 200 {
		u = "https://" + host + "/robots.txt"
		res, err = c.doGet(ctx, host, u)
	}
	if err != nil || res.StatusCode != 200 {
		return nil // 拿不到 robots 视为无限制
	}
	return fetch.ParseRobots(strings.NewReader(res.HTML), c.cfg.ua)
}

func (c *crawler) site(host string) *siteState {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sites[host]
	if s == nil {
		s = &siteState{}
		c.sites[host] = s
	}
	return s
}

// report 定期往 stderr 打进度。stdout 保持干净，日志一律走 stderr。
func (c *crawler) report(done <-chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			c.printProgress(os.Stderr)
		}
	}
}

func (c *crawler) printProgress(w io.Writer) {
	pages := atomic.LoadInt64(&c.stats.pages)
	queued, inflight, hosts := c.fr.Stats()
	el := time.Since(c.start).Seconds()
	rate := 0.0
	if el > 0 {
		rate = float64(pages) / el
	}
	fmt.Fprintf(w, "[%s] pages=%d articles=%d hosts=%d queue=%d inflight=%d %.1f pages/s 失败=%d 跳过=%d",
		time.Now().Format("15:04:05"), pages, atomic.LoadInt64(&c.stats.articles),
		hosts, queued, inflight, rate,
		atomic.LoadInt64(&c.stats.failed), atomic.LoadInt64(&c.stats.skipped))

	// 还要等很久才发下一个请求时把它写出来。否则页面数长时间不动、queue 又很小，
	// 看起来就是卡死，而实际上只是某个域在按自己声明的间隔慢慢等。
	if wait := c.fr.NextWait(); wait >= 30*time.Second {
		fmt.Fprintf(w, " 下次请求还要等 %s", wait.Round(time.Second))
	}
	fmt.Fprintln(w)
}

func (c *crawler) printFinal(w io.Writer) {
	el := time.Since(c.start)
	pages := atomic.LoadInt64(&c.stats.pages)
	rate := 0.0
	if el.Seconds() > 0 {
		rate = float64(pages) / el.Seconds()
	}
	linkRate := 0.0
	if cc := atomic.LoadInt64(&c.stats.contentChars); cc > 0 {
		linkRate = float64(atomic.LoadInt64(&c.stats.linkChars)) / float64(cc)
	}
	fmt.Fprintf(w, "\n=== 抓取结束 ===\n")
	fmt.Fprintf(w, "用时        %s（%.1f pages/s）\n", el.Truncate(time.Second), rate)
	fmt.Fprintf(w, "产出页数    %d（累计含续爬 %d）\n", pages, c.st.Saved())
	fmt.Fprintf(w, "覆盖域名    %d（已收尾 %d）\n", len(c.sites), atomic.LoadInt64(&c.stats.sitesFlushed))
	fmt.Fprintf(w, "其中正文页  %d\n", atomic.LoadInt64(&c.stats.articles))
	fmt.Fprintf(w, "下载字节    %.1f MB\n", float64(atomic.LoadInt64(&c.stats.bytes))/(1<<20))
	fmt.Fprintf(w, "抽取为空    %d\n", atomic.LoadInt64(&c.stats.empty))
	fmt.Fprintf(w, "抓取失败    %d\n", atomic.LoadInt64(&c.stats.failed))
	fmt.Fprintf(w, "robots 拒绝 %d\n", atomic.LoadInt64(&c.stats.robotsBlocked))
	fmt.Fprintf(w, "Crawl-delay 过长放弃 %d\n", atomic.LoadInt64(&c.stats.delayBlocked))
	fmt.Fprintf(w, "连续失败放弃的域 %d\n", atomic.LoadInt64(&c.stats.hostAbandoned))
	fmt.Fprintf(w, "已够 %d 篇而收手的域 %d（省下 %d 次请求）\n",
		c.cfg.perHost, atomic.LoadInt64(&c.stats.targetMet), atomic.LoadInt64(&c.stats.dropped))
	fmt.Fprintf(w, "续爬跳过    %d\n", atomic.LoadInt64(&c.stats.skipped))
	fmt.Fprintf(w, "正文链接密度 %.4f（越低越干净）\n", linkRate)
	fmt.Fprintf(w, "总页面字符  %d，正文占比 %.2f\n",
		atomic.LoadInt64(&c.stats.pageChars),
		ratio(atomic.LoadInt64(&c.stats.contentChars), atomic.LoadInt64(&c.stats.pageChars)))
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// normalizeSeeds 把种子文件里的一行（域名或 URL）统一成可抓的 URL。
func normalizeSeeds(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if !strings.Contains(s, "://") {
			s = "http://" + s
		}
		out = append(out, s)
	}
	return out
}

func readSeeds(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out, sc.Err()
}
