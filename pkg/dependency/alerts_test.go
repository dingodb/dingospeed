package dependency

import (
	"bytes"
	"gopkg.in/yaml.v3"
	"os"
	"testing"
	"text/template"
)

// Validate the shipped YAML and notification templates locally. A deployment
// must additionally run promtool against its installed Prometheus version.
func TestAlertRuleFileAndTemplates(t *testing.T) {
	contents, err := os.ReadFile("../../config/prometheus/dependency_alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	var rules struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				For         string            `yaml:"for"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&rules); err != nil {
		t.Fatal(err)
	}
	if len(rules.Groups) != 1 || len(rules.Groups[0].Rules) != 2 {
		t.Fatal("missing dependency rules")
	}
	for _, rule := range rules.Groups[0].Rules {
		if rule.For == "" || rule.Labels["severity"] != "warning" {
			t.Fatal("missing persistence or severity")
		}
		for _, value := range rule.Annotations {
			tmpl, err := template.New(rule.Alert).Parse("{{$labels := .}}" + value)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := tmpl.Execute(&out, map[string]string{"dependency": "metadata_storage_read", "impact": "local_metadata_lookup", "instance": "test-node"}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
