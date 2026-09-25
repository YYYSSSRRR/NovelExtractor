// Package store 负责把抓到的页面落盘，并维护一份可续爬的 manifest。
//
// 落盘布局：
//
//	data/raw/<host>/<sha1(url)[:16]>.html   原始 HTML，按域名分目录
//	data/manifest.jsonl                     一行一条元数据，追加写
//
// 之所以用「文件 + JSONL」而不是 SQLite：5000-100000 页这个量级，一个
// 追加写的 manifest 加一个内存集合就够，而且**天然可续爬**——重启时把
// manifest 读回来重建「已抓」集合，已完成的页面直接跳过。引入数据库
// 只会增加部署依赖，换不来任何东西。
package store

import (
	"bufio"
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

// Entry 是 manifest 的一行。
type Entry struct {
	URL       string    `json:"url"`
	FinalURL  string    `json:"final_url,omitempty"`
	Host      string    `json:"host"`
	Path      string    `json:"path"` // 相对 data 目录
	Status    int       `json:"status"`
	Bytes     int       `json:"bytes"`
	ElapsedMS int64     `json:"elapsed_ms"`
	FetchedAt time.Time `json:"fetched_at"`
	Title     string    `json:"meta_title,omitempty"` // 抽取结果一并记下，便于离线分析
	RealTitle string    `json:"real_title,omitempty"`
	Chars     int       `json:"content_chars,omitempty"`
}

// Store 并发安全。
type Store struct {
	dir string

	mu       sync.Mutex
	manifest *os.File
	bw       *bufio.Writer
	done     map[string]struct{} // 已抓 URL，续爬时重建
	saved    int
}

// Open 打开（或创建）数据目录，并从既有的 manifest 重建「已抓」集合。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "raw"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, done: make(map[string]struct{})}

	mfPath := filepath.Join(dir, "manifest.jsonl")
	if err := s.loadManifest(mfPath); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(mfPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.manifest = f
	s.bw = bufio.NewWriter(f)
	return s, nil
}

// loadManifest 读回已有的 manifest。只认能解析完整的行——被 kill -9 打断时
// 最后一行可能是半条，直接丢弃即可（那一页下次会重抓，代价可忽略）。
func (s *Store) loadManifest(path string) error {
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

// Save 写入 HTML 并追加一条 manifest。两条都成功才算数——先写 HTML 再写
// manifest，保证 manifest 里出现的路径一定已经有文件。
func (s *Store) Save(e Entry, html string) error {
	if e.URL == "" {
		return errors.New("store: empty url")
	}
	if e.FetchedAt.IsZero() {
		e.FetchedAt = time.Now()
	}
	rel := filepath.Join("raw", sanitizeHost(e.Host), hashURL(e.URL)+".html")
	abs := filepath.Join(s.dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(abs, html); err != nil {
		return err
	}
	e.Path = rel

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

// Flush 把缓冲刷到磁盘。优雅退出时调用。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bw.Flush()
}

// Close 刷盘并关闭。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.bw.Flush()
	if cerr := s.manifest.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeFileAtomic 先写临时文件再 rename，避免留下半个文件被误当成完整页面。
func writeFileAtomic(path, data string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

func hashURL(u string) string {
	sum := sha1.Sum([]byte(u))
	return hex.EncodeToString(sum[:])[:16]
}

// sanitizeHost 把域名变成安全的目录名：只保留字母数字与点横线，
// 其余替换成下划线。避免 `..` 或斜杠造成目录穿越。
func sanitizeHost(host string) string {
	if host == "" {
		return "_unknown"
	}
	out := make([]rune, 0, len(host))
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
		default:
			out = append(out, '_')
		}
	}
	s := string(out)
	if s == "" || s == "." || s == ".." {
		return "_unknown"
	}
	return s
}
