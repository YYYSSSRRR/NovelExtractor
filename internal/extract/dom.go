package extract

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// dropSubtree 列出不可能承载正文、且会污染文本统计的子树。
//
// 这些都是 HTML 规范定义的元素，不是站点模板特征——用它们不违反
// 「算法不读 class/id」的约束。判据是「这个标签在任何站点上语义都相同」，
// 而 class="ptm-content" 只在一个站点上有意义。
//
// <header> 刻意保留：不少站点把文章标题放在 <header> 里，删掉会连标题一起丢。
var dropSubtree = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true,
	atom.Iframe: true, atom.Svg: true, atom.Form: true, atom.Button: true,
	atom.Select: true, atom.Textarea: true, atom.Template: true, atom.Object: true,
	atom.Nav: true, atom.Aside: true, atom.Footer: true,
}

// attr 取属性值。HTML 解析器已把元素名与属性名统一转小写。
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// isHidden 判断元素是否被显式隐藏。用 style 属性而不是 class 名：
// display:none 是 CSS 语义，任何站点含义一致。
func isHidden(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if attr(n, "hidden") != "" {
		return true
	}
	style := attr(n, "style")
	if style == "" {
		return false
	}
	// 去掉全部空白再匹配，避免 "display : none" 这类写法漏网
	s := strings.ToLower(strings.Join(strings.Fields(style), ""))
	return strings.Contains(s, "display:none") || strings.Contains(s, "visibility:hidden")
}

// prune 就地删除噪声子树与注释。解析后立刻执行一次，
// 之后所有文本统计都不必再判断这些节点。
func prune(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling // RemoveChild 会把 c 的兄弟指针清空，先存后删
		switch {
		case c.Type == html.CommentNode:
			n.RemoveChild(c)
		case c.Type == html.ElementNode && (dropSubtree[c.DataAtom] || isHidden(c)):
			n.RemoveChild(c)
		default:
			prune(c)
		}
		c = next
	}
}

// statsOf 单次后序遍历，自底向上算出每个元素子树的文本统计量。
//
// 朴素做法是对每个候选块各扫一遍后代，复杂度 O(n·depth)；一次后序
// 遍历把每个节点的统计量算好存进 map，后续查表即可，是 O(n)。
func statsOf(root *html.Node) map[*html.Node]*textStats {
	m := make(map[*html.Node]*textStats, 256)
	var walk func(n *html.Node, inLink bool) textStats
	walk = func(n *html.Node, inLink bool) textStats {
		var st textStats
		switch n.Type {
		case html.TextNode:
			countText(n.Data, inLink, &st)
		case html.ElementNode:
			st.elems = 1
			if n.DataAtom == atom.A {
				inLink = true
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				st.add(walk(c, inLink))
			}
			cp := st // 取副本再存指针，否则所有节点会指向同一个变量
			m[n] = &cp
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				st.add(walk(c, inLink))
			}
		}
		return st
	}
	walk(root, false)
	return m
}

// firstElement 深度优先找第一个指定标签的元素。
func firstElement(n *html.Node, a atom.Atom) *html.Node {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if found != nil {
			return
		}
		if x.Type == html.ElementNode && x.DataAtom == a {
			found = x
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return found
}

// nodeText 拼出子树内的全部文本（原样，不做空白处理）。
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// elementsWithTag 收集文档中指定标签的元素，最多 max 个。
// 用于收集 <h1> 这类可能有多个、需要一并打分的候选来源。
func elementsWithTag(root *html.Node, a atom.Atom, max int) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(out) >= max {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == a {
			out = append(out, n)
			return // 不深入已命中元素的子树
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

// metaContents 收集所有 <meta> 中指定 property/name 的 content 值。
// 大小写不敏感：property="og:title" 与 property="OG:Title" 都算命中。
func metaContents(root *html.Node, keys ...string) []string {
	var out []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Meta {
			got := strings.ToLower(attr(n, "property"))
			if got == "" {
				got = strings.ToLower(attr(n, "name"))
			}
			for _, k := range keys {
				if got == strings.ToLower(k) {
					if v := strings.TrimSpace(attr(n, "content")); v != "" {
						out = append(out, v)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}
