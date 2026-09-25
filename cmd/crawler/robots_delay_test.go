package main

import (
	"testing"
	"time"
)

// TestDelayBlockedIsPerHostAndSticky 守住「Crawl-delay 过长就整个域不去」的两个细节。
//
// 一是一域只报一次：allowed 是每个 URL 调一次，一个被放弃的域可能排了几十条
// URL，不去重就会把日志刷满，而这条信息恰恰是排查「爬取为什么停不下来」时
// 第一眼要看的。
//
// 二是判定要粘住：调用方靠 delayBlockedHost 把「我们主动不去」和
// 「robots 不许去」分开计数，同一次爬取里两次问同一域，答案必须一样，
// 否则计数会在这两类之间跳动。
func TestDelayBlockedIsPerHostAndSticky(t *testing.T) {
	c := &crawler{delayReported: make(map[string]bool)}

	if c.delayBlockedHost("slow.example") {
		t.Fatal("还没判定过，不该说它被放弃")
	}

	c.noteDelayBlocked("slow.example", 2*time.Hour)
	c.noteDelayBlocked("slow.example", 2*time.Hour) // 第二次不该改变什么
	c.noteDelayBlocked("slow.example", 3*time.Hour) // 数值变了也一样

	if !c.delayBlockedHost("slow.example") {
		t.Error("判定过之后应当一直是「已放弃」")
	}
	if c.delayBlockedHost("fast.example") {
		t.Error("另一个域被连坐了")
	}
}

// TestMaxHostDelayLeavesNormalSites 盯住上限没有低到误伤常见站点。
//
// 这个上限只该拦下小时级的声明。常见的 Crawl-delay 是 1~30 秒，全部要在
// 上限之内照做——如果哪天有人把 maxHostDelay 调到几秒，这条会先红。
func TestMaxHostDelayLeavesNormalSites(t *testing.T) {
	for _, d := range []time.Duration{
		time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second,
	} {
		if d > maxHostDelay {
			t.Errorf("常见的 Crawl-delay %s 被上限 %s 误伤，会被整个域放弃", d, maxHostDelay)
		}
	}
	if 2*time.Hour <= maxHostDelay {
		t.Errorf("上限 %s 高到拦不住小时级的声明，那这轮爬取还是结束不了", maxHostDelay)
	}
}
