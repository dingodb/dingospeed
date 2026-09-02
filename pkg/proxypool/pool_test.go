package proxypool

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func newTestPool(t *testing.T, members []MemberConfig, failThreshold int, cooldown time.Duration) *Pool {
	t.Helper()
	p, err := New(Config{
		Enabled:       true,
		Members:       members,
		FailThreshold: failThreshold,
		Cooldown:      cooldown,
	})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	if p == nil {
		t.Fatal("New() 返回 nil")
	}
	return p
}

// 核心诉求：连续取用必须轮换出口，否则重试还是撞同一条死路。
func TestPickRotatesAcrossMembers(t *testing.T) {
	p := newTestPool(t, []MemberConfig{
		{Name: "a", URL: "http://10.0.0.1:8121"},
		{Name: "b", URL: "http://10.0.0.2:8121"},
		{Name: "c", URL: "http://10.0.0.3:8121"},
	}, 3, time.Minute)

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		m := p.Pick()
		if m == nil {
			t.Fatalf("第 %d 次 Pick 返回 nil", i)
		}
		if seen[m.Name()] {
			t.Fatalf("连续 3 次 Pick 重复命中 %s，重试不会换出口", m.Name())
		}
		seen[m.Name()] = true
	}
}

func TestPickHonorsWeight(t *testing.T) {
	p := newTestPool(t, []MemberConfig{
		{Name: "heavy", URL: "http://10.0.0.1:8121", Weight: 3},
		{Name: "light", URL: "http://10.0.0.2:8121", Weight: 1},
	}, 3, time.Minute)

	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		counts[p.Pick().Name()]++
	}
	if counts["heavy"] != 6 || counts["light"] != 2 {
		t.Fatalf("权重未生效: %v", counts)
	}
}

// HEAD 必须拿到 302 本身而不是跟过去：上层要靠 Location 头解析 CDN 真实地址。
func TestHeadClientDoesNotFollowRedirect(t *testing.T) {
	p := newTestPool(t, []MemberConfig{{Name: "a", URL: "http://10.0.0.1:8121"}}, 3, time.Minute)
	m := p.Members()[0]

	head := m.Client(http.MethodHead)
	if head.CheckRedirect == nil {
		t.Fatal("HEAD 客户端必须阻止跟随重定向")
	}
	if err := head.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect 应返回 ErrUseLastResponse，实际 %v", err)
	}
	if get := m.Client(http.MethodGet); get.CheckRedirect != nil {
		t.Fatal("GET 客户端应保持默认跟随重定向")
	}
	// 两者共用同一个 transport，连接池不重复。
	if head.Transport != m.Client(http.MethodGet).Transport {
		t.Fatal("HEAD 与 GET 客户端应共用 transport")
	}
}

func TestBreakerTripsAndExcludesMember(t *testing.T) {
	p := newTestPool(t, []MemberConfig{
		{Name: "bad", URL: "http://10.0.0.1:8121"},
		{Name: "good", URL: "http://10.0.0.2:8121"},
	}, 2, time.Hour)

	bad := p.Members()[0]
	p.Report(bad, 0, errors.New("EOF"))
	if !bad.Healthy() {
		t.Fatal("1 次失败不应触发熔断（阈值为 2）")
	}
	p.Report(bad, 0, errors.New("EOF"))
	if bad.Healthy() {
		t.Fatal("达到阈值后应熔断")
	}
	if got := p.Available(); got != 1 {
		t.Fatalf("可用成员数应为 1，实际 %d", got)
	}
	for i := 0; i < 5; i++ {
		if m := p.Pick(); m.Name() != "good" {
			t.Fatalf("熔断成员仍被选中: %s", m.Name())
		}
	}
}

func TestSuccessResetsFailStreak(t *testing.T) {
	p := newTestPool(t, []MemberConfig{{Name: "a", URL: "http://10.0.0.1:8121"}}, 3, time.Hour)
	m := p.Members()[0]
	p.Report(m, 0, errors.New("boom"))
	p.Report(m, 0, errors.New("boom"))
	p.Report(m, http.StatusOK, nil)
	p.Report(m, 0, errors.New("boom"))
	if !m.Healthy() {
		// 失败必须是「连续」的才熔断，中间成功一次就该清零。
		t.Fatal("中途成功后失败计数未清零")
	}
}

