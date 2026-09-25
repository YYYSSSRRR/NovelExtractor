// Command eval 是评测台：现场抓一小批真实页面，让多个抽取器跑**同一批**页面，
// 把对比数字打成表。
//
// 为什么是「现场抓」而不是「读爬取产物」：爬虫只留抽取结果、不留原始 HTML
// （见 internal/store 的说明）。这让重抽变得不可能，但评测需要的恰恰是
// 「同一批输入喂给不同实现」——那就把输入放在内存里，从抓取到出表一次跑完，
// 全程不落盘。样本量在几百页量级，内存完全吃得下。
//
// 参与对比的有两类：
//
//	本实现的不同配置  完整版 / 关掉标点率 / 关掉链接惩罚 / 关掉祖先累积 /
//	                  关掉跨页学习 / 关掉跨页模板过滤——这是消融实验
//	外部二进制        novel-extractor（基础题那份站点规则版），走 stdin/stdout
//	                  同一个 JSONL 接口，一行不改
//
// 指标必须**只依赖输出文本**才能公平：对照实现不会告诉我们它的链接字符数。
// 所以只取能从 main_content 与 real_title 直接算出来的量——空结果率、正文率、
// 标点密度、跨页模板残留率、标题非空率。页面可见文本长度由评测台自己用一把
// 中立的尺子量（去掉 script/style 后数非空白字符），与被测实现无关。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"web-extract/internal/discover"
	"web-extract/internal/extract"
	"web-extract/internal/fetch"

	"golang.org/x/net/html"
)

type page struct {
	URL  string
	Host string
	HTML string
	Text int // 页面可见文本的非空白字符数，评测侧的中立标尺
}

type result struct {
	URL         string `json:"url"`
	MetaTitle   string `json:"meta_title"`
	RealTitle   string `json:"real_title"`
	MainContent string `json:"main_content"`
}

