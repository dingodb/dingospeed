package util

import (
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestFileAccessMetricsSnapshot(t *testing.T) {
	r := prometheus.NewPedanticRegistry()
	collector := newFileAccessCollector()
	r.MustRegister(collector)
	var duplicate prometheus.AlreadyRegisteredError
	if err := r.Register(newFileAccessCollector()); !errors.As(err, &duplicate) {
		t.Fatalf("expected duplicate descriptor detection: %v", err)
	}
	before := FileAccessObservations()
	ObserveFileAccessFailure("read", &os.PathError{Op: "read", Path: "/private/repo/file", Err: os.ErrPermission})
	ObserveFileAccessFailure("read", &os.PathError{Op: "read", Path: "missing", Err: os.ErrNotExist})
	for scrape := 0; scrape < 2; scrape++ {
		families, err := r.Gather()
		if err != nil {
			t.Fatal(err)
		}
		if len(families) != 1 {
			t.Fatalf("families=%d", len(families))
		}
		f := families[0]
		if f.GetName() != "dingospeed_storage_access_errors_total" || f.GetType() != dto.MetricType_COUNTER || len(f.Metric) != 16 {
			t.Fatalf("unexpected family: %v", f)
		}
		seen := map[string]bool{}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if len(labels) != 2 {
				t.Fatalf("unexpected labels: %v", labels)
			}
			key := labels["operation"] + "/" + labels["kind"]
			if seen[key] {
				t.Fatal("duplicate series")
			}
			seen[key] = true
			found := false
			for _, b := range before {
				if b.Operation == labels["operation"] && string(b.Kind) == labels["kind"] {
					want := b.Count
					if b.Operation == "read" && b.Kind == FileAccessPermissionDenied {
						want++
					}
					if m.Counter.GetValue() != float64(want) {
						t.Fatalf("%s=%v want %d", key, m.Counter.GetValue(), want)
					}
					found = true
				}
			}
			if !found {
				t.Fatalf("unbounded label: %v", labels)
			}
		}
	}
	first := FileAccessObservations()
	_, _ = r.Gather()
	if !reflect.DeepEqual(first, FileAccessObservations()) {
		t.Fatal("scraping changes observation counts")
	}
}

func TestFileAccessMetricsConcurrentScrapes(t *testing.T) {
	r := prometheus.NewPedanticRegistry()
	r.MustRegister(newFileAccessCollector())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				ObserveFileAccessFailure("write", &os.PathError{Op: "write", Path: "p", Err: os.ErrPermission})
				if _, err := r.Gather(); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}
