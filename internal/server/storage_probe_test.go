package server

import (
	"context"
	"testing"

	"dingospeed/pkg/config"
	"github.com/prometheus/client_golang/prometheus"
)

func TestStorageProbeDisabledHasNoSideEffects(t *testing.T) {
	c := &config.Config{}
	c.Server.Repos = "invalid/unused/root"
	s := NewStorageProbeServer(c)
	registry := prometheus.NewPedanticRegistry()
	s.registry = registry
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.probe != nil {
		t.Fatal("disabled probe started")
	}
	metrics, err := registry.Gather()
	if err != nil || len(metrics) != 2 {
		t.Fatal("disabled probe should only register memory-only impact metrics")
	}
	for _, f := range metrics {
		if f.GetName() == "dingospeed_storage_impact_probe_enabled" && f.Metric[0].Gauge.GetValue() != 0 {
			t.Fatal("disabled probe reported enabled")
		}
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStorageProbeLifecycle(t *testing.T) {
	c := &config.Config{}
	c.StorageProbe.Enabled = true
	c.Server.Repos = t.TempDir() // No sentinel: Start must not perform filesystem I/O.
	s := NewStorageProbeServer(c)
	registry := prometheus.NewPedanticRegistry()
	s.registry = registry
	defer s.Stop(context.Background())
	for i := 0; i < 2; i++ {
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	metrics, err := registry.Gather()
	if err != nil || len(metrics) != 6 {
		t.Fatalf("unexpected collector registration: %d %v", len(metrics), err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}