func main() {
	var (
		seedsPath = flag.String("seeds", "seeds/seeds.txt", "种子表")
		domains   = flag.Int("domains", 120, "抽多少个域名（沿种子表等距取样，保证可复现）")
		perHost   = flag.Int("per-host", 3, "每个域名取几页")
		workers   = flag.Int("workers", 12, "抓取并发")
		timeout   = flag.Duration("timeout", 15*time.Second, "单次请求超时")
		delay     = flag.Duration("delay", 1200*time.Millisecond, "同域请求间隔")
		minPage   = flag.Int("min-page-chars", 400, "入样的页面至少要有这么多可见字符")
		baseline  = flag.String("baseline", "../novel-extractor/novel-extractor", "对照二进制路径，留空则跳过")
		ua        = flag.String("ua", fetch.DefaultUA, "User-Agent")
		outDir    = flag.String("out", "eval", "结果输出目录")
	)
	flag.Parse()

	ctx := context.Background()
	client := fetch.New(*ua, *timeout, 5<<20)

	hosts, err := pickHosts(*seedsPath, *domains)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读种子表: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "抽样 %d 个域名，每域至多 %d 页\n", len(hosts), *perHost)

	corpus := collect(ctx, client, hosts, *perHost, *workers, *delay, *minPage)
	if len(corpus) == 0 {
		fmt.Fprintln(os.Stderr, "没抓到任何可用页面")
		os.Exit(1)
	}
	byHost := map[string]int{}
	for _, p := range corpus {
		byHost[p.Host]++
	}
	fmt.Fprintf(os.Stderr, "样本就绪：%d 页 / %d 个域名\n\n", len(corpus), len(byHost))

	rows := []row{evalVariant("完整版（本实现）", inProc(extract.Options{}, true, true), corpus)}
	for _, a := range []struct {
		name string
		opt  extract.Options
	}{
		{"消融：关标点率", extract.Options{DisablePunctGate: true}},
		{"消融：关链接惩罚", extract.Options{DisableLinkPenalty: true}},
		{"消融：关祖先累积", extract.Options{DisablePropagation: true}},
	} {
		rows = append(rows, evalVariant(a.name, inProc(a.opt, true, true), corpus))
	}
	rows = append(rows,
		evalVariant("消融：关跨页标题学习", inProc(extract.Options{}, false, true), corpus),
		evalVariant("消融：关跨页模板过滤", inProc(extract.Options{}, true, false), corpus),
		evalVariant("单页模式（两个跨页机制全关）", inProc(extract.Options{}, false, false), corpus),
	)

	if *baseline != "" {
		if _, err := os.Stat(*baseline); err == nil {
			r, err := external(*baseline, corpus)
			if err != nil {
				fmt.Fprintf(os.Stderr, "对照二进制执行失败: %v\n", err)
			} else {
				rows = append(rows, evaluate("对照：novel-extractor（站点规则版）", r, corpus))
			}
		} else {
			fmt.Fprintf(os.Stderr, "未找到对照二进制 %s，跳过\n", *baseline)
		}
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	md := render(rows, len(corpus), len(byHost))
	if err := os.WriteFile(filepath.Join(*outDir, "report.md"), []byte(md), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if err := writeSamples(*outDir, corpus); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Print(md)
	fmt.Fprintf(os.Stderr, "\n已写出 %s/report.md\n", *outDir)
}

// pickHosts 沿种子表等距取样。等距而不是随机取前 N 个：种子表是按域名排序的，
// 取前缀会整片落在字母表靠前的站上，样本会系统性地偏向某一类站点。等距取样
// 还有一个好处——同一份种子表每次跑出来的样本完全相同，对比才可复现。
func pickHosts(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var all []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		if h := strings.TrimSpace(sc.Text()); h != "" {
			all = append(all, h)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n >= len(all) {
		return all, nil
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, all[i*len(all)/n])
	}
	return out, nil
}

// collect 抓一批样本页面。选页标准刻意保持中立：只用 URL 形态挑候选，
// 用「整页可见文本够长」做门槛，**不经过任何被测实现的正文判据**。
// 否则「哪些页面进入样本」本身就被被测实现决定了，对比从起点就不公平。
func collect(ctx context.Context, c *fetch.Client, hosts []string, perHost, workers int, delay time.Duration, minPage int) []page {
	var (
		mu     sync.Mutex
		corpus []page
		wg     sync.WaitGroup
	)
	sem := make(chan struct{}, workers)

	for _, h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var got []page
			res, err := c.Get(ctx, "http://"+h)
			if err != nil || res.StatusCode != 200 {
				if res, err = c.Get(ctx, "https://"+h); err != nil || res.StatusCode != 200 {
					return
				}
			}
			cands := discover.PickArticles(discover.Links(res.FinalURL, res.HTML), perHost+2)
			for _, cd := range cands {
				if len(got) >= perHost {
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
				r, err := c.Get(ctx, cd.URL)
				if err != nil || r.StatusCode != 200 {
					continue
				}
				if n := visibleChars(r.HTML); n >= minPage {
					got = append(got, page{URL: cd.URL, Host: h, HTML: r.HTML, Text: n})
				}
			}

			mu.Lock()
			corpus = append(corpus, got...)
			n := len(corpus)
			mu.Unlock()
			fmt.Fprintf(os.Stderr, "  %-28s +%d 页（累计 %d）\n", h, len(got), n)
		}(h)
	}
	wg.Wait()

	sort.Slice(corpus, func(i, j int) bool { return corpus[i].URL < corpus[j].URL })
	return corpus
}

// visibleChars 是评测侧那把中立的尺子：去掉 script/style/注释后，
// 数整页文本节点里的非空白字符。它不区分正文与导航，因此对任何实现都一致。
func visibleChars(htmlStr string) int {
	z := html.NewTokenizer(strings.NewReader(htmlStr))
	skip := 0
	n := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return n
		case html.StartTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "noscript":
				skip++
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "noscript":
				if skip > 0 {
					skip--
				}
			}
		case html.TextToken:
			if skip > 0 {
				continue
			}
			for _, r := range string(z.Text()) {
				if !unicode.IsSpace(r) {
					n++
				}
			}
		}
	}
}

