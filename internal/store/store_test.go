package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenRefusesSecondWriter 守住数据目录的单写者约束。
//
// 两个实例同时写一个目录会同时踩中三件事：每域限速失效（frontier 是进程内的，
// 各持一个时间片）、产物被对方的 truncateTornLine 截掉、manifest 分叉导致
// 重复抓取。实测踩到过一次（旧二进制 + 新二进制同时跑），所以这里钉死。
func TestOpenRefusesSecondWriter(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir)
	if err != nil {
		t.Fatalf("第一个 Open 失败: %v", err)
	}

	if _, err := Open(dir); err == nil {
		first.Close()
		t.Fatal("第二个 Open 应当被排他锁拒绝，却成功了")
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// 释放之后必须能重新打开，否则一次异常退出会让目录永久锁死
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("锁释放后 Open 仍失败: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
}

// TestSaveSurvivesReopen 是续爬的基础：写入的 URL 必须能在下次 Open 时
// 读回「已抓」集合，否则续爬会从头再抓一遍。
func TestSaveSurvivesReopen(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"http://a.example/1", "http://a.example/2"} {
		if err := s.Save(Entry{URL: u, MainContent: "正文"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	re, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	for _, u := range []string{"http://a.example/1", "http://a.example/2"} {
		if !re.Has(u) {
			t.Errorf("重开之后 %s 不在已抓集合里", u)
		}
	}
	if re.Saved() != 2 {
		t.Errorf("Saved() = %d, 期望 2", re.Saved())
	}
}

// TestTruncateTornLine 覆盖被 kill -9 打断的场景：末尾半行必须被截掉，
// 否则它会永久留在产物文件里，让每个读它的程序都得自己防。
func TestTruncateTornLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pages.jsonl")

	full := "{\"url\": \"http://a.example/1\"}\n{\"url\": \"http://a.example/2\"}\n"
	if err := os.WriteFile(p, []byte(full+"{\"url\": \"http://a.exa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := truncateTornLine(p); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != full {
		t.Errorf("截断后 = %q，期望 %q", got, full)
	}

	// 完整文件不该被动
	if err := truncateTornLine(p); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(p)
	if string(got2) != full {
		t.Errorf("完整文件被改动了: %q", got2)
	}
}
