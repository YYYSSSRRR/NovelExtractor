package extract

import "testing"

// TestIsPunctCoversNonCJKScripts 守住「标点判据不能只有中文一种」。
//
// 原实现列了一张 30 来个中日韩标点的表，非中文页面因此被系统性低估：
// 阿拉伯语页面真实标点率 6.7%，旧口径只数出 1.7%，punctGate 从 0.44 掉到
// 0.12，乘性得分被压掉一半——一个纯中文的判据把非中文页面判成了低质量块。
func TestIsPunctCoversNonCJKScripts(t *testing.T) {
	// 全部写成码点而不是字面量：弯引号、全角标点在不同编辑器/剪贴板之间
	// 会被静默替换成 ASCII 形近字，那样测的就不是想测的那个字符了。
	yes := []struct {
		r    rune
		desc string
	}{
		{'.', "ASCII 句点"}, {',', "ASCII 逗号"}, {'!', "ASCII 叹号"},
		{'?', "ASCII 问号"}, {';', "ASCII 分号"}, {':', "ASCII 冒号"},

		{'，', "中文逗号"}, {'。', "中文句号"}, {'！', "中文叹号"},
		{'？', "中文问号"}, {'；', "中文分号"}, {'：', "中文冒号"},
		{'、', "顿号"}, {'《', "左书名号"}, {'》', "右书名号"},
		{'“', "左弯双引号"}, {'”', "右弯双引号"},
		{'…', "省略号"}, {'—', "破折号"}, {'·', "间隔号"},

		{'،', "阿拉伯语逗号"}, {'؛', "阿拉伯语分号"}, {'؟', "阿拉伯语问号"},
		{'«', "左角引号"}, {'»', "右角引号"},
		{'¡', "倒叹号"}, {'¿', "倒问号"},

		{'～', "全角波浪号（Unicode 归 Sm，需手工补回）"},
	}
	for _, c := range yes {
		if !isPunct(c.r) {
			t.Errorf("isPunct(U+%04X %s) = false，应当为 true", c.r, c.desc)
		}
	}

	no := []rune{'a', 'Z', '0', ' ', '\n', '\t', '$', '+', '<',
		'中', '文', // 中 文
		'م', 'р', // م р
	}
	for _, r := range no {
		if isPunct(r) {
			t.Errorf("isPunct(U+%04X %q) = true，应当为 false", r, r)
		}
	}
}

// TestCountTextCountsNonASCIIPunctuation 是上面那条的量化形态：标点计数
// 直接决定 punctGate，而 punctGate 是得分里的乘性因子。
func TestCountTextCountsNonASCIIPunctuation(t *testing.T) {
	cases := []struct {
		in    string
		punct int
	}{
		{"Привет, мир! Как дела?", 3}, // , ! ?
		{"مرحبا، كيف حالك؟", 2},       // ، ؟
		{"你好，世界。", 2},                 // ， 。
		{"no punctuation here", 0},
	}
	for _, c := range cases {
		var st textStats
		countText(c.in, false, &st)
		if st.punct != c.punct {
			t.Errorf("countText(%q) punct = %d, 期望 %d", c.in, st.punct, c.punct)
		}
	}
}
