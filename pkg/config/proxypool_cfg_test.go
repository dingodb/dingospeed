package config

import "testing"

func TestScanParsesProxyPool(t *testing.T) {
	c, err := Scan("../../config/config.yaml")
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	if c.ProxyPool.ProbeInterval != 60 || c.ProxyPool.Cooldown != 300 || c.ProxyPool.FailThreshold != 3 {
		t.Fatalf("proxyPool 字段未正确解析: %+v", c.ProxyPool)
	}
	if c.ProxyPool.FallbackDirect == nil || !*c.ProxyPool.FallbackDirect {
		t.Fatal("fallbackDirect 未解析为 true")
	}
	if c.ProxyPool.Enabled {
		t.Fatal("默认应为关闭")
	}
	if !c.GetProxyPoolFallbackDirect() {
		t.Fatal("GetProxyPoolFallbackDirect 应为 true")
	}
}
