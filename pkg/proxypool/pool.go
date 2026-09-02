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

// Package proxypool 维护一组可用的出口代理（gost 集群等），对下载链路提供
// “每次取用即轮换”的选路能力，并按应用层信号对坏出口做熔断与半开恢复。
//
// 为什么需要它：gost 自带的负载均衡只在 TCP/CONNECT 层做健康检查，
// 隧道建立成功但内部 HTTP 传输超时、被限速、被墙的情况它一概看不见。
// 应用层的健康判定只能放在 dingospeed 里。
package proxypool

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MemberConfig 单个出口代理的静态配置。
type MemberConfig struct {
	Name   string `json:"name" yaml:"name"`
	URL    string `json:"url" yaml:"url"`
	Weight int    `json:"weight" yaml:"weight"`
}

// Config 代理池配置。
type Config struct {
	Enabled       bool
	Members       []MemberConfig
	ProbeTarget   string
	ProbeInterval time.Duration
	ProbeTimeout  time.Duration
	FailThreshold int
	Cooldown      time.Duration
	DialTimeout   time.Duration
	// ResponseHeaderTimeout 等响应头的上限。这是识别「隧道建起来了但对端不吐数据」
	// 这类故障的主要手段，不能为 0。
	ResponseHeaderTimeout time.Duration
	// ReqTimeout 为成员客户端的整体超时，0 表示不限制（大文件流式下载需要 0）。
	ReqTimeout time.Duration
	// NoProxy 额外的免代理后缀（域名或 IP 前缀），私有网段已默认旁路。
	NoProxy []string
}

// Member 一个出口代理，自带独立的 http.Client 与熔断状态。
type Member struct {
	name     string
	rawURL   string
	weight   int
	proxyURL *url.URL
	client   *http.Client
	// headClient 单独一份：HEAD 必须阻止跟随重定向，
	// 上层要靠 302 的 Location 头拿 CDN 真实地址，跟过去就拿不到了。
	headClient *http.Client

	mu        sync.Mutex
	fails     int       // 连续失败次数
	openUntil time.Time // 熔断到期时刻，零值表示未熔断

	okTotal   atomic.Int64
	failTotal atomic.Int64
}

func (m *Member) Name() string { return m.name }

// Client 返回该出口对应方法的客户端。
func (m *Member) Client(method string) *http.Client {
	if method == http.MethodHead {
		return m.headClient
	}
	return m.client
}

// tryAcquire 判断此刻能否使用该成员；熔断到期时放行一个半开试探请求。
// 半开的互斥靠把 openUntil 顺延一个冷却周期实现：拿到试探资格的那个调用
// 会把窗口推到未来，后续调用因此看到「仍在熔断」而退出，无需额外的标志位。
// 顺延同时保证了试探请求即使永不返回，下个周期也会再试，不会卡死。
func (m *Member) tryAcquire(now time.Time, cooldown time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.openUntil.IsZero() {
		return true
	}
	if now.After(m.openUntil) {
		m.openUntil = now.Add(cooldown)
		return true
	}
	return false
}

func (m *Member) markOK() {
	m.okTotal.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fails = 0
	m.openUntil = time.Time{}
}

func (m *Member) markFail(threshold int, cooldown time.Duration) {
	m.failTotal.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fails++
	if m.fails < threshold {
		return
	}
	// 只在熔断窗口尚未打开（或已到期）时开一个新窗口。
	// 熔断期内陆续返回的在途失败不再顺延窗口，否则实际冷却时间会远超配置值。
	now := time.Now()
	if m.openUntil.IsZero() || now.After(m.openUntil) {
		m.openUntil = now.Add(cooldown)
	}
}

// Healthy 报告成员当前是否可用（未处于熔断打开状态）。
func (m *Member) Healthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openUntil.IsZero() || time.Now().After(m.openUntil)
}

// Pool 代理池。
type Pool struct {
	cfg     Config
	members []*Member
	ring    []*Member // 按权重展开的轮询环
	cursor  atomic.Uint64
	noProxy []string
	stopCh  chan struct{}
	stopOne sync.Once
}

