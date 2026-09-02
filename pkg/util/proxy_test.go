//  Copyright (c) 2025 dingodb.com, Inc. All Rights Reserved
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http:www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package util

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/proxypool"
)

// 这些用例覆盖的是「池子接进下载链路」这一层，而不是池子本身：
// 选路、重试换出口、结果回灌、内网旁路、全池熔断回退。
// 池内部的熔断/权重/探活逻辑在 pkg/proxypool 里单独测。

func setupPool(t *testing.T, members []proxypool.MemberConfig, failThreshold int) *proxypool.Pool {
	t.Helper()
	p, err := proxypool.New(proxypool.Config{
		Enabled:       true,
		Members:       members,
		FailThreshold: failThreshold,
		Cooldown:      time.Hour,
	})
	if err != nil {
		t.Fatalf("构建代理池失败: %v", err)
	}
	prev := globalPool.Load()
	globalPool.Store(p)
	t.Cleanup(func() { globalPool.Store(prev) })
	return p
}

func setupConfig(t *testing.T, hfNetLoc, bpNetLoc string) {
	t.Helper()
	prev := config.SysConfig
	config.SysConfig = &config.Config{}
	config.SysConfig.Server.HfNetLoc = hfNetLoc
	config.SysConfig.Server.BpHfNetLoc = bpNetLoc
	config.SysConfig.Server.HfScheme = "http"
	t.Cleanup(func() { config.SysConfig = prev })
}

// 核心回归：一次请求的多次重试必须落在不同出口上。
// 修复前 RetryRequest 三次重试复用同一个代理，生产日志表现为 #1 EOF #2 EOF #3 EOF。
func TestConstructRouteRotatesOnRetry(t *testing.T) {
	setupConfig(t, "hf-mirror.com", "hf-mirror.com")
	setupPool(t, []proxypool.MemberConfig{
		{Name: "a", URL: "http://10.0.0.1:8121"},
		{Name: "b", URL: "http://10.0.0.2:8121"},
		{Name: "c", URL: "http://10.0.0.3:8121"},
	}, 3)

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		r, err := constructRoute(http.MethodGet)
		if err != nil {
			t.Fatalf("第 %d 次选路失败: %v", i, err)
		}
		if r.member == nil {
			t.Fatalf("第 %d 次选路没走池子", i)
		}
		if seen[r.member.Name()] {
			t.Fatalf("第 %d 次重试又落回 %s，重试没换出口", i, r.member.Name())
		}
		seen[r.member.Name()] = true
	}
}

// HEAD 必须拿到不跟随重定向的客户端。
func TestConstructRouteUsesHeadClient(t *testing.T) {
	setupConfig(t, "hf-mirror.com", "hf-mirror.com")
	setupPool(t, []proxypool.MemberConfig{{Name: "a", URL: "http://10.0.0.1:8121"}}, 3)

	r, err := constructRoute(http.MethodHead)
	if err != nil {
		t.Fatal(err)
	}
	if r.client.CheckRedirect == nil {
		t.Fatal("HEAD 应拿到阻止重定向的客户端")
	}
	g, _ := constructRoute(http.MethodGet)
	if g.client.CheckRedirect != nil {
		t.Fatal("GET 应保持默认跟随重定向")
	}
}

// 全池熔断后必须回退直连备用域名，而不是把请求丢掉。
func TestConstructRouteFallsBackToDirect(t *testing.T) {
	setupConfig(t, "hf-mirror.com", "backup.example.com")
	p := setupPool(t, []proxypool.MemberConfig{{Name: "a", URL: "http://10.0.0.1:8121"}}, 1)
	p.Report(p.Members()[0], http.StatusBadGateway, nil)

	r, err := constructRoute(http.MethodGet)
	if err != nil {
		t.Fatal(err)
	}
	if r.member != nil {
		t.Fatal("全池熔断后不应再选中成员")
	}
	if r.domain != "http://backup.example.com" {
		t.Fatalf("应回退到备用域名，实际 %s", r.domain)
	}
}

// gated 仓库返回 403 不能熔断健康出口。
func TestReportDoesNotTripOnGatedRepo(t *testing.T) {
	setupConfig(t, "hf-mirror.com", "hf-mirror.com")
	p := setupPool(t, []proxypool.MemberConfig{{Name: "a", URL: "http://10.0.0.1:8121"}}, 3)
	m := p.Members()[0]

	r := route{member: m}
	for i := 0; i < 5; i++ {
		r.report(http.StatusForbidden, nil)
	}
	if !m.Healthy() {
		t.Fatal("连续 5 次 403（gated 仓库无权限）不应熔断出口")
	}
}

func TestReportTripsOnServerErrors(t *testing.T) {
	setupConfig(t, "hf-mirror.com", "hf-mirror.com")
	p := setupPool(t, []proxypool.MemberConfig{{Name: "a", URL: "http://10.0.0.1:8121"}}, 3)
	m := p.Members()[0]

	r := route{member: m}
	for i := 0; i < 3; i++ {
		r.report(http.StatusBadGateway, nil)
	}
	if m.Healthy() {
		t.Fatal("连续 3 次 502 应熔断出口")
	}
}

