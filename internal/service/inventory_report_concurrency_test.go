package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"dingospeed/pkg/inventory"
)

func TestRepositoryReportDiscardsScanAcrossCompletedReconciliation(t *testing.T) {
	root := t.TempDir()
	k := inventory.Key{Namespace: "team", RepoType: "models", Repo: "a"}
	seedReportState(t, root, k)
	p, err := buildRepositoryReport(root, k, func() ([]inventory.File, error) {
		if err := inventory.RequestReconcile(root, "new-epoch"); err != nil {
			t.Fatal(err)
		}
		baseline := &inventory.Report{Version: 2, InstanceID: "node", Epoch: "new-epoch", Baseline: true, Files: []inventory.File{}}
		if err := inventory.Update(root, func(q *inventory.State) error {
			q.Cutoff, q.Baseline = q.Sequence, baseline
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// A mutation after the baseline cutoff survives, with its sequence rebased
		// to 1 again. Comparing sequence alone would accept the old-epoch scan.
		if err := inventory.Run(root, k, "publish", nil, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		ack := inventory.Ack{Epoch: baseline.Epoch, Sequence: baseline.Sequence, Digest: baseline.Digest(), Status: "accepted"}
		if err := inventory.Confirm(root, baseline, ack); err != nil {
			t.Fatal(err)
		}
		return []inventory.File{{Key: k, Path: "old", SHA256: "old", Size: 1}}, nil
	})
	if err != nil || p != nil {
		t.Fatalf("old-epoch scan accepted: %+v, %v", p, err)
	}
	q, err := inventory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	e := q.Entries[k.ID()]
	if q.Epoch != "new-epoch" || q.ReconcileEpoch != "" || e == nil || e.Sequence != 1 || e.Report != nil {
		t.Fatalf("new epoch dirty state damaged: %+v", q)
	}
}

func TestRepositoryReportConcurrentBuildersKeepFirstFixedBytes(t *testing.T) {
	root := t.TempDir()
	k := inventory.Key{Namespace: "team", RepoType: "models", Repo: "a"}
	seedReportState(t, root, k)
	scanning, release := make(chan struct{}), make(chan struct{})
	type outcome struct {
		report *inventory.Report
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		p, err := buildRepositoryReport(root, k, func() ([]inventory.File, error) {
			close(scanning)
			<-release
			return []inventory.File{{Key: k, Path: "slow", SHA256: "slow", Size: 4}}, nil
		})
		done <- outcome{p, err}
	}()
	<-scanning
	first, err := buildRepositoryReport(root, k, func() ([]inventory.File, error) {
		return []inventory.File{{Key: k, Path: "first-fixed", SHA256: "first", Size: 5}}, nil
	})
	close(release)
	later := <-done
	if err != nil || later.err != nil || first == nil || later.report == nil {
		t.Fatalf("builders failed: first=%+v %v later=%+v", first, err, later)
	}
	q, err := inventory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(first)
	got, _ := json.Marshal(later.report)
	persisted, _ := json.Marshal(q.Entries[k.ID()].Report)
	if !bytes.Equal(want, got) || !bytes.Equal(want, persisted) {
		t.Fatalf("fixed retry bytes changed: first=%s later=%s stored=%s", want, got, persisted)
	}
}

func seedReportState(t *testing.T, root string, k inventory.Key) {
	t.Helper()
	if err := inventory.Update(root, func(q *inventory.State) error {
		q.NodeID, q.Epoch, q.Sequence = "node", "epoch", 1
		q.Entries[k.ID()] = &inventory.Entry{Key: k, Sequence: 1, Due: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryReportScanDoesNotBlockOtherRepositoryPublication(t *testing.T) {
	root := t.TempDir()
	a := inventory.Key{Namespace: "team", RepoType: "models", Repo: "a"}
	b := inventory.Key{Namespace: "team", RepoType: "models", Repo: "b"}
	seedReportState(t, root, a)
	scanning, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		p, err := buildRepositoryReport(root, a, func() ([]inventory.File, error) {
			close(scanning)
			<-release
			return []inventory.File{}, nil
		})
		if err == nil && (p == nil || p.Sequence != 1) {
			err = errors.New("unrelated publication invalidated report")
		}
		done <- err
	}()
	<-scanning
	published := make(chan error, 1)
	go func() { published <- inventory.Run(root, b, "publish", nil, func() error { return nil }) }()
	select {
	case err := <-published:
		if err != nil {
			close(release)
			<-done
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		close(release)
		<-done
		<-published
		t.Fatal("repository A scan blocked repository B publication")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryReportDiscardsConcurrentMutationAndReconcile(t *testing.T) {
	for _, action := range []string{"publish", "delete", "reconcile", "operation"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			k := inventory.Key{Namespace: "team", RepoType: "models", Repo: "a"}
			seedReportState(t, root, k)
			p, err := buildRepositoryReport(root, k, func() ([]inventory.File, error) {
				var err error
				switch action {
				case "publish", "delete":
					kind := "publish"
					if action == "delete" {
						kind = "delete-repository"
					}
					err = inventory.Run(root, k, kind, nil, func() error { return nil })
				case "operation":
					_ = inventory.Run(root, k, "publish", nil, func() error { return errors.New("interrupted") })
				case "reconcile":
					err = inventory.RequestReconcile(root, "new-epoch")
				}
				if err != nil {
					t.Fatal(err)
				}
				// A racing write may make the scan transiently incomplete. This must
				// not become a persisted report, or a failure for the newer state.
				return nil, errors.New("transient scan failure")
			})
			if err != nil || p != nil {
				t.Fatalf("stale scan result: report=%+v err=%v", p, err)
			}
			q, err := inventory.Read(root)
			if err != nil || q.Entries[k.ID()] == nil || q.Entries[k.ID()].Report != nil {
				t.Fatalf("dirty update lost or stale report saved: %+v, %v", q, err)
			}
			if action == "publish" {
				p, err = buildRepositoryReport(root, k, func() ([]inventory.File, error) {
					return []inventory.File{{Key: k, Path: "new-file", SHA256: "new", Size: 1}}, nil
				})
				if err != nil || p == nil || p.Sequence != 2 || len(p.Files) != 1 {
					t.Fatalf("next pass did not capture new state: %+v, %v", p, err)
				}
			}
		})
	}
}
