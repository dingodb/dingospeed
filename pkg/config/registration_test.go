package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistrationPersistenceAndIdentity(t *testing.T) {
	c := &Config{}
	c.Server.Repos = t.TempDir()
	r := Registration{Enabled: true, NodeID: "node-a", Address: "scheduler:19091", Host: "speed", Port: 8090}
	if err := c.SaveRegistration(r); err != nil {
		t.Fatal(err)
	}
	next := &Config{}
	next.Server.Repos = c.Server.Repos
	if next.Registration() != r {
		t.Fatal("lost registration on restart")
	}
	r.NodeID = "node-b"
	if next.SaveRegistration(r) == nil {
		t.Fatal("changed bound ID")
	}
	r.NodeID = "node-a"
	r.Enabled = false
	if err := next.SaveRegistration(r); err != nil {
		t.Fatal(err)
	}
	if next.GetOriginSchedulerModel() != "standalone" {
		t.Fatal("disable not applied")
	}
}
func TestRegistrationYAMLBindingAndCorruptFile(t *testing.T) {
	c := &Config{}
	c.Server.Repos = t.TempDir()
	c.Scheduler.Discovery.InstanceId = "legacy"
	if c.SaveRegistration(Registration{NodeID: "other"}) == nil {
		t.Fatal("overrode YAML binding")
	}
	if err := c.SaveRegistration(Registration{NodeID: "legacy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Server.Repos, ".scheduler-registration.json")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".scheduler-registration.json"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := &Config{}
	bad.Server.Repos = dir
	if bad.SaveRegistration(Registration{NodeID: "a"}) == nil {
		t.Fatal("overwrote corrupt binding")
	}
}
func TestRegistrationValidation(t *testing.T) {
	for _, r := range []Registration{{NodeID: "../x"}, {Enabled: true, NodeID: "n", Address: "http://scheduler:90", Host: "speed", Port: 8090}, {Enabled: true, NodeID: "n", Address: "host:99999", Host: "speed", Port: 8090}} {
		if ValidateRegistration(r) == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
}
