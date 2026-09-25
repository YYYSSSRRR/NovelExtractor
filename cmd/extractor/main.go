// Command extractor 从 stdin 逐行读 JSON，抽取后逐行写 JSON 到 stdout。
//
// 输入： {"url": "...", "html": "...", "title": "..."}
//   - title 是上游已有策略给出的参考值，本程序不读取它。三种标题
//     （上游 title / meta_title / real_title）在真实页面上经常互不相同，
//     以参考值为输入会形成循环依赖。
//
// 输出： {"url": "...", "meta_title": "...", "real_title": "...", "main_content": "..."}
//
// stdout 只出现 JSON，任何日志与告警一律走 stderr，保证管道可直接被下游消费。
//
// 吞吐设计：按批读取（默认 256 行）→ 批内并行抽取 → 按原序写出。
// 既拿到多核并行，又把内存占用钉在「批大小 × 单页大小」这个上界内，
// 不需要把整个输入读进内存。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	"web-extract/internal/extract"
)

type record struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	HTML  string `json:"html"`
}

type output struct {
	URL         string `json:"url"`
	MetaTitle   string `json:"meta_title"`
	RealTitle   string `json:"real_title"`
	MainContent string `json:"main_content"`
}

// stats 累计整批输入的质量指标。它同时是爬虫正文页精筛和评测脚本
// 复用的那套量，放在这里是为了不重复解析 DOM。
type stats struct {
	Pages      int
	Empty      int
	ContentLen int
	LinkChars  int
	PageChars  int
}

func main() {
	var opt extract.Options
	var batch int
	var showStats, noCrossPage bool
	flag.BoolVar(&opt.DisablePunctGate, "no-punct", false, "消融：关闭标点率因子")
	flag.BoolVar(&opt.DisableLinkPenalty, "no-link", false, "消融：关闭链接密度惩罚")
	flag.BoolVar(&opt.DisablePropagation, "no-propagate", false, "消融：关闭祖先累积（退化为选单块最高分）")
	flag.BoolVar(&opt.KeepAttribution, "keep-attribution", false, "保留「本文来自 xxx(http://...)」这类署名行")
	flag.BoolVar(&noCrossPage, "no-cross-page", false, "消融：关闭跨页站点模板学习（退化为纯单页抽取）")
	flag.IntVar(&batch, "batch", 256, "批大小：批内并行处理，决定内存上界")
	flag.BoolVar(&showStats, "stats", false, "处理结束后向 stderr 打印汇总指标")
	flag.Parse()

	if batch < 1 {
		batch = 1
	}
	var learner *extract.ChromeLearner
	if !noCrossPage {
		learner = extract.NewChromeLearner()
	}

	var st stats
	if err := run(os.Stdin, os.Stdout, extract.New(opt), learner, batch, &st); err != nil {
		fmt.Fprintf(os.Stderr, "extractor: %v\n", err)
		os.Exit(1)
	}

	if showStats {
		reportStats(os.Stderr, &st)
	}
}

func reportStats(w io.Writer, st *stats) {
	if st.Pages == 0 {
		return
	}
	avg := func(total int) float64 { return float64(total) / float64(st.Pages) }
	linkRate := 0.0
	if st.ContentLen > 0 {
		linkRate = float64(st.LinkChars) / float64(st.ContentLen)
	}
	fmt.Fprintf(w, "pages=%d empty=%d (%.2f%%)\n", st.Pages, st.Empty, 100*float64(st.Empty)/float64(st.Pages))
	fmt.Fprintf(w, "avg_content_chars=%.1f avg_page_chars=%.1f\n", avg(st.ContentLen), avg(st.PageChars))
	fmt.Fprintf(w, "content_link_rate=%.4f\n", linkRate)
}

