package extract

import (
	"strings"
	"testing"
)

// TestLooksLikeArticle 是判据本身的白盒测试。
//
// 之所以直接测这个纯函数而不是全靠端到端样本：端到端 HTML 里，选中哪个容器
// 由打分决定，写死一个期望值等于把「打分结果」也一并钉死，改打分就会连带
// 红一片，反而看不清这条判据本身对不对。四条判据各自独立，就各自测。
func TestLooksLikeArticle(t *testing.T) {
	// base 是一篇正常文章的画像：div 容器、链接密度低、长度足够。
	base := Result{ContainerTag: "div", LinkRate: 0.02, ContentChars: 1200}

	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{"正常文章", base, true},

		// 判据一：没有子区域在竞争里胜出，说明整页没有正文主体
		{"容器是 body", Result{ContainerTag: "body", LinkRate: 0.2, ContentChars: 2164}, false},
		{"容器是 html", Result{ContainerTag: "html", LinkRate: 0.2, ContentChars: 2164}, false},
		{"没有容器", Result{ContainerTag: "", LinkRate: 0, ContentChars: 0}, false},

		// 判据二：胜出的是导航/版式元素
		{"容器是 ul", Result{ContainerTag: "ul", LinkRate: 0.02, ContentChars: 329}, false},
		{"容器是 dd", Result{ContainerTag: "dd", LinkRate: 0, ContentChars: 130}, false},
		{"容器是 header", Result{ContainerTag: "header", LinkRate: 0.11, ContentChars: 36}, false},
		{"容器是 table", Result{ContainerTag: "table", LinkRate: 0.05, ContentChars: 900}, false},

		// 判据三：抽出来的文字以链接为主
		{"链接密度超阈值", Result{ContainerTag: "div", LinkRate: 0.62, ContentChars: 1500}, false},
		{"链接密度恰在阈值", Result{ContainerTag: "div", LinkRate: skipLinkRate, ContentChars: 1500}, true},

		// 判据四：短到不成句
		{"只有 2 个字符", Result{ContainerTag: "div", LinkRate: 0, ContentChars: 2}, false},
		{"只有 21 个字符", Result{ContainerTag: "div", LinkRate: 0, ContentChars: 21}, false},
		{"恰好到下限", Result{ContainerTag: "div", LinkRate: 0, ContentChars: minArticleChars}, true},
	}
	for _, c := range cases {
		if got := looksLikeArticle(&c.res); got != c.want {
			t.Errorf("%s: looksLikeArticle = %v, 期望 %v（%+v）", c.name, got, c.want, c.res)
		}
	}
}

// 一篇正文足够长、链接密度低、结构清晰的页面——判据必须放行。
const articleHTML = `<!DOCTYPE html><html><head><title>城市轨道交通建设的三个关键问题 - 某某新闻网</title></head>
<body>
<nav><a href="/a">首页</a><a href="/b">要闻</a><a href="/c">财经</a><a href="/d">体育</a></nav>
<div id="main">
  <h1>城市轨道交通建设的三个关键问题</h1>
  <p>第一个问题是资金。轨道交通的造价极高，一公里地下线路的投入通常在数亿元量级，单靠财政难以支撑。</p>
  <p>第二个问题是客流预测。预测偏高会让线路长期亏损，偏低又会在建成后立刻饱和，两个方向的失误代价都不小。</p>
  <p>第三个问题是沿线开发。把站点周边的土地收益纳入项目平衡，是目前多数城市正在尝试的做法，但它的前提是规划与土地政策能够协同。</p>
</div>
<footer><a href="/about">关于我们</a><a href="/contact">联系方式</a></footer>
</body></html>`

// 一个纯导航页：整页都是链接，没有任何一段连续的文字——正是过去会被
// 当成文章收进语料的那类页面。
const navOnlyHTML = `<!DOCTYPE html><html><head><title>网址导航</title></head>
<body><ul>
<li><a href="/1">新闻</a></li><li><a href="/2">视频</a></li><li><a href="/3">音乐</a></li>
<li><a href="/4">购物</a></li><li><a href="/5">地图</a></li><li><a href="/6">翻译</a></li>
<li><a href="/7">邮箱</a></li><li><a href="/8">网盘</a></li><li><a href="/9">日历</a></li>
<li><a href="/10">笔记</a></li><li><a href="/11">图库</a></li><li><a href="/12">文档</a></li>
</ul></body></html>`