// inProc 构造一个进程内的跑法。
//
// 跨页的两个机制（标题模板、模板残留）都是域名级的统计量，所以这里也按域名
// 分组处理——和爬虫里的收尾逻辑保持同一套语义。关掉某个机制时，用的就是
// extract 包自身的开关，不是另写一份简化实现。
func inProc(opt extract.Options, useChrome, useTemplates bool) func([]page) []result {
	return func(corpus []page) []result {
		ext := extract.New(opt)
		var learner *extract.ChromeLearner
		if useChrome {
			learner = extract.NewChromeLearner()
		}

		groups := map[string][]int{}
		var order []string
		for i, p := range corpus {
			if _, ok := groups[p.Host]; !ok {
				order = append(order, p.Host)
			}
			groups[p.Host] = append(groups[p.Host], i)
		}

		outs := make([]result, len(corpus))
		for _, h := range order {
			idxs := groups[h]
			ch := extract.Chrome{}
			if learner != nil {
				for _, i := range idxs {
					learner.Observe(h, extract.ScanTitle(corpus[i].HTML))
				}
				ch = learner.Chrome(h)
			}
			contents := make([]string, len(idxs))
			for k, i := range idxs {
				res, err := ext.ExtractWithChrome(corpus[i].URL, corpus[i].HTML, ch)
				if err != nil {
					continue
				}
				contents[k] = res.MainContent
				outs[i] = result{
					URL: corpus[i].URL, MetaTitle: res.MetaTitle,
					RealTitle: res.RealTitle, MainContent: res.MainContent,
				}
			}
			if !useTemplates {
				continue
			}
			tm := extract.LearnTemplates(contents)
			if tm == nil {
				continue
			}
			for _, i := range idxs {
				if outs[i].MainContent == "" {
					continue
				}
				outs[i].MainContent = tm.Strip(outs[i].MainContent)
			}
		}
		return outs
	}
}

// external 把同一批页面喂给外部二进制。走的是题目规定的 stdin/stdout JSONL
// 接口，不碰它的内部实现。
func external(bin string, corpus []page) ([]result, error) {
	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go func() {
		defer stdin.Close()
		enc := json.NewEncoder(stdin)
		for _, p := range corpus {
			_ = enc.Encode(map[string]string{"url": p.URL, "html": p.HTML, "title": ""})
		}
	}()

	outs := make([]result, 0, len(corpus))
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var r result
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			outs = append(outs, r)
		}
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	// 对照实现可能漏行（异常页面直接丢弃），按 URL 对齐到样本顺序，
	// 漏掉的算作空结果——这本来就是它的成绩，不该替它补。
	idx := make(map[string]result, len(outs))
	for _, r := range outs {
		idx[r.URL] = r
	}
	aligned := make([]result, len(corpus))
	for i, p := range corpus {
		aligned[i] = idx[p.URL]
		aligned[i].URL = p.URL
	}
	return aligned, nil
}

// row 是一行对比结果。
type row struct {
	Name       string
	Empty      float64 // 空结果率
	Coverage   float64 // 正文率：抽出长度 / 页面可见文本长度
	Punct      float64 // 抽出文本的标点密度
	Residue    float64 // 跨页模板残留率
	TitleOK    float64 // real_title 非空率
	AvgChars   float64 // 平均正文长度
	NoNewlines float64 // 输出里换行缺失的比例（正文被压成一行的征兆）
}

// evalVariant 跑一个进程内的配置并计分。
func evalVariant(name string, run func([]page) []result, corpus []page) row {
	return evaluate(name, run(corpus), corpus)
}