// 客户端主动取消不能记到出口头上。
func TestIsClientCanceled(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// remote_task 在 ctx done 时返回的正是这个包装形态。
		{"包装的 ctx 取消", fmt.Errorf("form remote ctx done: %w", context.Canceled), true},
		{"包装的 ctx 超时", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), true},
		{"真实传输错误", fmt.Errorf("premature EOF: expected 100 bytes, got 30"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isClientCanceled(c.err); got != c.want {
				t.Fatalf("isClientCanceled(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// 端到端：GetStream 打真实 HTTP 服务，成功后出口保持健康、状态码正确回灌。
func TestGetStreamReportsThroughProxy(t *testing.T) {
	var viaProxy int
	var mu sync.Mutex
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		viaProxy++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "hello")
	}))
	defer proxySrv.Close()

	setupConfig(t, "hf-mirror.com", "hf-mirror.com")
	p := setupPool(t, []proxypool.MemberConfig{{Name: "a", URL: proxySrv.URL}}, 2)

	var gotCode int
	err := GetStream("http://hf-mirror.com", "/api/models/x", map[string]string{},
		func(resp *http.Response) error {
			gotCode = resp.StatusCode
			return nil
		})
	if err != nil {
		t.Fatalf("GetStream 失败: %v", err)
	}
	if gotCode != http.StatusOK {
		t.Fatalf("状态码 %d", gotCode)
	}
	mu.Lock()
	n := viaProxy
	mu.Unlock()
	if n != 1 {
		t.Fatalf("请求应经过代理出口，实际经过 %d 次", n)
	}
	if !p.Members()[0].Healthy() {
		t.Fatal("成功请求后出口应保持健康")
	}
}

// 端到端：流式传输中途断掉（gost 的 TCP 层健康检查看不见的那类故障）应计入失败。
func TestGetStreamReportsMidStreamFailure(t *testing.T) {
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "partial")
	}))
	defer proxySrv.Close()

	setupConfig(t, "hf-mirror.com", "hf-mirror.com")
	p := setupPool(t, []proxypool.MemberConfig{{Name: "a", URL: proxySrv.URL}}, 1)

	err := GetStream("http://hf-mirror.com", "/x", map[string]string{},
		func(resp *http.Response) error {
			return fmt.Errorf("premature EOF: expected 100 bytes, got 7")
		})
	if err == nil {
		t.Fatal("应把回调错误透出")
	}
	if p.Members()[0].Healthy() {
		t.Fatal("流式中途失败应熔断出口")
	}
}

// 端到端：客户端取消不应熔断出口。
func TestGetStreamIgnoresClientCancel(t *testing.T) {
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data")
	}))
	defer proxySrv.Close()

	setupConfig(t, "hf-mirror.com", "hf-mirror.com")
	p := setupPool(t, []proxypool.MemberConfig{{Name: "a", URL: proxySrv.URL}}, 1)

	err := GetStream("http://hf-mirror.com", "/x", map[string]string{},
		func(resp *http.Response) error {
			return fmt.Errorf("form remote ctx done: %w", context.Canceled)
		})
	if err == nil {
		t.Fatal("应把取消错误透出给调用方")
	}
	if !p.Members()[0].Healthy() {
		t.Fatal("客户端取消不应熔断出口")
	}
}

// 内网目标必须旁路代理：兄弟节点互拉走公网出口必然失败，
// 还会把健康出口误判成坏出口。
func TestGetStreamBypassesInnerDomain(t *testing.T) {
	var viaProxy int
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viaProxy++
		w.WriteHeader(http.StatusOK)
	}))
	defer proxySrv.Close()

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("inner") == "" {
			t.Error("内网请求应带上 inner 标记头")
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "peer-data")
	}))
	defer peer.Close()

	setupConfig(t, "hf-mirror.com", "hf-mirror.com")
	setupPool(t, []proxypool.MemberConfig{{Name: "a", URL: proxySrv.URL}}, 2)

	headers := map[string]string{}
	err := GetStream(peer.URL, "/data", headers, func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("内网直连失败: %v", err)
	}
	if viaProxy != 0 {
		t.Fatalf("内网请求不应经过代理，实际经过 %d 次", viaProxy)
	}
}

func TestBuildPoolConfigFallsBackToLegacyProxy(t *testing.T) {
	prev := config.SysConfig
	config.SysConfig = &config.Config{}
	config.SysConfig.DynamicProxy.HttpProxy = "http://10.0.0.9:1080"
	config.SysConfig.DynamicProxy.HttpProxyName = "旧代理"
	config.SysConfig.DynamicProxy.MaxContinuousFails = 4
	defer func() { config.SysConfig = prev }()

	cfg, ok := buildPoolConfig()
	if !ok {
		t.Fatal("只配了 dynamicProxy 时也应构建单成员池")
	}
	if len(cfg.Members) != 1 || cfg.Members[0].URL != "http://10.0.0.9:1080" {
		t.Fatalf("单成员池内容不对: %+v", cfg.Members)
	}
	if cfg.Members[0].Name != "旧代理" {
		t.Fatalf("应沿用 httpProxyName: %s", cfg.Members[0].Name)
	}
	// 旧配置的 maxContinuousFails 语义一致，应被继承。
	if cfg.FailThreshold != 4 {
		t.Fatalf("应继承 maxContinuousFails，实际 %d", cfg.FailThreshold)
	}
}

func TestBuildPoolConfigDisabled(t *testing.T) {
	prev := config.SysConfig
	config.SysConfig = &config.Config{}
	defer func() { config.SysConfig = prev }()

	if _, ok := buildPoolConfig(); ok {
		t.Fatal("既没配 proxyPool 也没配 httpProxy 时不应启用")
	}
}
