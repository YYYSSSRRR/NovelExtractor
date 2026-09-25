package fetch

import (
	"strings"
	"testing"
	"time"
)

const sampleRobots = `
# 注释行
User-agent: *
Disallow: /search
Disallow: /admin/
Allow: /admin/public/
Crawl-delay: 2

User-agent: BadBot
Disallow: /
`

func TestRobotsAllowed(t *testing.T) {
	// 我们的 UA 谁都不匹配，应落到 * 组
	rb := ParseRobots(strings.NewReader(sampleRobots), "web-extract/1.0")
	cases := []struct {
		path string
		want bool
	}{
		{"/", true},
		{"/article/123.html", true},
		{"/search", false},
		{"/search/xxx", false},
		{"/admin/panel", false},
		{"/admin/public/x", true}, // Allow 比 Disallow 更具体
	}
	for _, c := range cases {
		if got := rb.Allowed(c.path); got != c.want {
			t.Errorf("Allowed(%q) = %v，期望 %v", c.path, got, c.want)
		}
	}
}

func TestRobotsSpecificAgentWins(t *testing.T) {
	rb := ParseRobots(strings.NewReader(sampleRobots), "BadBot/2.1")
	if rb.Allowed("/article/1") {
		t.Error("BadBot 组声明 Disallow: /，不应允许任何路径")
	}
}

func TestRobotsCrawlDelay(t *testing.T) {
	rb := ParseRobots(strings.NewReader(sampleRobots), "web-extract/1.0")
	if d := rb.Delay(); d != 2*time.Second {
		t.Errorf("Crawl-delay = %s，期望 2s", d)
	}
}

// TestRobotsAllowWinsOnTie 验证同长度冲突时 Allow 优先。
func TestRobotsAllowWinsOnTie(t *testing.T) {
	const txt = "User-agent: *\nDisallow: /a\nAllow: /a\n"
	rb := ParseRobots(strings.NewReader(txt), "x")
	if !rb.Allowed("/a") {
		t.Error("同长度冲突时应 Allow 优先")
	}
}

// TestRobotsNoRulesAllows 验证默认语义：没有规则即允许。
func TestRobotsNoRulesAllows(t *testing.T) {
	rb := ParseRobots(strings.NewReader("User-agent: *\n"), "x")
	if !rb.Allowed("/anything") {
		t.Error("无规则时应默认允许")
	}
}
