package extract

import (
	"fmt"
	"testing"
)

// benchTitles 造一批拟真标题：书名 + 章节名 + 栏目 + 站名，四段结构，
// 站点模板成分（书名、栏目、站名）固定，只有章节名逐页变化。
var benchTitles = func() []string {
	chaps := []string{"风起", "夜行", "断刃", "孤城", "寒江", "归途", "残阳", "旧约"}
	out := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		out = append(out, fmt.Sprintf("万古神帝_第%d章 %s_玄幻奇幻_手机小说",
			i+1, chaps[i%len(chaps)]))
	}
	return out
}()

// BenchmarkChromeLearn 模拟 100 个域名 × 100 页的完整学习过程。
// 这是爬虫每域抓满配额时的真实调用量。
func BenchmarkChromeLearn(b *testing.B) {
	hosts := make([]string, 100)
	for d := range hosts {
		hosts[d] = fmt.Sprintf("site%03d.example.com", d)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := NewChromeLearner()
		for _, h := range hosts {
			for _, t := range benchTitles {
				l.Observe(h, t)
			}
		}
		for _, h := range hosts {
			_ = l.Chrome(h)
		}
	}
}

// BenchmarkScanTitle 量第一遍 tokenizer 扫描的成本，它是每页都要付的。
func BenchmarkScanTitle(b *testing.B) {
	page := []byte("<html><head><title>万古神帝_第32章 断刃_玄幻奇幻_手机小说</title>" +
		"<meta charset=\"utf-8\"></head><body><div><p>正文</p></div></body></html>")
	s := string(page)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ScanTitle(s)
	}
}