func run(in io.Reader, out io.Writer, ext *extract.Extractor, learner *extract.ChromeLearner, batch int, st *stats) error {
	r := bufio.NewReaderSize(in, 1<<20)
	w := bufio.NewWriterSize(out, 1<<20)
	defer w.Flush()
	enc := json.NewEncoder(w)

	recs := make([]record, batch)
	outs := make([]item, batch)
	chroma := make([]extract.Chrome, batch)

	for {
		n, err := readBatch(r, recs)
		if n > 0 {
			processBatch(ext, learner, recs[:n], outs[:n], chroma[:n], st)
			for i := 0; i < n; i++ {
				if outs[i].out == nil {
					continue
				}
				if e := enc.Encode(outs[i].out); e != nil {
					return e
				}
			}
			if e := w.Flush(); e != nil {
				return e
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// item 把输出与抽取过程的中间统计量绑在一起。统计量要在批处理结束后
// 单线程汇总，所以不能由 worker 直接写共享计数器。
type item struct {
	out *output
	res extract.Result
}

// readBatch 读满一批或读到 EOF。返回已读条数与终止原因。
func readBatch(r *bufio.Reader, buf []record) (int, error) {
	n := 0
	for n < len(buf) {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			line = trimNewline(line)
			if len(line) > 0 {
				var rec record
				if json.Unmarshal(line, &rec) == nil {
					buf[n] = rec
					n++
				} else {
					fmt.Fprintf(os.Stderr, "skip malformed json line (%d bytes)\n", len(line))
				}
			}
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// processBatch 批内并行抽取，结果写回 outs 的对应下标，从而保持输入顺序。
//
// 批内分两遍。第一遍只扫 <title>（不建 DOM 树），把该批每个域名的标题收齐，
// 学出「域名级常量」——站点名后缀、栏目名、书名。第二遍才是完整的并行抽取。
//
// 这就是「用数据规模换泛化」在管道里的落地：同一域名下的页面互相提供上下文，
// 单看一页永远分不清「我的明星老婆免费章节(花枫年华)」是书名还是标题的一部分
// （它和章节名之间连分隔符都没有），跨页一看它每页都在，立刻暴露。
//
// 两遍都是按输入顺序确定性执行的，因此输出可复现；学习器跨批累积，页数越多越准。
func processBatch(ext *extract.Extractor, learner *extract.ChromeLearner, recs []record, outs []item, chroma []extract.Chrome, st *stats) {
	if learner != nil {
		for i := range recs {
			learner.Observe(hostOf(recs[i].URL), extract.ScanTitle(recs[i].HTML))
		}
		for i := range recs {
			chroma[i] = learner.Chrome(hostOf(recs[i].URL))
		}
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > len(recs) {
		workers = len(recs)
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i := range recs {
		wg.Add(1)
		sem <- struct{}{} // 先占坑再起协程，批内并发数被硬性限制在 workers
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			outs[i] = extractOne(ext, &recs[i], chroma[i])
		}(i)
	}
	wg.Wait()

	// 汇总在并发结束后单线程做，worker 不碰共享计数器
	for _, it := range outs {
		if it.out == nil {
			continue
		}
		st.Pages++
		if it.out.MainContent == "" {
			st.Empty++
		}
		st.ContentLen += utf8.RuneCountInString(it.out.MainContent)
		st.LinkChars += it.res.LinkChars
		st.PageChars += it.res.PageChars
	}
}

// extractOne 抽取单页。单页 panic 不能带崩整批——输入里明确允许存在
// 少量垃圾页面，一个畸形 HTML 不该让整轮任务失败。
func extractOne(ext *extract.Extractor, rec *record, ch extract.Chrome) (it item) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "panic on %s: %v\n", rec.URL, r)
			it = item{out: &output{URL: rec.URL}}
		}
	}()

	res, err := ext.ExtractWithChrome(rec.URL, rec.HTML, ch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "extract %s: %v\n", rec.URL, err)
		return item{out: &output{URL: rec.URL}}
	}
	return item{
		out: &output{
			URL:         rec.URL,
			MetaTitle:   res.MetaTitle,
			RealTitle:   res.RealTitle,
			MainContent: res.MainContent,
		},
		res: res,
	}
}

// hostOf 取 URL 的 host，作为「同一站点」的归并键。
// 解析失败返回空串，该页退化为单页模式，不影响其它页。
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