func TestHalfOpenAfterCooldown(t *testing.T) {
	p := newTestPool(t, []MemberConfig{{Name: "a", URL: "http://10.0.0.1:8121"}}, 1, 30*time.Millisecond)
	m := p.Members()[0]
	p.Report(m, 0, errors.New("boom"))
	if p.Pick() != nil {
		t.Fatal("熔断期内不应放行")
	}
	time.Sleep(50 * time.Millisecond)
	if p.Pick() == nil {
		t.Fatal("冷却结束后应放行半开试探请求")
	}
	// 半开试探成功即完全恢复。
	p.Report(m, http.StatusOK, nil)
	if !m.Healthy() {
		t.Fatal("半开试探成功后应恢复")
	}
}

func TestPoolExhaustedReturnsNil(t *testing.T) {
	p := newTestPool(t, []MemberConfig{
		{Name: "a", URL: "http://10.0.0.1:8121"},
		{Name: "b", URL: "http://10.0.0.2:8121"},
	}, 1, time.Hour)
	for _, m := range p.Members() {
		p.Report(m, 0, errors.New("boom"))
	}
	if p.Pick() != nil {
		t.Fatal("全池熔断时应返回 nil，交由调用方回退直连")
	}
}

func TestIsFailure(t *testing.T) {
	cases := []struct {
		name string
		code int
		err  error
		want bool
	}{
		{"传输错误", 0, errors.New("EOF"), true},
		{"429 限流", http.StatusTooManyRequests, nil, true},
		{"502 网关错误", http.StatusBadGateway, nil, true},
		{"503 不可用", http.StatusServiceUnavailable, nil, true},
		{"200 正常", http.StatusOK, nil, false},
		{"302 跳转", http.StatusFound, nil, false},
		// 404 是内容不存在，跟出口好坏无关。
		{"404 内容不存在", http.StatusNotFound, nil, false},
		// 401/403 是 gated 仓库的权限拒绝。若计入失败，
		// 用户拉几次 gated 模型就会熔断一个完全健康的出口。
		{"401 未授权", http.StatusUnauthorized, nil, false},
		{"403 gated 仓库", http.StatusForbidden, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsFailure(c.code, c.err); got != c.want {
				t.Fatalf("IsFailure(%d, %v) = %v, want %v", c.code, c.err, got, c.want)
			}
		})
	}
}

func TestShouldBypass(t *testing.T) {
	p, err := New(Config{
		Enabled: true,
		Members: []MemberConfig{{Name: "a", URL: "http://10.0.0.1:8121"}},
		NoProxy: []string{"svc.cluster.local"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   string
		want bool
	}{
		{"http://127.0.0.1:8091", true},
		{"http://10.201.146.65:8090", true},
		{"http://192.168.1.10:8090", true},
		{"http://172.16.5.4:8090", true},
		{"http://localhost:8091", true},
		{"http://dingospeed.svc.cluster.local:8090", true},
		{"https://huggingface.co", false},
		{"https://hf-mirror.com", false},
		{"https://cas-bridge.xethub.hf.co", false},
		{"", false},
	}
	for _, c := range cases {
		if got := p.ShouldBypass(c.in); got != c.want {
			t.Fatalf("ShouldBypass(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// 池未启用时全部方法必须对 nil 安全，调用方不该到处判空。
func TestNilPoolIsSafe(t *testing.T) {
	var p *Pool
	if p.Pick() != nil || p.Available() != 0 || p.Size() != 0 || p.ShouldBypass("https://huggingface.co") {
		t.Fatal("nil 池的行为不符合预期")
	}
	p.Report(nil, 200, nil)
	p.Close()
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{Enabled: true, Members: []MemberConfig{{Name: "a", URL: ""}}}); err == nil {
		t.Fatal("空 url 应报错")
	}
	if _, err := New(Config{Enabled: true, Members: []MemberConfig{{Name: "a", URL: "10.0.0.1:8121"}}}); err == nil {
		t.Fatal("缺少 scheme 的 url 应报错")
	}
	if _, err := New(Config{Enabled: true, Members: []MemberConfig{
		{Name: "dup", URL: "http://10.0.0.1:8121"},
		{Name: "dup", URL: "http://10.0.0.2:8121"},
	}}); err == nil {
		t.Fatal("重名成员应报错")
	}
	p, err := New(Config{Enabled: false, Members: []MemberConfig{{Name: "a", URL: "http://10.0.0.1:8121"}}})
	if err != nil || p != nil {
		t.Fatal("未启用时应返回 (nil, nil)")
	}
}
