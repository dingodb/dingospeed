package storageprobe

import (
	"os"
	"os/exec"
	"testing"
)

func TestProbeAlertRules(t *testing.T) {
	tool := os.Getenv("PROMTOOL")
	if tool == "" {
		t.Skip("set PROMTOOL to validate alert timing with Prometheus")
	}
	for _, args := range [][]string{{"check", "rules", "../../config/prometheus/storage_probe_alerts.yml"}, {"test", "rules", "../../config/prometheus/tests/storage_probe.test.yml"}} {
		out, err := exec.Command(tool, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("promtool: %v\n%s", err, out)
		}
		t.Log(string(out))
	}
}