// TestExtractFlagsNavOnlyPage 是判据的端到端形态：接线断了的话
// NoArticle 永远是零值 false，上面那条白盒测试照样全绿。
func TestExtractFlagsNavOnlyPage(t *testing.T) {
	e := New(Options{})

	res, err := e.Extract("http://news.example/2026/0918/a.html", articleHTML)
	if err != nil {
		t.Fatal(err)
	}
	if res.NoArticle {
		t.Errorf("正常文章被判成无正文：容器=%q 链接密度=%.3f 字符数=%d",
			res.ContainerTag, res.LinkRate, res.ContentChars)
	}
	if !strings.Contains(res.MainContent, "沿线开发") {
		t.Errorf("正文没抽到，MainContent = %q", res.MainContent)
	}

	nav, err := e.Extract("http://nav.example/", navOnlyHTML)
	if err != nil {
		t.Fatal(err)
	}
	if !nav.NoArticle {
		t.Errorf("纯导航页被判成有正文：容器=%q 链接密度=%.3f 字符数=%d\n正文前 200 字：%s",
			nav.ContainerTag, nav.LinkRate, nav.ContentChars, truncate(nav.MainContent, 200))
	}
}

// 一个**带摘要的列表页**：每条新闻都有标题链接 + 一段描述。描述文字让整块
// 的链接密度降到 0.21，所以爬虫原有的 `链接密度 ≤ 0.3` 拦不住它；字符数
// 276 也够长。过去的实现会把这样一个列表页当成一篇文章收进语料。
const listingHTML = `<!DOCTYPE html><html><head><title>要闻列表 - 某某网</title></head><body>
<div><h1>要闻</h1></div>
<ul>
<li><a href="/n/1">城市轨道交通建设的三个关键问题</a><p>轨道交通的造价极高，一公里地下线路的投入通常在数亿元量级，单靠财政难以支撑，需要多渠道筹资。</p></li>
<li><a href="/n/2">新能源汽车下乡政策延续</a><p>本轮政策覆盖的车型范围进一步扩大，充电设施建设被列为配套考核指标，地方财政给予一定补贴。</p></li>
<li><a href="/n/3">多地调整住房公积金贷款额度</a><p>调整后单职工最高可贷额度普遍上浮，部分城市同时放宽了缴存年限的要求，具体以各地公告为准。</p></li>
<li><a href="/n/4">秋粮收购价格保持稳定</a><p>主产区收购进度快于去年同期，仓储与烘干能力充足，市场价格总体平稳，农户售粮意愿较强。</p></li>
<li><a href="/n/5">铁路部门加开旅客列车</a><p>假期运输方案已经公布，热门方向加开夜间高铁，旅客可通过官方渠道查询余票并及时改签。</p></li>
</ul></body></html>`

// TestListingPageIsNotAnArticle 覆盖的是这条判据真正想拦的那类页面：
// 不是「抽不出东西」，而是「抽出 276 个字符、链接密度只有 0.21」——
// 单独看每个指标都像一个正常页面，只有「胜出的容器是 <ul>」暴露了
// 它是一份列表而不是一篇文章。
//
// 同时钉住契约：判成无正文后 MainContent 必须是空的。留着样板文字、指望
// 调用方自己去看 NoArticle，等于把「别把菜单当文章」这件事外包给每一个
// 调用方，忘掉的那个就会把整页菜单收进语料。
func TestListingPageIsNotAnArticle(t *testing.T) {
	e := New(Options{})
	res, err := e.Extract("http://news.example/list/1.html", listingHTML)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NoArticle {
		t.Fatalf("带摘要的列表页未被拦下：容器=%q 字符数=%d 链接密度=%.3f",
			res.ContainerTag, res.ContentChars, res.LinkRate)
	}
	if res.MainContent != "" {
		t.Errorf("判成无正文，MainContent 却不是空的：%q", truncate(res.MainContent, 80))
	}
	if res.ContentChars != 0 {
		t.Errorf("判成无正文，ContentChars = %d，期望 0", res.ContentChars)
	}
	// 诊断依据不能跟着一起没了：排查「为什么判成无正文」全靠这几个统计量
	if res.ContainerTag == "" || res.PageChars == 0 {
		t.Errorf("统计字段被清空了，排查时无从下手：容器=%q 整页字符数=%d",
			res.ContainerTag, res.PageChars)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