// evaluate 对一批输出计分。入参是**结果**而不是跑法，因为对照实现的结果
// 来自另一个进程，两类跑法要在同一处汇合，指标口径才不可能分叉。
func evaluate(name string, outs []result, corpus []page) row {

	groups := map[string][]int{}
	for i, p := range corpus {
		groups[p.Host] = append(groups[p.Host], i)
	}

	r := row{Name: name}
	var chars, punct, pageChars, titles, noNL int

	for i, p := range corpus {
		c := outs[i].MainContent
		n := countNonSpace(c)
		chars += n
		pageChars += p.Text
		punct += countPunct(c)
		if n == 0 {
			r.Empty++
		}
		if strings.TrimSpace(outs[i].RealTitle) != "" {
			titles++
		}
		if n > 200 && !strings.Contains(c, "\n") {
			noNL++
		}
	}

	// 模板残留率：把该域的输出当成新语料，再学一次模板，看还能剥掉多少。
	// 剥不掉说明这一版已经把跨页重复的部分清干净了；剥得掉说明还有残留。
	// 这条度量不需要任何人工标注，对两个实现也是同一把尺子。
	var residueNum, residueDen int
	for _, idxs := range groups {
		if len(idxs) < 3 {
			continue
		}
		contents := make([]string, 0, len(idxs))
		for _, i := range idxs {
			contents = append(contents, outs[i].MainContent)
		}
		before := 0
		for _, c := range contents {
			before += countNonSpace(c)
		}
		if before == 0 {
			continue
		}
		tm := extract.LearnTemplates(contents)
		if tm == nil {
			continue
		}
		after := 0
		for _, c := range contents {
			after += countNonSpace(tm.Strip(c))
		}
		residueNum += before - after
		residueDen += before
	}

	n := float64(len(corpus))
	r.Empty /= n
	r.Coverage = ratio(chars, pageChars)
	r.Punct = ratio(punct, chars)
	r.TitleOK = float64(titles) / n
	r.AvgChars = float64(chars) / n
	r.NoNewlines = float64(noNL) / n
	r.Residue = ratio(residueNum, residueDen)
	return r
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func countNonSpace(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// countPunct 数中英文标点。用与抽取器内部同一套判据的近似：ASCII 常见标点
// 加中文标点区间。这里只在输出文本上跑，不必追求与内部完全一致。
func countPunct(s string) int {
	n := 0
	for _, r := range s {
		if isPunct(r) {
			n++
		}
	}
	return n
}

func isPunct(r rune) bool {
	switch {
	case r < 128:
		return strings.ContainsRune(",.!?;:", r)
	case r >= 0x3000 && r <= 0x303F: // 中文标点与全角符号
		return true
	case r >= 0xFF00 && r <= 0xFF0F:
		return true
	case r >= 0xFF1A && r <= 0xFF1F:
		return true
	case r == 0x2018 || r == 0x2019 || r == 0x201C || r == 0x201D || r == 0x2026 || r == 0x2014:
		return true
	}
	return false
}

func render(rows []row, pages, hosts int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 泛化抽取：对比与消融实验\n\n")
	fmt.Fprintf(&b, "样本：现场抓取的真实网页 **%d 页 / %d 个域名**，"+
		"按种子表等距取样，选页只依据 URL 形态与整页可见文本长度，不经任何被测实现的判据。\n\n", pages, hosts)
	fmt.Fprintf(&b, "| 配置 | 空结果率 | 正文率 | 标点密度 | 跨页模板残留率 | 标题非空率 | 平均正文长度 | 单行输出占比 |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %.1f%% | %.1f%% | %.3f | %.2f%% | %.1f%% | %.0f | %.1f%% |\n",
			r.Name, r.Empty*100, r.Coverage*100, r.Punct, r.Residue*100,
			r.TitleOK*100, r.AvgChars, r.NoNewlines*100)
	}
	b.WriteString("\n指标口径：\n\n")
	b.WriteString("- **空结果率**：`main_content` 为空的比例。越低越好，但空不一定错——栏目页、列表页本就无正文可抽。\n")
	b.WriteString("- **正文率**：抽出文本的非空白字符数 ÷ 整页可见文本的非空白字符数。太低是漏抽，太高是把导航模板也抽了进来。\n")
	b.WriteString("- **标点密度**：抽出文本里标点字符的占比。正文是成句的，导航不是；这个值明显偏低说明抽进来的东西不成句。\n")
	b.WriteString("- **跨页模板残留率**：把同一域名的输出当成新语料再学一次模板，还能剥掉的字符占比。没人标注也能算，且对所有实现是同一把尺子。\n")
	b.WriteString("- **单行输出占比**：正文超过 200 字却一个换行都没有的比例。段落结构丢了，语料的分段信息也就没了。\n")
	return b.String()
}

// writeSamples 落一份样本清单，供人工抽检时按 URL 回看。
func writeSamples(dir string, corpus []page) error {
	var b strings.Builder
	for _, p := range corpus {
		fmt.Fprintf(&b, "%s\t%d\n", p.URL, p.Text)
	}
	return os.WriteFile(filepath.Join(dir, "sample_urls.tsv"), []byte(b.String()), 0o644)
}
