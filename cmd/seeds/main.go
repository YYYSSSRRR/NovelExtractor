// Command seeds 汇总种子域名：多来源并联 → 去重 → 探可达性 → 产出种子表。
//
// 种子表是整条流水线的入口，它的质量直接决定爬取产出。把死站、域名停放页、
// 纯 JS 空壳放进队列，等于白烧抓取预算，所以入表前必须过一道可达性闸门。
//
//	go run ./cmd/seeds -jsonl ../novel-extractor/data/novel.json \
//	                   -extra seeds/manual.txt -harvest seeds/directories.txt \
//	                   -probe -out seeds/seeds.txt
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
	"unicode"

	"web-extract/internal/discover"
	"web-extract/internal/fetch"
	"web-extract/internal/seed"
)

func main() {
	var (
		jsonl    = flag.String("jsonl", "", "从 {url,...} JSONL 里提取域名（基础题数据集，最廉价的一批）")
		extra    = flag.String("extra", "", "人工兜底清单，一行一个域名/URL")
		harvest  = flag.String("harvest", "", "目录站 URL 清单，站内翻一层、站外只扩散一层")
		sections = flag.Int("sections", 8, "每个目录站最多再翻几个栏目页")
		probe    = flag.Bool("probe", false, "入表前探测可达性（强烈建议开启）")
		workers  = flag.Int("workers", 32, "探测并发数")
		timeout  = flag.Duration("timeout", 6*time.Second, "单次请求超时（探测阶段的主要成本，压短可显著提速）")
		minCJK   = flag.Int("min-cjk", 50, "首页至少含多少个中日韩字符才算「活的」")
		ua       = flag.String("ua", fetch.DefaultUA, "User-Agent")
		out      = flag.String("out", "", "输出种子表路径（留空则打到 stdout）")
	)
	flag.Parse()

	if *jsonl == "" && *extra == "" && *harvest == "" {
		fmt.Fprintln(os.Stderr, "至少指定 -jsonl / -extra / -harvest 之一")
		flag.Usage()
		os.Exit(2)
	}

	src := map[string][]string{}

	if *jsonl != "" {
		hosts, err := readAnd(func(f *os.File) ([]string, error) { return seed.FromJSONL(f) }, *jsonl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读 %s: %v\n", *jsonl, err)
			os.Exit(1)
		}
		src["数据集"] = hosts
		fmt.Fprintf(os.Stderr, "数据集 %-40s → %d 个域名\n", *jsonl, len(hosts))
	}

	if *extra != "" {
		hosts, err := readAnd(func(f *os.File) ([]string, error) { return seed.FromLines(f) }, *extra)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读 %s: %v\n", *extra, err)
			os.Exit(1)
		}
		src["人工清单"] = hosts
		fmt.Fprintf(os.Stderr, "人工清单 %-38s → %d 个域名\n", *extra, len(hosts))
	}

	client := fetch.New(*ua, *timeout, 4<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *harvest != "" {
		dirs, err := readAnd(func(f *os.File) ([]string, error) { return seed.FromLines(f) }, *harvest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读 %s: %v\n", *harvest, err)
			os.Exit(1)
		}
		// 目录站自身是种子，它链出去的站也是种子——两层都收
		got := harvestDirs(ctx, client, dirs, *workers, *sections, time.Second)
		src["目录扩散"] = got
		fmt.Fprintf(os.Stderr, "目录扩散 %d 个目录页               → %d 个域名\n", len(dirs), len(got))
	}

	// 各来源合并去重
	var all []string
	for _, v := range src {
		all = append(all, v...)
	}
	all = seed.Dedupe(all)
	fmt.Fprintf(os.Stderr, "\n合并去重后 %d 个域名\n", len(all))

	if *probe {
		// 探测用独立的客户端：关掉重试。探测只关心「活着没有」，重试既不改
		// 结论，又让每个死域名付出 (retries+1)×timeout 的代价——整批探测的
		// 墙钟时间直接翻三倍，实测正是它把一次种子构建拖到十几分钟。
		probeClient := fetch.New(*ua, *timeout, 1<<20).SetRetries(0)
		alive, dead := probeHosts(ctx, probeClient, all, *workers, *minCJK)
		fmt.Fprintf(os.Stderr, "可达性探测：存活 %d，剔除 %d（死站/停放页/空壳）\n", len(alive), len(dead))
		all = alive
	}

	if *out == "" {
		for _, h := range all {
			fmt.Println(h)
		}
		return
	}
	if err := writeLines(*out, all); err != nil {
		fmt.Fprintf(os.Stderr, "写 %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "已写出 %s（%d 行）\n", *out, len(all))
}

// harvestDirs 从每个目录站收集站外域名，站内多走一层、站外只扩散一层。
func harvestDirs(ctx context.Context, c *fetch.Client, dirs []string, workers, sections int, delay time.Duration) []string {
	sem := make(chan struct{}, workers)
	results := make([][]string, len(dirs))
	var wg sync.WaitGroup

	for i, d := range dirs {
		wg.Add(1)
		go func(i int, d string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			got := harvestSite(ctx, c, d, sections, delay)
			results[i] = got
			fmt.Fprintf(os.Stderr, "  %-24s → %4d 个站外域名\n", d, len(got))
		}(i, d)
	}
	wg.Wait()

	var out []string
	for _, r := range results {
		out = append(out, r...)
	}
	return seed.Dedupe(out)
}

// harvestSite 在一个目录站内部先走一层，再收集站外域名。
//
// 只抓首页远远不够：目录站的价值几乎全在分类页上，首页往往只链出去几十个站，
// 一个分类页却能链几百个。所以站内要翻一层（首页 → 若干栏目页）。
//
// 注意「只扩散一层」约束的是**跨域跳转**，不是站内翻页——顺着目录站的站内
// 链接多翻几页仍然停在这一个站上，不会漂走；真正会让人漂走的是拿捞回来的
// 域名当新的目录站再捞一轮，那一步绝不做。
func harvestSite(ctx context.Context, c *fetch.Client, host string, sections int, delay time.Duration) []string {
	res, err := c.Get(ctx, "https://"+host)
	if err != nil || res.StatusCode != 200 {
		// 老站仍有不少只开 80 端口
		if res, err = c.Get(ctx, "http://"+host); err != nil || res.StatusCode != 200 {
			return nil
		}
	}

	out := seed.HarvestHosts(res.FinalURL, res.HTML)

	links := seed.PickSections(discover.Links(res.FinalURL, res.HTML), sections)
	for _, l := range links {
		select {
		case <-ctx.Done():
			return out
		case <-time.After(delay): // 对同一个站保持礼貌间隔
		}
		r, err := c.Get(ctx, l)
		if err != nil || r.StatusCode != 200 {
			continue
		}
		out = append(out, seed.HarvestHosts(r.FinalURL, r.HTML)...)
	}
	return out
}

// probeHosts 并发探测域名是否可爬，返回存活与剔除两组。
//
// 判据是「HTTP 200 且首页含足量中日韩字符」。加字符数这一条是因为大量域名
// 已经变成停放页或纯 JS 空壳：状态码是 200，但抓回来对语料毫无价值，
// 而它们会实打实地占掉每域的抓取预算。
func probeHosts(ctx context.Context, c *fetch.Client, hosts []string, workers, minCJK int) (alive, dead []string) {
	type verdict struct {
		host string
		ok   bool
	}
	ch := make(chan verdict, len(hosts))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for _, h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res, err := c.Get(ctx, "https://"+h)
			if err != nil || res.StatusCode != 200 {
				// https 失败的再试一次 http：老站仍有不少只开 80 端口
				if res, err = c.Get(ctx, "http://"+h); err != nil || res.StatusCode != 200 {
					ch <- verdict{h, false}
					return
				}
			}
			ch <- verdict{h, countCJK(res.HTML) >= minCJK}
		}(h)
	}
	go func() { wg.Wait(); close(ch) }()

	// 边收边报进度：探测是整条命令里最慢的一步，不打印的话长时间没有任何
	// 输出，从外面看和卡死没有区别。
	done := 0
	for v := range ch {
		done++
		if v.ok {
			alive = append(alive, v.host)
		} else {
			dead = append(dead, v.host)
		}
		if done%100 == 0 || done == len(hosts) {
			fmt.Fprintf(os.Stderr, "  探测进度 %d/%d（存活 %d）\n", done, len(hosts), len(alive))
		}
	}
	sort.Strings(alive)
	sort.Strings(dead)
	return alive, dead
}

// countCJK 数中日韩字符。用码点区间判断而不是 regexp：这里对每个首页全量扫一遍，
// 正则的编译与回溯成本不值得。
func countCJK(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Hangul, r) {
			n++
		}
	}
	return n
}

func readAnd(fn func(*os.File) ([]string, error), path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return fn(f)
}

func writeLines(path string, lines []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			return err
		}
	}
	return nil
}
