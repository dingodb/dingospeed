package proxypool

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProxy 起一个最小的正向代理：对明文 http 请求，代理收到的是绝对 URI，
// 直接按 handler 回内容即可，足以覆盖 probeOnce 的真实路径。
func fakeProxy(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeMarksHealthyOnSuccess(t *testing.T) {
	srv := fakeProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, strings.Repeat("x", 2048))
	})
	p := newTestPool(t, []MemberConfig{{Name: "a", URL: srv.URL}}, 1, time.Hour)
	m := p.Members()[0]
	p.Report(m, 0, io.EOF) // 先打成熔断
	if m.Healthy() {
		t.Fatal("前置条件不成立：应已熔断")
	}

	code, err := probeOnce(m, "http://example.invalid/api/models/x", 5*time.Second)
	if err != nil || code != http.StatusOK {
		t.Fatalf("探活应成功, code=%d err=%v", code, err)
	}
	p.Report(m, code, err)
	if !m.Healthy() {
		t.Fatal("探活成功后应恢复可用")
	}
}

func TestProbeDetectsTruncatedBody(t *testing.T) {
	// 模拟 gost 隧道建起来了、响应头也回来了，但正文传一半就断。
	// 这正是 TCP 层健康检查看不见、只有读 body 才暴露的故障。
	srv := fakeProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// handler 返回但没写满 Content-Length，客户端读 body 时会拿到错误。
	})
	p := newTestPool(t, []MemberConfig{{Name: "a", URL: srv.URL}}, 1, time.Hour)
	m := p.Members()[0]

	_, err := probeOnce(m, "http://example.invalid/api/models/x", 5*time.Second)
	if err == nil {
		t.Fatal("正文被截断时探活应判失败")
	}
}

func TestProbeFailsOnDeadProxy(t *testing.T) {
	p := newTestPool(t, []MemberConfig{{Name: "dead", URL: "http://127.0.0.1:1"}}, 1, time.Hour)
	m := p.Members()[0]
	code, err := probeOnce(m, "http://example.invalid/x", 2*time.Second)
	if err == nil {
		t.Fatalf("连不上的代理应报错, code=%d", code)
	}
	p.Report(m, code, err)
	if m.Healthy() {
		t.Fatal("探活失败达阈值后应熔断")
	}
}

func TestProbeAllCoversEveryMember(t *testing.T) {
	var hits atomic.Int32
	srv := fakeProxy(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	p := newTestPool(t, []MemberConfig{
		{Name: "a", URL: srv.URL},
		{Name: "b", URL: srv.URL + "/"},
	}, 1, time.Hour)
	p.cfg.ProbeTimeout = 5 * time.Second

	p.probeAll("http://example.invalid/x")
	if got := hits.Load(); got != 2 {
		t.Fatalf("每个成员都应被探一次，实际 %d", got)
	}
	if p.Available() != 2 {
		t.Fatalf("探活全通后可用数应为 2，实际 %d", p.Available())
	}
}

func TestStartProbeStopsOnClose(t *testing.T) {
	srv := fakeProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	p := newTestPool(t, []MemberConfig{{Name: "a", URL: srv.URL}}, 1, time.Hour)
	p.cfg.ProbeInterval = 10 * time.Millisecond
	p.cfg.ProbeTimeout = time.Second
	p.StartProbe()
	time.Sleep(30 * time.Millisecond)
	p.Close()
	p.Close() // 重复 Close 不应 panic
}
