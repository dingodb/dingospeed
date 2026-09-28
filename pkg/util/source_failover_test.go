package util

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/proxypool"
)

func TestSharedSourceFailoverPreservesOrigin(t *testing.T) {
	for _, source := range []string{"huggingface", "modelscope"} {
		t.Run(source, func(t *testing.T) {
			setupConfig(t, "huggingface.invalid", "mirror.invalid")
			config.SysConfig.Modelscope.MaxRetry = 1
			calls := []string{}
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls = append(calls, "bad"); w.WriteHeader(503) }))
			defer bad.Close()
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.Host)
				io.WriteString(w, "ok")
			}))
			defer good.Close()
			setupPool(t, []proxypool.MemberConfig{{Name: "bad", URL: bad.URL, Weight: 5}, {Name: "good", URL: good.URL}}, 3)
			r, err := constructSourceRoute(source, "GET", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := r.client.Get("http://" + source + ".invalid/metadata")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if strings.Join(calls, ",") != "bad,"+source+".invalid" || resp.StatusCode != 200 {
				t.Fatalf("calls=%v status=%d", calls, resp.StatusCode)
			}
		})
	}
}

func TestSharedSource404DoesNotFailover(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			setupConfig(t, "huggingface.invalid", "mirror.invalid")
			calls := 0
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(status) }))
			defer proxy.Close()
			setupPool(t, []proxypool.MemberConfig{{Name: "a", URL: proxy.URL}, {Name: "b", URL: proxy.URL}}, 1)
			config.SysConfig.Modelscope.MaxRetry = 5
			req, _ := http.NewRequest("GET", "http://modelscope.invalid/missing", nil)
			resp, err := DoRequestWithRetry(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != status || calls != 1 {
				t.Fatalf("status=%d calls=%d", resp.StatusCode, calls)
			}
		})
	}
}

func TestSharedSourceDirectFallbackPolicy(t *testing.T) {
	for _, source := range []string{"huggingface", "modelscope"} {
		for _, allowed := range []bool{false, true} {
			t.Run(source+map[bool]string{false: "-disabled", true: "-enabled"}[allowed], func(t *testing.T) {
				directCalls := 0
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { directCalls++; io.WriteString(w, "direct") }))
				defer origin.Close()
				setupConfig(t, "hf.invalid", strings.TrimPrefix(origin.URL, "http://"))
				config.SysConfig.ProxyPool.FallbackDirect = &allowed
				bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
				defer bad.Close()
				setupPool(t, []proxypool.MemberConfig{{Name: "bad", URL: bad.URL}}, 3)
				r, err := constructSourceRoute(source, "GET", nil)
				if err != nil {
					t.Fatal(err)
				}
				target := origin.URL + "/metadata"
				if source == "huggingface" {
					target = "http://hf.invalid/metadata"
				}
				resp, err := r.client.Get(target)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if allowed && (directCalls != 1 || resp.StatusCode != 200) {
					t.Fatal("fallback failed")
				}
				if !allowed && (directCalls != 0 || resp.StatusCode != 503) {
					t.Fatal("unexpected direct fallback")
				}
			})
		}
	}
}

func TestSharedSourceBudgetStopsFailover(t *testing.T) {
	setupConfig(t, "hf.invalid", "mirror.invalid")
	calls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; <-r.Context().Done() }))
	defer proxy.Close()
	setupPool(t, []proxypool.MemberConfig{{Name: "a", URL: proxy.URL}, {Name: "b", URL: proxy.URL}}, 1)
	config.SysConfig.Modelscope.MaxRetry = 5
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://modelscope.invalid/metadata", nil)
	_, err := DoRequestWithRetry(req)
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestModelScopeLegacyProxyUsesSharedConfig(t *testing.T) {
	setupConfig(t, "hf.invalid", "mirror.invalid")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "modelscope.invalid" {
			t.Error(r.URL)
		}
		io.WriteString(w, "ok")
	}))
	defer proxy.Close()
	config.SysConfig.DynamicProxy.HttpProxy = proxy.URL
	cfg, enabled := buildPoolConfig()
	if !enabled {
		t.Fatal("legacy config ignored")
	}
	pool, err := proxypool.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	old := globalPool.Load()
	globalPool.Store(pool)
	t.Cleanup(func() { globalPool.Store(old) })
	config.SysConfig.Modelscope.MaxRetry = 1
	req, _ := http.NewRequest("GET", "http://modelscope.invalid/repo", nil)
	resp, err := DoRequestWithRetry(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}
