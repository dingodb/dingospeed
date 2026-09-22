package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestScanModelScopeOptionalConfiguration(t *testing.T) {
	raw, err := os.ReadFile("../../config/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var base map[string]interface{}
	if err := yaml.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	old, oldInfo := SysConfig, SystemInfo
	t.Cleanup(func() { SysConfig, SystemInfo = old, oldInfo })
	for _, tc := range []struct {
		name      string
		override  interface{}
		wantURL   string
		wantRetry int
		invalid   bool
	}{
		{"omitted", nil, "https://www.modelscope.cn", 5, false},
		{"empty", map[string]interface{}{}, "https://www.modelscope.cn", 5, false},
		{"partial", map[string]interface{}{"officialBaseURL": "http://localhost:9000"}, "http://localhost:9000", 5, false},
		{"explicit", map[string]interface{}{"officialBaseURL": "https://modelscope.cn", "maxRetry": 2, "retryDelay": 7, "chunkSize": 4096}, "https://modelscope.cn", 2, false},
		{"negative retries", map[string]interface{}{"maxRetry": -1}, "", 0, true},
		{"negative delay", map[string]interface{}{"retryDelay": -1}, "", 0, true},
		{"negative chunk", map[string]interface{}{"chunkSize": -1}, "", 0, true},
		{"relative URL", map[string]interface{}{"officialBaseURL": "modelscope.cn"}, "", 0, true},
		{"unsupported scheme", map[string]interface{}{"officialBaseURL": "ftp://modelscope.cn"}, "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delete(base, "modelscope")
			if tc.override != nil {
				base["modelscope"] = tc.override
			}
			data, err := yaml.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Scan(path)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "modelscope") {
					t.Fatalf("expected actionable startup error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Modelscope.OfficialBaseURL != tc.wantURL || cfg.Modelscope.MaxRetry != tc.wantRetry || cfg.Modelscope.ChunkSize <= 0 || cfg.Modelscope.RetryDelay <= 0 {
				t.Fatalf("unexpected provider config: %+v", cfg.Modelscope)
			}
			if tc.name == "explicit" && (cfg.Modelscope.RetryDelay != 7 || cfg.Modelscope.ChunkSize != 4096) {
				t.Fatalf("override lost: %+v", cfg.Modelscope)
			}
		})
	}
}
