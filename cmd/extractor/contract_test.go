package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"web-extract/internal/extract"
)

// chapterPage 是一页仿真实小说站章节页的 HTML。
//
// 结构照搬题目样例里那个站：<title> 上挂了「书名-文库-站名」三级后缀，
// 用户看到的标题在 <h1> 里，正文是一串 <p>，页脚是站点模板。
// 题目给出的期望输出是：
//
//	meta_title 第一卷 …第十章 村子的防卫力-安逸领主的愉快领地防卫-其他文库-轻小说文库
//	real_title 第一卷 …第十章 村子的防卫力
//
// 也就是 real_title 是 meta_title 的一个前缀。
const chapterPage = `<html><head><meta charset="utf-8">` +
	`<title>第一卷 用生产系魔术将无名村改造成最强要塞都市 第十章 村子的防卫力-安逸领主的愉快领地防卫-其他文库-轻小说文库</title>` +
	`</head><body>` +
	`<div id="nav">首页 | 轻小说文库 | 其他文库</div>` +
	`<h1>第一卷 用生产系魔术将无名村改造成最强要塞都市 第十章 村子的防卫力</h1>` +
	`<div id="content">` +
	`<p>「盖城墙和盖围墙一样，由艾斯帕达主导。毕竟是第二次了，应该能毫不犹豫地进行吧。规模比较大，所以慢慢来小心不要受伤。戴伊等人负责调度建材，麻烦力气大的人帮忙了。奥尔特先生你们要做什么？」</p>` +
	`<p>「我们去砍树。反正闲着也是闲着。」奥尔特如此回答，带着几个年轻人往森林的方向走去。</p>` +
	`<p>村子的防卫力在一点点地提升，所有人都能感觉到这件事。</p>` +
	`</div>` +
	`<div id="footer">版权所有 © 轻小说文库 京ICP备00000000号</div>` +
	`</body></html>`

// TestContractOneLinePerLine 锁住输出与输入逐行对齐。
//
// 这是与外部系统之间的接口约定，不是内部实现细节：题目要求「从 stdin 按行
// 读 json、在 stdout 打印一行 json」，按下标把输入第 i 行与输出第 i 行配对
// 是最自然的消费方式。任何一行被丢掉，从那一行起配对全错，而且错得很安静
// ——下游只会发现内容对不上，不会知道是行数变了。
//
// 三种输入都在这里：正常页、坏 JSON、空 HTML。坏 JSON 必须占一格。
func TestContractOneLinePerLine(t *testing.T) {
	in := strings.Join([]string{
		`{"url":"https://a.example.com/1.htm","html":` + jsonString(chapterPage) + `,"title":"参考值"}`,
		`这行不是 JSON`,
		`{"url":"https://a.example.com/2.htm","html":"","title":""}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := run(strings.NewReader(in), &out, extract.New(extract.Options{}), nil, 16, &stats{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	lines := splitNonEmpty(out.String())
	if len(lines) != 3 {
		t.Fatalf("输入 3 行，输出 %d 行——行数必须一致", len(lines))
	}

	// 每一行都必须是合法 JSON，且字段恰好是题目要求的那四个
	want := []string{"url", "meta_title", "real_title", "main_content"}
	for i, l := range lines {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v", i+1, err)
		}
		if len(m) != len(want) {
			t.Fatalf("第 %d 行有 %d 个字段，题目要求恰好 %d 个: %v", i+1, len(m), len(want), m)
		}
		for _, k := range want {
			if _, ok := m[k]; !ok {
				t.Errorf("第 %d 行缺字段 %q", i+1, k)
			}
		}
	}

	// 第 2 行是坏 JSON，占位行的 url 必须为空——不能假装它是某一页
	var placeholder output
	if err := json.Unmarshal([]byte(lines[1]), &placeholder); err != nil {
		t.Fatalf("占位行解析失败: %v", err)
	}
	if placeholder.URL != "" || placeholder.MainContent != "" {
		t.Errorf("坏 JSON 的占位行应四个字段全空，得到 %+v", placeholder)
	}

	// 第 1 行是题目样例的形状，real_title 必须是 meta_title 的前缀
	var page output
	if err := json.Unmarshal([]byte(lines[0]), &page); err != nil {
		t.Fatalf("第 1 行解析失败: %v", err)
	}
	if !strings.HasPrefix(page.MetaTitle, page.RealTitle) {
		t.Errorf("real_title 不是 meta_title 的前缀\n  meta=%q\n  real=%q", page.MetaTitle, page.RealTitle)
	}
	if page.RealTitle == "" || page.MainContent == "" {
		t.Errorf("样例页应同时抽出标题与正文，得到 real=%q content=%d 字",
			page.RealTitle, len(page.MainContent))
	}
}

// TestContractCrossPageTitleWithoutH1 锁住跨页标题学习这条泛化路径。
//
// 上面那页有 <h1>，答对不算难。真实小说站的章节页常常**没有 h1**，
// 只有 <title>，且后缀链是站点模板——这时唯一能依据的就是「同一域名下
// 多页共有的那一段」。这里给 4 页同域、无 h1 的输入，断言每页都只留下
// 各自变化的章节名。
//
// 这条断言直接对应进阶题的目的：「希望策略能泛化，对互联网上所有网页
// 都能解析」。靠 h1 是单页特征，靠跨页统计才是规模换来的泛化。
func TestContractCrossPageTitleWithoutH1(t *testing.T) {
	chapters := []string{"第一章 序章", "第二章 出发", "第三章 相遇", "第四章 村子的防卫力"}
	var sb strings.Builder
	for i, ch := range chapters {
		html := `<html><head><meta charset="utf-8"><title>` + ch +
			`-安逸领主的愉快领地防卫-其他文库-轻小说文库</title></head><body>` +
			`<div id="content"><p>` + strings.Repeat("正文段落甲。", 30) +
			`</p><p>` + strings.Repeat("正文段落乙。", 30) + `</p></div>` +
			`<div id="footer">轻小说文库 版权所有</div></body></html>`
		sb.WriteString(`{"url":"https://novel.example.com/book/1/` + string(rune('0'+i)) + `.htm","html":` +
			jsonString(html) + `,"title":""}` + "\n")
	}

	var out bytes.Buffer
	if err := run(strings.NewReader(sb.String()), &out, extract.New(extract.Options{}),
		extract.NewChromeLearner(), 16, &stats{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	lines := splitNonEmpty(out.String())
	if len(lines) != len(chapters) {
		t.Fatalf("输入 %d 行，输出 %d 行", len(chapters), len(lines))
	}
	for i, l := range lines {
		var got output
		if err := json.Unmarshal([]byte(l), &got); err != nil {
			t.Fatalf("第 %d 行解析失败: %v", i+1, err)
		}
		if got.RealTitle != chapters[i] {
			t.Errorf("第 %d 页 real_title = %q，期望 %q\n  （meta_title=%q）",
				i+1, got.RealTitle, chapters[i], got.MetaTitle)
		}
	}
}

// jsonString 把一个 Go 字符串编码成 JSON 字符串字面量（含两端的引号）。
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // 测试里的常量，编码不了就是测试自己写错了
	}
	return string(b)
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
