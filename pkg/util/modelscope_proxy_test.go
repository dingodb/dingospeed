package util

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"dingospeed/pkg/config"
)

func TestModelScopeProxyPreservesTargetAndRedirect(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Range") != "bytes=0-3" {
			t.Error("range header lost")
		}
		switch r.URL.String() {
		case "http://modelscope.invalid/repo":
			http.Redirect(w, r, "http://storage.invalid/blob", http.StatusFound)
		case "http://storage.invalid/blob":
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "data")
		default:
			t.Errorf("unexpected target %s", r.URL)
			w.WriteHeader(500)
		}
	}))
	defer proxy.Close()
	client := newModelScopeHTTPClient(proxy.URL)
	defer client.CloseIdleConnections()
	client.Timeout = 2 * time.Second
	req, _ := http.NewRequest("GET", "http://modelscope.invalid/repo", nil)
	req.Header.Set("Range", "bytes=0-3")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if calls.Load() != 2 || resp.StatusCode != 206 || string(body) != "data" {
		t.Fatalf("calls=%d status=%d body=%q", calls.Load(), resp.StatusCode, body)
	}
}

func TestModelScopeProxyHTTPSConnect(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "metadata") }))
	defer origin.Close()
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Errorf("method=%s", r.Method)
			w.WriteHeader(500)
			return
		}
		upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		connects.Add(1)
		io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go io.Copy(upstream, conn)
		io.Copy(conn, upstream)
	}))
	defer proxy.Close()
	client := newModelScopeHTTPClient(proxy.URL)
	defer client.CloseIdleConnections()
	client.Timeout = 2 * time.Second
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if connects.Load() != 1 || string(body) != "metadata" {
		t.Fatalf("connects=%d body=%q", connects.Load(), body)
	}
}

func TestModelScopeDirectDoesNotInheritHFOrEnvironmentProxy(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{DynamicProxy: config.DynamicProxy{HttpProxy: "http://127.0.0.1:1"}}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	client := newModelScopeHTTPClient("")
	defer client.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "https://www.modelscope.cn", nil)
	proxy, err := client.Transport.(*http.Transport).Proxy(req)
	if err != nil || proxy != nil {
		t.Fatalf("direct route inherited proxy: %v, %v", proxy, err)
	}
}
