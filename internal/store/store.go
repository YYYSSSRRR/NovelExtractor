// Package store 负责把抓取产物落盘，并维护一份可续爬的 manifest。
//
// 落盘布局：
//
//	data/pages.jsonl     一行一页：抽取结果 + 抓取元数据，追加写
//	data/queue.jsonl     待抓队列，追加写，与 manifest 合起来才是完整的续爬状态
//
// **只存抽取结果，不存原始 HTML**。题目要的交付物是 real_title 与 main_content，
// HTML 只是中间态：它对语料的唯一用途是「重抽一遍」，而重抽在正文判定规则
// 变化时才有意义，为此把每页上百 KB 的原文常驻磁盘（本轮实测 7000 页 709MB）
// 并不划算。若确需离线复算，爬虫重新跑一遍即可——种子表、队列、去重集合都在，
// 复现的是同一批 URL。
//
// 代价说清楚：没有原文就不能事后换算法重抽，只能重爬。这是有意接受的取舍。
//
// 用「JSONL + 内存集合」而不是 SQLite：5000-100000 页这个量级，一个追加写的
// manifest 加一个内存集合就够，而且**天然可续爬**——重启时把 manifest 读回来
// 重建「已抓」集合，已完成的页面直接跳过。引入数据库只会增加部署依赖。
package store

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry 是产物文件的一行。
//
// 字段顺序即 JSON 输出顺序，刻意把题目要求的四个字段排在前面：产物文件因此
// 可以直接当作交付物读，元数据只是附在后面的证据。
type Entry struct {
	URL         string `json:"url"`
	MetaTitle   string `json:"meta_title"`
	RealTitle   string `json:"real_title"`
	MainContent string `json:"main_content"`

	// 以下为抓取元数据。bytes + html_sha1 是「确实下载过这么多页面」的证据：
	// 不留原文，但要留下原文的指纹与体积，便于核对与去重分析。
	FinalURL  string    `json:"final_url,omitempty"`
	Host      string    `json:"host"`
	Status    int       `json:"status"`
	Bytes     int       `json:"bytes"`
	HTMLSha1  string    `json:"html_sha1,omitempty"`
	ElapsedMS int64     `json:"elapsed_ms"`
	FetchedAt time.Time `json:"fetched_at"`

	Chars    int     `json:"content_chars"`
	LinkRate float64 `json:"content_link_rate"`
}

// Store 并发安全。
type Store struct {
	dir string

	mu     sync.Mutex
	out    *os.File
	bw     *bufio.Writer
	done   map[string]struct{} // 已抓 URL，续爬时重建
	saved  int
	unlock func() // 释放数据目录的排他锁
}

// Open 打开（或创建）数据目录，并从既有的产物文件重建「已抓」集合。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// 抢锁必须在 truncateTornLine 之前：那一步是**破坏性**的，两个实例
	// 同时进来会互相把对方正在写的行当成断行截掉。详见 lock_unix.go。
	unlock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}

	s := &Store{dir: dir, done: make(map[string]struct{}), unlock: unlock}

	p := filepath.Join(dir, "pages.jsonl")
	if err := s.loadDone(p); err != nil {
		return nil, err
	}
	if err := truncateTornLine(p); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.out = f
	s.bw = bufio.NewWriter(f)
	return s, nil
}

// loadDone 读回已抓集合。只认能解析完整的行——被 kill -9 打断时最后一行
// 可能是半条，直接丢弃即可（那一页下次会重抓，代价可忽略）。
func (s *Store) loadDone(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.URL == "" {
			continue
		}
		s.done[e.URL] = struct{}{}
		s.saved++
	}
	return sc.Err()
}

// truncateTornLine 把产物文件末尾没写完的半行截掉。
//
// 被 kill -9 打断时最后一行可能只写了一半。留着它有两个害处：产物文件永久
// 带着一段解析不了的残片，任何读它的程序都得自己防；而且它占着文件尾，
// 下次续爬追加的正常记录会紧跟在残片后面，看起来像是数据损坏。
//
// 截掉是安全的：残片对应的那一页因为解析不出来，必然没进「已抓」集合，
// 下次一定会重抓，所以这半行没有任何信息量。
func truncateTornLine(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return nil // 文件不存在，没有可截的
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], st.Size()-1); err != nil {
		return nil
	}
	if last[0] == '\n' {
		return nil
	}

	// 从尾部往前分块找最后一个换行。块足够大，正常情况一次就能命中。
	const chunk = 64 << 10
	for off := st.Size(); off > 0; {
		n := int64(chunk)
		if n > off {
			n = off
		}
		off -= n
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, off); err != nil {
			return nil
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			return os.Truncate(path, off+int64(i)+1)
		}
	}
	return os.Truncate(path, 0) // 整份文件没有换行，等于没写过
}

// Has 报告 URL 是否已经抓过。续爬时爬虫据此跳过。
func (s *Store) Has(url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.done[url]
	return ok
}

// Saved 返回本次运行已落盘的页面数（含续爬时读回的历史记录）。
func (s *Store) Saved() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved
}

// Save 追加一条产物记录。只做一次缓冲写，没有文件系统操作。
func (s *Store) Save(e Entry) error {
	if e.URL == "" {
		return errors.New("store: empty url")
	}
	if e.FetchedAt.IsZero() {
		e.FetchedAt = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.bw.Write(append(line, '\n')); err != nil {
		return err
	}
	s.done[e.URL] = struct{}{}
	s.saved++
	return nil
}

// Digest 返回原文的 SHA-1 前 16 位十六进制。不留原文，但留下指纹：
// 「这一页确实下载过」以及「两页是不是同一份内容」都还能验证。
func Digest(html string) string {
	sum := sha1.Sum([]byte(html))
	return hex.EncodeToString(sum[:])[:16]
}

// Flush 把缓冲刷到磁盘。优雅退出时调用。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bw.Flush()
}

// Close 刷盘、关闭并释放数据目录锁。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.bw.Flush()
	if cerr := s.out.Close(); err == nil {
		err = cerr
	}
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	return err
}

// queuePath 是待抓队列的持久化位置。
func (s *Store) queuePath() string { return filepath.Join(s.dir, "queue.jsonl") }

// AppendQueue 把新发现的 URL 追加进待抓队列文件。
//
// manifest 记录「抓完了什么」，queue 记录「还要抓什么」，两者合起来才是
// 完整的续爬状态。只持久化 manifest 的话，kill -9 重启后内存里的待抓队列
// 就没了，而已抓集合又会让所有种子被跳过——结果是什么都不做，直接「爬完」。
func (s *Store) AppendQueue(urls []string) error {
	if len(urls) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.queuePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	for _, u := range urls {
		if _, err := bw.WriteString(u); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// LoadQueue 读回待抓队列。文件不存在时返回空。
func (s *Store) LoadQueue() ([]string, error) {
	f, err := os.Open(s.queuePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		if u := strings.TrimSpace(sc.Text()); u != "" {
			out = append(out, u)
		}
	}
	return out, sc.Err()
}
