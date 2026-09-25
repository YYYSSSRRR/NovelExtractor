//go:build unix

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir 给数据目录加一把排他锁，返回解锁函数。
//
// 为什么必须有这把锁：数据目录是**单写者**结构——内存里的「已抓」集合、
// 待抓队列文件、产物的追加写，三者只在「只有一个进程在写」的前提下自洽。
// 两个实例同时开着会同时踩中三件事：
//
//	限速失效  frontier 是进程内的，两个实例各有各的时间片，同一个站会以
//	          2× 的速率被请求，每域间隔的承诺直接作废
//	产物错乱  两边都在 O_APPEND 同一个文件，续爬时各自 truncateTornLine，
//	          互相把对方正在写的行判成「断行」截掉
//	重复劳动  两边的 manifest 会分叉，同一个 URL 被抓两次
//
// 实测踩到过：一个改名前的旧二进制还在跑，产出全都写进了一个已删除的
// inode（lsof 里看得见、磁盘上看不见），新二进制同时在写同一个目录。
// 这类错误没有任何日志会提示，只能靠锁在入口拦住。
//
// 用 flock 而不是 pid 文件：进程被 kill -9 时内核会随 fd 关闭自动释放，
// 不会留下需要人工清理的僵死锁。
func lockDir(dir string) (unlock func(), err error) {
	path := filepath.Join(dir, "crawler.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: 打开锁文件 %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: 数据目录 %s 已被另一个爬虫实例占用（%w）；"+
			"同一目录同时只允许一个写者，请先停掉它或换一个 -data 目录", dir, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
