package fetch

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

// Robots 是 robots.txt 中与本次抓取有关的部分。
//
// 只实现两件事：允许/禁止，以及 Crawl-delay。通配符与正则形式的路径
// （`*`、`$`）不做完整支持——按前缀匹配处理，方向是**偏保守**
// （拿不准就当禁止），这比过度支持更安全。
type Robots struct {
	groups []group
	delay  time.Duration
	ua     string
}

type group struct {
	agents []string
	rules  []rule
}

type rule struct {
	allow  bool
	prefix string
}

// ParseRobots 解析 robots.txt，挑出适用本次 UA 的规则组。
//
// 选择逻辑按标准：先找 agent 精确/前缀匹配我们 UA 的组，找不到再用 `*` 组。
func ParseRobots(r io.Reader, ua string) *Robots {
	rb := &Robots{ua: ua}
	var cur *group

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)

		switch key {
		case "user-agent":
			// 同一组可以跟多个 User-agent 行；遇到新的 UA 行且当前组已有规则
			// 就开新组。
			if cur == nil || len(cur.rules) > 0 {
				rb.groups = append(rb.groups, group{})
				cur = &rb.groups[len(rb.groups)-1]
			}
			cur.agents = append(cur.agents, strings.ToLower(val))
		case "disallow", "allow":
			if cur == nil {
				continue
			}
			cur.rules = append(cur.rules, rule{allow: key == "allow", prefix: val})
		case "crawl-delay":
			if d, err := strconv.ParseFloat(val, 64); err == nil && d > 0 {
				rb.delay = time.Duration(d * float64(time.Second))
			}
		}
	}
	return rb
}

// match 报告本条 UA 是否匹配该组。specific 表示命中的是具名 agent 而非 `*`。
func (g group) match(ua string) (matched, specific bool) {
	ua = strings.ToLower(ua)
	for _, a := range g.agents {
		if a == "" {
			continue
		}
		if a != "*" && strings.Contains(ua, a) {
			return true, true
		}
	}
	for _, a := range g.agents {
		if a == "*" {
			return true, false
		}
	}
	return false, false
}

// bestGroup 挑出适用本条 UA 的规则组。
//
// 必须优先用具名组而不是文件里第一个匹配的组：`User-agent: *` 通常写在最前面，
// 若按出现顺序返回，后面针对特定爬虫的规则就永远不会生效——这会让
// 「BadBot 禁止全站」形同虚设。标准做法也是具名优先。
func (rb *Robots) bestGroup() *group {
	var fallback *group
	for i := range rb.groups {
		matched, specific := rb.groups[i].match(rb.ua)
		if !matched {
			continue
		}
		if specific {
			return &rb.groups[i]
		}
		if fallback == nil {
			fallback = &rb.groups[i]
		}
	}
	return fallback
}

// Delay 返回站点声明的 Crawl-delay，未声明时为 0。
func (rb *Robots) Delay() time.Duration {
	if rb.bestGroup() == nil {
		return 0
	}
	return rb.delay
}

// Allowed 判断某个路径是否可抓。
//
// 规则冲突时取**最长匹配**（标准的做法）；长度相同时 Allow 优先。
// 没有任何规则命中即视为允许——这是 robots 的默认语义。
func (rb *Robots) Allowed(path string) bool {
	if rb == nil || path == "" {
		path = "/"
	}
	g := rb.bestGroup()
	if g == nil {
		return true
	}
	best := -1
	allow := true
	for _, rl := range g.rules {
		if rl.prefix == "" {
			continue // `Disallow:` 空值表示全部允许
		}
		if !strings.HasPrefix(path, rl.prefix) {
			continue
		}
		if n := len(rl.prefix); n > best || (n == best && rl.allow) {
			best, allow = n, rl.allow
		}
	}
	return allow
}
