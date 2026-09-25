// Command crawler 是并发爬虫：从种子域名出发，每域抓取若干篇正文页并落盘。
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
	ua         string
}

func main() {
	var c config
	flag.StringVar(&c.dataDir, "data", "data", "数据目录：原始 HTML 与 manifest 落在这里")
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
	flag.StringVar(&c.ua, "ua", fetch.DefaultUA, "User-Agent（单一、诚实，不轮换）")
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
		cfg:     c,
		client:  fetch.New(c.ua, c.timeout, c.maxBytes),
		fr:      frontier.New(c.delay, c.budget, c.workers),
		st:      st,
		ext:     extract.New(extract.Options{}),
		learner: extract.NewChromeLearner(),
		robots:  make(map[string]*fetch.Robots),
		sites:   make(map[string]*siteState),
		start:   time.Now(),
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

	var wg sync.WaitGroup
	for i := 0; i < c.workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); cr.work(ctx) }()
	}

	progressDone := make(chan struct{})
	go cr.report(progressDone)

	wg.Wait()
	close(progressDone)
	if err := st.Flush(); err != nil {
		return err
	}
	cr.printFinal(os.Stderr)
	return ctx.Err()
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
	stats  stats
}

type siteState struct {
	fetched  int  // 本域已抓页数
	articles int  // 本域已确认的正文页数
	seeded   bool // 首页是否已处理过（漏斗只跑一次）
}

type stats struct {
	pages, articles, empty, failed, skipped, robotsBlocked int64
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
		c.fr.Done(t.Host)
	}()

	if c.cfg.maxPages > 0 && atomic.LoadInt64(&c.stats.pages) >= int64(c.cfg.maxPages) {
		return
	}
	if c.st.Has(t.URL) {
		atomic.AddInt64(&c.stats.skipped, 1)
		return
	}

	if ok, path := c.allowed(ctx, t.Host, t.URL); !ok {
		atomic.AddInt64(&c.stats.robotsBlocked, 1)
		fmt.Fprintf(os.Stderr, "robots 禁止，跳过: %s%s\n", t.Host, path)
		return
	}

	res, err := c.client.Get(ctx, t.URL)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		atomic.AddInt64(&c.stats.failed, 1)
		fmt.Fprintf(os.Stderr, "抓取失败 %s: %v\n", t.URL, err)
		return
	}

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
		ElapsedMS: res.Elapsed.Milliseconds(),
		FetchedAt: time.Now(),
	}
	if exErr == nil {
		entry.Title, entry.RealTitle, entry.Chars = out.MetaTitle, out.RealTitle, out.ContentChars
	}
	if err := c.st.Save(entry, res.HTML); err != nil {
		fmt.Fprintf(os.Stderr, "落盘失败 %s: %v\n", t.URL, err)
	}

	isArticle := exErr == nil &&
		out.ContentChars >= c.cfg.minContent &&
		out.LinkRate <= c.cfg.maxLinkRt

	atomic.AddInt64(&c.stats.pages, 1)
	atomic.AddInt64(&c.stats.bytes, int64(res.Bytes))
	if exErr == nil {
		atomic.AddInt64(&c.stats.contentChars, int64(out.ContentChars))
		atomic.AddInt64(&c.stats.linkChars, int64(out.LinkChars))
		atomic.AddInt64(&c.stats.pageChars, int64(out.PageChars))
		if out.MainContent == "" {
			atomic.AddInt64(&c.stats.empty, 1)
		}
	}
	if isArticle {
		atomic.AddInt64(&c.stats.articles, 1)
	}

	// 漏斗：每个域只在首页跑一次发现，其余页面只计数并继续爬已排队的候选。
	s := c.site(t.Host)
	s.fetched++
	if isArticle {
		s.articles++
	}
	if !s.seeded {
		s.seeded = true
		discovered = c.funnel(t.Host, t.URL, res.HTML, s)
	} else if isArticle && s.articles >= c.cfg.perHost {
		// 配额已满，不再追加候选。已在队列里的照抓（有 budget 兜底）。
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

// allowed 查 robots.txt。结果按域缓存；首次访问该域时才真正拉取。
//
// 与真实抓取共享同一条串行路径：这个请求也发生在该域独占 worker 的期间，
// 所以不会绕过每域限速。
func (c *crawler) allowed(ctx context.Context, host, rawURL string) (bool, string) {
	rb := c.robotsFor(ctx, host)
	if rb == nil {
		return true, ""
	}
	if d := rb.Delay(); d > 0 {
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
	scheme := "http"
	res, err := c.client.Get(ctx, scheme+"://"+host+"/robots.txt")
	if err != nil || res.StatusCode != 200 {
		res, err = c.client.Get(ctx, "https://"+host+"/robots.txt")
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
	fmt.Fprintf(w, "[%s] pages=%d articles=%d hosts=%d queue=%d inflight=%d %.1f pages/s 失败=%d 跳过=%d\n",
		time.Now().Format("15:04:05"), pages, atomic.LoadInt64(&c.stats.articles),
		hosts, queued, inflight, rate,
		atomic.LoadInt64(&c.stats.failed), atomic.LoadInt64(&c.stats.skipped))
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
	fmt.Fprintf(w, "落盘页数    %d（累计含续爬 %d）\n", pages, c.st.Saved())
	fmt.Fprintf(w, "其中正文页  %d\n", atomic.LoadInt64(&c.stats.articles))
	fmt.Fprintf(w, "下载字节    %.1f MB\n", float64(atomic.LoadInt64(&c.stats.bytes))/(1<<20))
	fmt.Fprintf(w, "抽取为空    %d\n", atomic.LoadInt64(&c.stats.empty))
	fmt.Fprintf(w, "抓取失败    %d\n", atomic.LoadInt64(&c.stats.failed))
	fmt.Fprintf(w, "robots 拒绝 %d\n", atomic.LoadInt64(&c.stats.robotsBlocked))
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
