package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/pkg/config"
	"dingospeed/pkg/util"
)

func pendingPublication(t *testing.T, s *RepositorySyncService, r SyncRequest) *syncEntry {
	t.Helper()
	r.Files = append([]dao.LocalManifestFile{}, r.Files...)
	id := digestJSON(r)
	e := &syncEntry{request: r, fingerprint: id, executionID: executionFingerprint(r), status: SyncStatus{TaskID: r.TaskID, State: "publishing", PublicationID: id, ManifestDigest: digestJSON(r.Files), TotalFiles: len(r.Files)}}
	if err := util.WriteDataToFileAtomic(s.taskPath(r.TaskID, "manifest"), r); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteDataToFileAtomic(s.taskPath(r.TaskID, "status"), e.status); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSyncRecoveryStartupPreservesLaterRevision(t *testing.T) {
	s, u := newSyncTest(t)
	seedSync(t, u, "dev", "a", "payload")
	r := syncReq("lost-status", syncFile("a", "payload"))
	e := pendingPublication(t, s, r)
	published, err := u.PublishTree(s.publication(e))
	if err != nil {
		t.Fatal(err)
	}
	later := seedSync(t, u, "release", "b", "later edit")
	// Simulate losing the task completion write and restarting the service.
	restarted := NewRepositorySyncService(u)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); restarted.RunRecovery(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := restarted.Status(r.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == "completed" {
			if st.Commit != published.Commit {
				t.Fatalf("wrong historical commit: %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup did not recover: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	current, _, err := dao.ReadInventorySnapshot(r.key(), r.Revision)
	if err != nil || current != later {
		t.Fatalf("later edit changed: %s %v", current, err)
	}
}

func TestSyncRecoveryNoIntentDoesNotPublish(t *testing.T) {
	s, _ := newSyncTest(t)
	r := syncReq("before-publication")
	e := pendingPublication(t, s, r)
	s.recoverPublication(e)
	if st := s.snapshot(e); st.State != "interrupted" {
		t.Fatalf("%+v", st)
	}
	if _, _, err := dao.ReadInventorySnapshot(r.key(), r.Revision); !os.IsNotExist(err) {
		t.Fatalf("unexpected publication: %v", err)
	}
}

func TestSyncRecoveryStatusIsReadOnlyAndManualRetry(t *testing.T) {
	s, u := newSyncTest(t)
	r := syncReq("retry-result")
	e := pendingPublication(t, s, r)
	if _, err := u.PublishTree(s.publication(e)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.SysConfig.Repos(), ".upload-inventory", "publications", e.status.PublicationID+".json")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	s.entries[r.TaskID] = e
	for i := 0; i < maxSyncRecoveryAttempts; i++ {
		s.recoverPublication(e)
	}
	st, err := s.Status(r.TaskID)
	if err != nil || st.State != "needs_attention" || st.RecoveryAttempts != maxSyncRecoveryAttempts {
		t.Fatalf("%+v %v", st, err)
	}
	if err := os.WriteFile(path, good, 0600); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status(r.TaskID)
	if st.State != "needs_attention" {
		t.Fatal("status query performed recovery")
	}
	st, err = s.Reconcile(r.TaskID)
	if err != nil || st.State != "completed" || st.Error != "" {
		t.Fatalf("manual inspection failed: %+v %v", st, err)
	}
}

func TestSyncRecoveryLegacyCannotInferPublicationFromEqualContent(t *testing.T) {
	s, _ := newSyncTest(t)
	r := syncReq("legacy")
	e := pendingPublication(t, s, r)
	e.status.PublicationID = ""
	s.recoverPublication(e)
	if st := s.snapshot(e); st.State != "needs_attention" {
		t.Fatalf("%+v", st)
	}
}
