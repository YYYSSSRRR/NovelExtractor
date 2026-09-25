// Package fetch 负责把 URL 变成 UTF-8 的 HTML 字符串。
//
// 这里刻意做得薄：没有代理池、没有 UA 轮换、没有 JS 挑战破解。那些属于
// 规避行为，而且有上千个候选域作冗余，单域被拒的成本可以忽略，做对抗是
// 负期望。真正需要做对的是三件事——编码统一、体积上限、诚实重试。
package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/html/charset"
)

// DefaultUA 是单一、诚实的 User-Agent，不轮换、不伪装。
const DefaultUA = "web-extract/1.0 (research crawler; +https://example.invalid/bot)"

// Result 是一次抓取的产物。
type Result struct {
	URL        string
	FinalURL   string // 跟随重定向之后的地址
	StatusCode int
	Proto      string // "HTTP/1.1" 等。客户端刻意关掉了 HTTP/2，留个字段可验证
	HTML       string
	Bytes      int
	Elapsed    time.Duration
	Truncated  bool // 正文超过上限被截断
}

// Client 可复用，内部 http.Client 自带连接池。
type Client struct {
	hc       *http.Client
	ua       string
	maxBytes int64
	timeout  time.Duration
	retries  int
}

// New 构造客户端。maxBytes 是响应体上限——不设上限的话，一个指向大文件的
// URL 就能把内存打满。
//
// 这里显式关掉 HTTP/2，两个理由：
//
//  1. **HTTP/2 下 Client.Timeout 会失效。** 实测被卡死过：评测跑到最后一个域
//     时挂住两分多钟不动，kill -QUIT 打出的栈是
//     `net/http.(*http2ClientStream).writeRequest` 里的 select，
//     而 15s 的 Client.Timeout 始终没触发。HTTP/2 的 roundTrip 走的是
//     stream 级的 select，Client.Timeout 那套基于连接 deadline 的取消覆盖不到它。
//     详见 once 里再补的请求级 context 超时——两道保险都要。
//  2. **多路复用对礼貌爬虫没有价值。** 我们对每个域本来就是串行的，
//     HTTP/2 把多个请求塞进一条 TCP 连接，只会让「一次请求 = 一次可计时的
//     网络往返」这个账算不清。HTTP/1.1 的连接池语义更适合这里。
//
// 关掉 HTTP/2 不影响可达性：HTTP/1.1 是所有服务器都支持的兜底协议。
func New(ua string, timeout time.Duration, maxBytes int64) *Client {
	if ua == "" {
		ua = DefaultUA
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &Client{
		hc: &http.Client{
			Transport: tr,
			Timeout:   timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
		ua:       ua,
		maxBytes: maxBytes,
		timeout:  timeout,
		retries:  2,
	}
}

// SetRetries 调整失败重试次数，返回自身以便链式构造。
//
// 抓取正文时重试是划算的——一次网络抖动不该让这个 URL 白丢。但批量探测
// 可达性时正相反：探测只关心「活着没有」，重试既不会改变结论，又让每个
// 死域名都付出 (retries+1) × timeout 的代价，整批探测的墙钟时间直接翻三倍。
func (c *Client) SetRetries(n int) *Client {
	if n < 0 {
		n = 0
	}
	c.retries = n
	return c
}

// Get 抓取一个 URL 并解码成 UTF-8。
//
// 只对「可能自愈」的失败重试：网络抖动、5xx、429。4xx 一律不重试——
// 那是资源本身不存在或被拒，重试只是给别人添麻烦。
func (c *Client) Get(ctx context.Context, rawURL string) (*Result, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			// 退避：0.5s、1s。不再往上加，因为调度器本身已经保证了域级间隔
			delay := time.Duration(attempt) * 500 * time.Millisecond
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		res, retryable, err := c.once(ctx, rawURL)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !retryable {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, rawURL string) (res *Result, retryable bool, err error) {
	start := time.Now()

	// 请求级超时。Client.Timeout 在 HTTP/2 下不可靠（见 New 的注释），
	// 这里用 context 再兜一道：context 的取消是所有 RoundTripper 都必须响应的，
	// 与协议实现无关。两道保险叠加，单个请求的最坏耗时就被钉死在 timeout 上，
	// 「一个请求永远挂着」这种状态不可能出现。
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, err // 网络类错误，值得重试
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) // 让连接可复用
		resp.Body.Close()
	}()

	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return nil, true, fmt.Errorf("%s: http %d", rawURL, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("%s: http %d", rawURL, resp.StatusCode)
	}

	// 先套体积上限，再交给 charset 探测器：顺序反过来的话，探测器会为了
	// 嗅探 meta 标签读进超量数据。charset.NewReader 一次性覆盖 BOM、
	// Content-Type 头与 <meta charset> 三种来源，GBK/GB18030/Big5 都能转。
	body := io.LimitReader(resp.Body, c.maxBytes+1)
	decoded, err := charset.NewReader(body, resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, false, err
	}
	raw, err := io.ReadAll(decoded)
	if err != nil {
		return nil, true, err
	}

	truncated := int64(len(raw)) > c.maxBytes
	if truncated {
		raw = raw[:c.maxBytes]
	}

	return &Result{
		URL:        rawURL,
		FinalURL:   resp.Request.URL.String(),
		StatusCode: resp.StatusCode,
		Proto:      resp.Proto,
		HTML:       string(raw),
		Bytes:      len(raw),
		Elapsed:    time.Since(start),
		Truncated:  truncated,
	}, false, nil
}
