package extract

import (
	"strings"
	"testing"
)

// 模板过滤是一个统计判据，测的是「哪些字符串被判为模板」，用构造语料
// 反而比真实页面更清楚：真实页面里正文千差万别，断言只能写得含糊。
// 这里刻意用真实站点上常见的模板形态（版权尾巴、责任编辑署名、字号提示），
// 但正文内容自造，好让「该剥的剥了、不该剥的没动」逐条可断言。
func TestLearnTemplates(t *testing.T) {
	contents := []string{
		"昨夜的雨下到天亮才停，院子里的石板路积了一层水。\n他起得很早，把炉子生上，烧了一壶水。\n本文由某某网编辑整理，转载请注明出处。",
		"试验田里的稻子抽穗了，比去年早了差不多一周。\n农技站的人说，这跟入夏以来的积温有关。\n本文由某某网编辑整理，转载请注明出处。",
		"比赛进行到第八十分钟，比分还是零比零。\n替补席上的人都站了起来，盯着场上。\n本文由某某网编辑整理，转载请注明出处。",
	}
	tm := LearnTemplates(contents)
	if tm == nil {
		t.Fatal("三页共现的模板应当被学到")
	}

	got := tm.Strip(contents[0])
	if strings.Contains(got, "转载请注明出处") {
		t.Errorf("模板片段未被剥掉: %q", got)
	}
	if !strings.Contains(got, "院子里的石板路积了一层水") {
		t.Errorf("正文被误伤: %q", got)
	}

	// 只在一页里出现的句子绝不能被当成模板——这是文档频率与词频的分界
	solo := "第四篇文章的正文。\n这里有一句只在本文出现的话，它不属于任何模板。"
	if out := tm.Strip(solo); !strings.Contains(out, "只在本文出现的话") {
		t.Errorf("单页独有的句子被误判为模板: %q", out)
	}
}

func TestLearnTemplatesNeedsEnoughPages(t *testing.T) {
	// 两页共现不构成统计：两页里的「过半」等于「两页都有」，巧合概率太高
	if tm := LearnTemplates([]string{"一样的文本内容", "一样的文本内容"}); tm != nil {
		t.Error("页数不足时不应当下判断")
	}
}

func TestStripKeepsContentWhenMostWouldVanish(t *testing.T) {
	// 高度重复的页面（同一篇文章的多个 URL）会让整页都像模板。
	// 这种情况下安全闸必须让它原样返回，而不是交一份残缺正文。
	dup := "完全一样的一小段文字"
	tm := LearnTemplates([]string{dup, dup, dup})
	if tm == nil {
		t.Skip("该语料未学到模板")
	}
	if got := tm.Strip(dup); got != dup {
		t.Errorf("剥除过半时应当放弃剥离，得到 %q", got)
	}
}

func TestStripNilIsIdentity(t *testing.T) {
	var tm *Templates
	const s = "任意文本"
	if got := tm.Strip(s); got != s {
		t.Errorf("nil Templates 应当是恒等变换，得到 %q", got)
	}
}
