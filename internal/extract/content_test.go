package extract

import (
	"strings"
	"testing"
)

// TestInlineLinkTextKept 锁定输出阶段的这条边界：块级层面拦住导航/相关阅读，
// 行内层面不能拦——<a> 自身的链接密度天然是 1，按同一规则挡掉就会把正文段落
// 里的内联链接连文字一起删掉，句子从中间断掉。
func TestInlineLinkTextKept(t *testing.T) {
	const page = `<html><head><title>内联链接 - 示例站</title></head><body>
	<div id="main">
	  <p>这是一段正常的正文内容，包含足够多的字符来通过长度阈值判断，并且有标点符号。这里还有一个<a href="http://x.com">内联链接</a>混在正文中间，它应该被保留而不是让句子断掉。</p>
	  <p>第二段正文继续补充内容，让整个容器的文本密度足够高，从而被选为正文容器。再多写一些字确保长度因子拿满分。</p>
	</div></body></html>`

	res, err := New(Options{}).ExtractWithChrome("http://x.com/a/1.html", page, Chrome{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.MainContent, "内联链接") {
		t.Errorf("内联链接文字被删掉，正文出现断句：\n%s", res.MainContent)
	}
	if res.LinkChars == 0 {
		t.Error("LinkChars 为 0：内联链接字符没被计入，链接密度指标会恒为 0")
	}
	if res.LinkRate <= 0 {
		t.Errorf("LinkRate = %v，期望 > 0", res.LinkRate)
	}
}

// TestLinkOnlyBlockDropped 验证整块都是链接的段落仍会被丢弃——判据落在块级
// 元素自身的链接密度上，是上一条用例的另一半。
func TestLinkOnlyBlockDropped(t *testing.T) {
	const page = `<html><head><title>链接块 - 示例站</title></head><body>
	<div id="main">
	  <p>正文段落一，写得足够长以便通过长度阈值，并且带上标点符号让标点率达标，这样容器才会被选中。</p>
	  <p>正文段落二，同样要写得足够长，确保这个容器的文本密度明显高于周围的其他容器，从而胜出。</p>
	  <p><a href="http://x.com/1">相关阅读一</a><a href="http://x.com/2">相关阅读二</a><a href="http://x.com/3">相关阅读三</a></p>
	</div></body></html>`

	res, err := New(Options{}).ExtractWithChrome("http://x.com/a/2.html", page, Chrome{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.MainContent, "相关阅读") {
		t.Errorf("纯链接段落应被丢弃，实际混进了正文：\n%s", res.MainContent)
	}
	if !strings.Contains(res.MainContent, "正文段落一") {
		t.Errorf("正文段落丢失：\n%s", res.MainContent)
	}
}
