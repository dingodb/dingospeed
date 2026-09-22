package dependency

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// PROMTOOL points to a separately installed, trusted promtool executable.
// Samples come from Monitor itself, so this tests the observation-to-alert chain.
func TestPrometheusAlertTimelines(t *testing.T) {
	tool := os.Getenv("PROMTOOL")
	if tool == "" {
		t.Skip("set PROMTOOL to run the actual Prometheus rule engine")
	}
	rules, err := filepath.Abs("../../config/prometheus/dependency_alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(tool, "check", "rules", rules).CombinedOutput(); err != nil {
		t.Fatalf("check rules: %v\n%s", err, out)
	}
	type checkpoint struct {
		sec                  int
		degraded, unresolved bool
	}
	cases := []struct {
		name   string
		id     ID
		events map[int]bool
		checks []checkpoint
	}{
		{"brief_failure_verified_recovery", MetadataRead, map[int]bool{0: false, 5: true, 10: true, 15: true}, []checkpoint{{10, false, false}, {15, false, false}, {180, false, false}}},
		{"failure_then_no_traffic", MetadataRead, map[int]bool{0: false, 5: false, 10: false}, []checkpoint{{35, false, false}, {40, true, false}, {115, true, false}, {120, true, true}, {135, false, true}, {180, false, true}}},
		{"write_failure_read_and_scheduler_healthy", MetadataWrite, map[int]bool{0: false, 5: false, 10: false, 140: true, 145: true, 150: true}, []checkpoint{{40, true, false}, {120, true, true}, {140, false, true}, {145, false, true}, {150, false, false}}},
		{"scheduler_single_failure_no_traffic", Scheduler, map[int]bool{0: false}, []checkpoint{{115, false, false}, {120, false, true}, {125, false, true}, {180, false, true}}},
		{"interrupted_recovery", Scheduler, map[int]bool{0: false, 5: false, 10: false, 45: true, 50: true, 55: false, 60: false, 65: false}, []checkpoint{{40, true, false}, {50, false, false}, {90, false, false}, {95, true, false}, {120, true, true}}},
	}
	deps := []string{"metadata_storage_read", "metadata_storage_write", "scheduler_rpc"}
	impacts := []string{"local_metadata_lookup", "metadata_cache_persistence", "cluster_coordination"}
	var groups []any
	for _, tc := range cases {
		m := &Monitor{}
		var states, unresolved [dependencyCount][]string
		base := time.Unix(1000, 0)
		for sec := 0; sec <= 180; sec += 5 {
			now := base.Add(time.Duration(sec) * time.Second)
			for id := ID(0); id < dependencyCount; id++ {
				if id != tc.id {
					m.Observe(id, true, now)
				} else if success, ok := tc.events[sec]; ok {
					m.Observe(id, success, now)
				}
				s := m.Snapshot(id, now)
				states[id] = append(states[id], fmt.Sprint(int(s.State)))
				v := "0"
				if s.Unresolved {
					v = "1"
				}
				unresolved[id] = append(unresolved[id], v)
			}
		}
		var series, checks []any
		for id := ID(0); id < dependencyCount; id++ {
			labels := fmt.Sprintf(`{job="dingospeed",instance="node-a",dependency="%s",impact="%s"}`, deps[id], impacts[id])
			series = append(series, map[string]any{"series": "dingospeed_dependency_state" + labels, "values": strings.Join(states[id], " ")}, map[string]any{"series": "dingospeed_dependency_unresolved" + labels, "values": strings.Join(unresolved[id], " ")})
			other := strings.Replace(labels, "node-a", "node-b", 1)
			series = append(series, map[string]any{"series": "dingospeed_dependency_state" + other, "values": "1x36"}, map[string]any{"series": "dingospeed_dependency_unresolved" + other, "values": "0x36"})
		}
		for _, check := range tc.checks {
			for i, firing := range []bool{check.degraded, check.unresolved} {
				name := "DingoSpeedDependencyDegraded"
				summary := "Dependency capability affected: " + deps[tc.id]
				description := fmt.Sprintf("Instance node-a has sustained failed operations affecting %s. This does not establish whole-node or mount failure; do not automatically restart.", impacts[tc.id])
				if i == 1 {
					name = "DingoSpeedDependencyUnresolved"
					summary = "Dependency failure lacks recovery evidence: " + deps[tc.id]
					description = fmt.Sprintf("Instance node-a has an unresolved observation affecting %s. Repeated failures or absence of further traffic may explain this; inspect evidence before changing request policy.", impacts[tc.id])
				}
				expected := []any{}
				if firing {
					expected = append(expected, map[string]any{"exp_labels": map[string]string{"job": "dingospeed", "instance": "node-a", "dependency": deps[tc.id], "impact": impacts[tc.id], "severity": "warning"}, "exp_annotations": map[string]string{"summary": summary, "description": description}})
				}
				checks = append(checks, map[string]any{"eval_time": fmt.Sprintf("%ds", check.sec), "alertname": name, "exp_alerts": expected})
			}
		}
		groups = append(groups, map[string]any{"name": tc.name, "interval": "5s", "input_series": series, "alert_rule_test": checks})
	}
	data, err := yaml.Marshal(map[string]any{"rule_files": []string{rules}, "evaluation_interval": "5s", "tests": groups})
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "dependency_timelines.yml")
	if err := os.WriteFile(fixture, data, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(tool, "test", "rules", fixture).CombinedOutput(); err != nil {
		t.Fatalf("timeline test: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
