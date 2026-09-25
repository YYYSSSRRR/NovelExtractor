package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestGetTimesOutOnStalledServer 是一条回归测试：请求绝不能永远挂着。
//
// 这个 bug 真实发生过。评测跑到最后一个域名时挂住两分多钟，15s 的
// Client.Timeout 始终没触发，kill -QUIT 打出的栈是
// `net/http.(*http2ClientStream).writeRequest` 里的 select——HTTP/2 的
// stream 级等待覆盖不到 Client.Timeout 那套连接级 deadline。
//
// 所以这里断言的不是「超时后会返回错误」（那太弱），而是**一定会返回**：
// 服务端收到请求后既不回响应也不关连接，客户端必须在 timeout 量级内脱身。
func TestGetTimesOutOnStalledServer(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // 永不响应，模拟卡死的服务端
	}))
	// defer 是后进先出：必须先注册 srv.Close，再注册 close(release)，
	// 这样退出时先放行 handler、再关服务器。反过来写会自锁——srv.Close()
	// 会等在途 handler 退出，而 handler 正阻塞在 <-release 上。
	defer srv.Close()
	defer close(release)

	const timeout = 300 * time.Millisecond
	c := New(DefaultUA, timeout, 1<<20).SetRetries(0)

	done := make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), srv.URL)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("卡死的服务端不该返回成功")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("请求挂住超过 10s 仍未返回——超时没有生效")
	}
}

// TestGetTimesOutOnStalledBody 覆盖另一种卡法：响应头已经回来了，body 却不再
// 推进。这一路走的是读 body 的阶段，和上一条的「等响应头」不是同一段代码。
func TestGetTimesOutOnStalledBody(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(200)
		w.(http.Flusher).Flush() // 先把头送出去
		<-release                // body 停在这里
	}))
	defer srv.Close()
	defer close(release) // 顺序同 TestGetTimesOutOnStalledServer

	const timeout = 300 * time.Millisecond
	c := New(DefaultUA, timeout, 1<<20).SetRetries(0)

	done := make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), srv.URL)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("body 卡死不该返回成功")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("读 body 阶段挂住超过 10s——超时没有覆盖到")
	}
}

// TestGetDisabledHTTP2 说明我们刻意不用 HTTP/2。上面那条挂死就出在 HTTP/2 上，
// 而且对按域串行的礼貌爬虫来说多路复用本来也没有价值。
func TestGetDisabledHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><body>ok</body></html>"))
	}))
	srv.EnableHTTP2 = true // 服务端支持 h2，客户端也应当仍走 HTTP/1.1
	srv.StartTLS()
	defer srv.Close()

	c := New(DefaultUA, 5*time.Second, 1<<20).SetRetries(0)

	// httptest 的证书是自签的。只信任它，并且**只**协商 http/1.1——
	// 否则 ALPN 可能选中 h2，而传输层已按 HTTP/1.1 配置，反而对不上。
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr := c.hc.Transport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, NextProtos: []string{"http/1.1"}}
	c.hc.Transport = tr

	res, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.HTML == "" {
		t.Fatal("没拿到内容")
	}
	if got := res.Proto; got != "HTTP/1.1" {
		t.Errorf("协议是 %s，期望 HTTP/1.1（客户端应已关掉 HTTP/2）", got)
	}
}