// New 构建代理池。members 为空或 Enabled=false 时返回 nil，调用方据此回退到旧逻辑。
func New(cfg Config) (*Pool, error) {
	if !cfg.Enabled || len(cfg.Members) == 0 {
		return nil, nil
	}
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = 3
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 5 * time.Minute
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.ProbeInterval <= 0 {
		cfg.ProbeInterval = time.Minute
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 10 * time.Second
	}
	if cfg.ResponseHeaderTimeout <= 0 {
		cfg.ResponseHeaderTimeout = 60 * time.Second
	}

	p := &Pool{cfg: cfg, stopCh: make(chan struct{})}
	for _, s := range cfg.NoProxy {
		if s = strings.TrimSpace(strings.ToLower(s)); s != "" {
			p.noProxy = append(p.noProxy, s)
		}
	}

	seen := make(map[string]struct{}, len(cfg.Members))
	for i, mc := range cfg.Members {
		raw := strings.TrimSpace(mc.URL)
		if raw == "" {
			return nil, fmt.Errorf("proxypool: 第 %d 个成员缺少 url", i+1)
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("proxypool: 成员 %s 的 url 无法解析: %w", raw, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("proxypool: 成员 %s 的 url 需形如 http://host:port", raw)
		}
		name := strings.TrimSpace(mc.Name)
		if name == "" {
			name = u.Host
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("proxypool: 成员名重复: %s", name)
		}
		seen[name] = struct{}{}

		weight := mc.Weight
		if weight <= 0 {
			weight = 1
		}
		transport := newTransport(u, cfg)
		m := &Member{
			name:     name,
			rawURL:   raw,
			weight:   weight,
			proxyURL: u,
			client:   &http.Client{Timeout: cfg.ReqTimeout, Transport: transport},
			headClient: &http.Client{
				Timeout:   cfg.ReqTimeout,
				Transport: transport,
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse // 阻止跟随重定向
				},
			},
		}
		p.members = append(p.members, m)
		for i := 0; i < weight; i++ {
			p.ring = append(p.ring, m)
		}
	}
	return p, nil
}

// newTransport 每个出口一份 transport，成员之间连接池互不干扰。
func newTransport(proxyURL *url.URL, cfg Config) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyURL(proxyURL),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   cfg.DialTimeout,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		DialContext: (&net.Dialer{
			Timeout:   cfg.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
}

// Pick 取一个可用成员。每次调用都推进游标，因此同一请求的多次重试
// 会自动落到不同出口——这正是修复 "#1 EOF #2 EOF #3 EOF" 的关键。
// 全部成员都在熔断中时返回 nil，调用方应回退直连。
func (p *Pool) Pick() *Member {
	if p == nil {
		return nil
	}
	n := len(p.ring)
	if n == 0 {
		return nil
	}
	now := time.Now()
	start := p.cursor.Add(1) - 1
	for i := 0; i < n; i++ {
		m := p.ring[(start+uint64(i))%uint64(n)]
		if m.tryAcquire(now, p.cfg.Cooldown) {
			return m
		}
	}
	return nil
}

// Report 上报一次请求结果。statusCode 为 0 表示没拿到响应。
func (p *Pool) Report(m *Member, statusCode int, err error) {
	if p == nil || m == nil {
		return
	}
	// Report 位于每个数据块的下载热路径上，这里只做计数器自增（无锁），
	// 健康度 gauge 的刷新交给探活循环，不在热路径上遍历全部成员。
	if IsFailure(statusCode, err) {
		m.markFail(p.cfg.FailThreshold, p.cfg.Cooldown)
		observe(m, false)
	} else {
		m.markOK()
		observe(m, true)
	}
}

// IsFailure 判定一次请求是否算出口故障。
//
// 只认三类信号：传输层错误、429 限流、5xx 服务端错误。
//
// 刻意不计入 4xx 中的鉴权类状态码：HF 对 gated 仓库返回 401/403，
// 这是「用户没有该资源的权限」而不是「出口坏了」（见 remote_task.go 对
// 401/403 的处理）。把它算作故障会导致用户拉几次 gated 模型就熔断一个
// 健康出口。被墙的出口同样可能返回 403，但这两种情况在响应里无法可靠
// 区分，宁可漏判也不能误杀——漏判由传输错误和 5xx 兜底。
func IsFailure(statusCode int, err error) bool {
	if err != nil {
		return true
	}
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

// Available 返回当前未熔断的成员数。
func (p *Pool) Available() int {
	if p == nil {
		return 0
	}
	n := 0
	for _, m := range p.members {
		if m.Healthy() {
			n++
		}
	}
	return n
}

// Size 返回成员总数。
func (p *Pool) Size() int {
	if p == nil {
		return 0
	}
	return len(p.members)
}

// Members 返回成员列表（只读用途）。
func (p *Pool) Members() []*Member {
	if p == nil {
		return nil
	}
	return p.members
}

// ShouldBypass 判断目标是否应绕过代理直连。
// 私有网段/回环必须旁路：兄弟节点互拉、gRPC 连 scheduler、
// local-upload 回环这些流量走公网代理必然失败。
func (p *Pool) ShouldBypass(rawURL string) bool {
	host := hostOf(rawURL)
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if p == nil {
		return false
	}
	for _, s := range p.noProxy {
		if host == s || strings.HasSuffix(host, "."+strings.TrimPrefix(s, ".")) {
			return true
		}
	}
	return false
}

func hostOf(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "//") {
		s = "//" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		// 形如 "127.0.0.1:8091" 且解析失败时兜底
		if h, _, e := net.SplitHostPort(strings.TrimPrefix(rawURL, "//")); e == nil {
			host = h
		}
	}
	return strings.ToLower(host)
}

// Close 停止后台探活。
func (p *Pool) Close() {
	if p == nil {
		return
	}
	p.stopOne.Do(func() { close(p.stopCh) })
}
