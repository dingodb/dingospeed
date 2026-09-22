package handler

import (
	"dingospeed/pkg/config"
	"encoding/json"
	"github.com/labstack/echo/v4"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSchedulerRegistrationWithoutCredentials(t *testing.T) {
	old := config.SysConfig
	defer func() { config.SysConfig = old }()
	cfg := &config.Config{}
	// Repos uses the configured server repo root.
	cfg.Server.Repos = t.TempDir()
	config.SysConfig = cfg
	t.Setenv("DINGO_NODE_MANAGEMENT_TOKEN", "ignored-old-token")
	body := `{"enabled":true,"nodeId":"external-node","address":"scheduler:19091","host":"speed","port":8090,"managementUrl":"http://speed:8091","downloadUrl":"http://speed:8090"}`
	for _, method := range []string{"PUT", "GET"} {
		req := httptest.NewRequest(method, "/api/scheduler-registration", strings.NewReader(body))
		rec := httptest.NewRecorder()
		if err := SchedulerRegistration(echo.New().NewContext(req, rec)); err != nil || rec.Code != 200 {
			t.Fatalf("%s: %d %s %v", method, rec.Code, rec.Body.String(), err)
		}
		var out struct{ Config config.Registration }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Config.ManagementURL != "http://speed:8091" {
			t.Fatalf("readback: %s %v", rec.Body.String(), err)
		}
	}
}
