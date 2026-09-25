package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 基准与回归都用 testdata 里的**真实页面**，不用构造出来的 HTML。
//
// 抽取的耗时几乎全在 HTML 解析与 DOM 遍历上，而真实的新闻页有大量
// 广告位、导航树、内联样式和畸形标签；构造的页面结构太规整，
// 量出来的吞吐会明显偏乐观，写进 README 就是虚假数字。
func loadFixture(tb testing.TB, name string) (string, string) {
	tb.Helper()
	p := filepath.Join("testdata", name)
	raw, err := os.ReadFile(p)
	if err != nil {
		tb.Fatalf("读 %s: %v（基准依赖 testdata 里的真实页面）", p, err)
	}
	return "https://www.gov.cn/zhengce/jiedu/202609/content_1.htm", string(raw)
}

// fixtures 是基准用的真实页面。两个体量差十倍，单看一个数字会误导：
// 抽取耗时几乎全在建 DOM 上，与页面字节数近似线性。
var fixtures = []string{"news_1.html", "news_2.html"}

func BenchmarkExtract(b *testing.B) {
	e := New(Options{})
	for _, fx := range fixtures {
		b.Run(fx, func(b *testing.B) {
			u, page := loadFixture(b, fx)
			b.SetBytes(int64(len(page)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := e.ExtractWithChrome(u, page, Chrome{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkExtractParallel 量的是吞吐上限：爬虫侧是多 worker 并发的，
// 单核数字不能直接换算成整机 pages/sec，必须实测并行扩展性。
func BenchmarkExtractParallel(b *testing.B) {
	e := New(Options{})
	for _, fx := range fixtures {
		b.Run(fx, func(b *testing.B) {
			u, page := loadFixture(b, fx)
			b.SetBytes(int64(len(page)))
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := e.ExtractWithChrome(u, page, Chrome{}); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// TestExtractRealPages 用真实页面做端到端回归。
//
// 前面的单测都是为某条规则构造的最小用例，它们能保证规则本身没写错，
// 但保证不了「规则合起来在真页面上还work」——这一条补的就是这个缺口。
func TestExtractRealPages(t *testing.T) {
	cases := []struct {
		fixture   string
		wantTitle string
	}{
		{"news_1.html", "图表：2026年前8个月国家铁路发送货物26.9亿吨"},
		{"news_2.html", ""},
	}
	for _, c := range cases {
		u, page := loadFixture(t, c.fixture)
		res, err := New(Options{}).ExtractWithChrome(u, page, Chrome{})
		if err != nil {
			t.Fatalf("%s: %v", c.fixture, err)
		}

		if res.MetaTitle == "" {
			t.Errorf("%s: meta_title 为空", c.fixture)
		}
		if res.RealTitle == "" {
			t.Errorf("%s: real_title 为空", c.fixture)
		}
		// 站点后缀必须已被剥掉——这是 real_title 之所以叫「真的标题」的原因
		if strings.Contains(res.RealTitle, "中国政府网") {
			t.Errorf("%s: real_title 残留站点名: %q", c.fixture, res.RealTitle)
		}
		if res.ContentChars < 100 {
			t.Errorf("%s: 正文只有 %d 字，真实新闻页不该这么少", c.fixture, res.ContentChars)
		}
		if c.wantTitle != "" && res.RealTitle != c.wantTitle {
			t.Errorf("%s: real_title = %q，期望 %q", c.fixture, res.RealTitle, c.wantTitle)
		}
		t.Logf("%s: title=%q chars=%d linkRate=%.4f container=%s",
			c.fixture, res.RealTitle, res.ContentChars, res.LinkRate, res.ContainerTag)
	}
}
