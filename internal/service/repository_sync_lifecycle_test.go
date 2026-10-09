package service

import (
	"context"
	"dingospeed/internal/dao"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestRepositorySyncPauseResumeSameManifestAndGeneration(t *testing.T) {
	s, u := newSyncTest(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	s.resolve = func(ctx context.Context, k repository.RepoKey, f dao.LocalManifestFile) ([]SyncSource, error) {
		close(entered)
		<-ctx.Done()
		<-release
		return nil, ctx.Err()
	}
	req := syncReq("lifecycle", syncFile("a", "hello"))
	initial, err := s.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	pausing, err := s.Pause(req.TaskID)
	if err != nil || pausing.State != "pausing" {
		t.Fatalf("pause %+v %v", pausing, err)
	}
	if _, err = s.Resume(req.TaskID); err == nil {
		t.Fatal("resumed before old worker exited")
	}
	if _, err = s.Dismiss(req.TaskID); err == nil {
		t.Fatal("hid active worker")
	}
	close(release)
	paused := waitSync(t, s, req.TaskID)
	if paused.State != "paused" || paused.Error != "" || paused.SnapshotVersion <= initial.SnapshotVersion {
		t.Fatalf("paused %+v", paused)
	}
	if _, _, err = dao.ReadInventorySnapshot(req.key(), req.Revision); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pause changed target: %v", err)
	}
	// Persisted fixed manifest is reused by a fresh service instance.
	next := NewRepositorySyncService(u)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveSyncRange(t, w, r, []byte("hello")) }))
	defer server.Close()
	useSyncSource(next, server.URL)
	resumed, err := next.Resume(req.TaskID)
	if err != nil || resumed.Generation != 2 || resumed.ManifestDigest != initial.ManifestDigest {
		t.Fatalf("resume %+v %v", resumed, err)
	}
	done := waitSync(t, next, req.TaskID)
	if done.State != "completed" {
		t.Fatalf("done %+v", done)
	}
	list, err := next.List(10, "")
	if err != nil || len(list.Items) != 1 || list.Items[0].Repo != "demo" {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err = next.Dismiss(req.TaskID); err != nil {
		t.Fatal(err)
	}
	list, err = next.List(10, "")
	if err != nil || len(list.Items) != 0 {
		t.Fatalf("dismissed visible %+v %v", list, err)
	}
	if _, _, err = dao.ReadInventorySnapshot(req.key(), req.Revision); err != nil {
		t.Fatal("dismiss deleted content", err)
	}
	persisted := NewRepositorySyncService(u)
	st, err := persisted.Status(req.TaskID)
	if err != nil || !st.Dismissed {
		t.Fatalf("dismiss not durable %+v %v", st, err)
	}
}

func TestRepositorySyncCancelWaitsAndCannotResume(t *testing.T) {
	s, _ := newSyncTest(t)
	entered := make(chan struct{})
	exit := make(chan struct{})
	s.resolve = func(ctx context.Context, _ repository.RepoKey, _ dao.LocalManifestFile) ([]SyncSource, error) {
		close(entered)
		<-ctx.Done()
		<-exit
		return nil, ctx.Err()
	}
	r := syncReq("cancelwait", syncFile("a", "x"))
	if _, err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	<-entered
	st, err := s.Cancel(r.TaskID)
	if err != nil || st.State != "canceling" {
		t.Fatalf("cancel %+v %v", st, err)
	}
	st, _ = s.Status(r.TaskID)
	if st.State != "canceling" {
		t.Fatalf("worker still alive: %+v", st)
	}
	close(exit)
	st = waitSync(t, s, r.TaskID)
	if st.State != "cancelled" || st.Error != "" {
		t.Fatalf("cancel result %+v", st)
	}
	if _, err = s.Resume(r.TaskID); err == nil {
		t.Fatal("resumed cancelled task")
	}
}

func TestRepositorySyncRetryFixedBaselineAndStaleWorker(t *testing.T) {
	s, _ := newSyncTest(t)
	s.resolve = func(context.Context, repository.RepoKey, dao.LocalManifestFile) ([]SyncSource, error) {
		return nil, errors.New("offline")
	}
	r := syncReq("retryfixed", syncFile("a", "x"))
	if _, err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	if st := waitSync(t, s, r.TaskID); st.State != "failed" {
		t.Fatal(st)
	}
	old := s.entries[r.TaskID]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveSyncRange(t, w, r, []byte("x")) }))
	defer server.Close()
	useSyncSource(s, server.URL)
	if _, err := s.Retry(r.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := s.transition(old, "failed", errors.New("old worker")); !errors.Is(err, context.Canceled) {
		t.Fatalf("stale worker accepted: %v", err)
	}
	st := waitSync(t, s, r.TaskID)
	if st.State != "completed" || st.Generation != 2 {
		t.Fatalf("retry %+v", st)
	}
}

func TestRepositorySyncFinalizationAndUnknownRejectActions(t *testing.T) {
	s, _ := newSyncTest(t)
	for _, state := range []string{"publishing", "result_unconfirmed", "needs_attention"} {
		r := syncReq("blocked-" + state)
		e := &syncEntry{request: r, status: initializeSyncStatus(SyncStatus{TaskID: r.TaskID, State: state, UpdatedAt: time.Now()}, r)}
		s.entries[r.TaskID] = e
		for _, action := range []func(string) (SyncStatus, error){s.Pause, s.Cancel, s.Resume, s.Retry, s.Dismiss} {
			if _, err := action(r.TaskID); err == nil {
				t.Fatalf("allowed action during %s", state)
			}
		}
	}
}

func TestRepositorySyncResumeRejectsChangedTarget(t *testing.T) {
	s, u := newSyncTest(t)
	r := syncReq("changed-target")
	e := &syncEntry{request: r, fingerprint: digestJSON(r), status: initializeSyncStatus(SyncStatus{TaskID: r.TaskID, State: "paused"}, r)}
	s.entries[r.TaskID] = e
	if err := util.WriteDataToFileAtomic(s.taskPath(r.TaskID, "manifest"), r); err != nil {
		t.Fatal(err)
	}
	seedSync(t, u, "release", "new", "elsewhere")
	st, err := s.Resume(r.TaskID)
	if err == nil || st.State != "conflict" {
		t.Fatalf("changed baseline accepted: %+v %v", st, err)
	}
}

func TestRepositorySyncExposedVersionsSurviveRestart(t *testing.T) {
	s, u := newSyncTest(t)
	r := syncReq("durable-version")
	if _, err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	waitSync(t, s, r.TaskID)
	e := s.entries[r.TaskID]
	s.mu.Lock()
	e.status.SnapshotVersion += 100
	e.status.TransferredBytes = 42
	s.mu.Unlock()
	before, err := s.Status(r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := os.Stat(s.taskPath(r.TaskID, "status"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Status(r.TaskID); err != nil {
		t.Fatal(err)
	}
	b, _ := os.Stat(s.taskPath(r.TaskID, "status"))
	if !a.ModTime().Equal(b.ModTime()) {
		t.Fatal("unchanged polling rewrote status")
	}
	after, err := NewRepositorySyncService(u).Status(r.TaskID)
	if err != nil || after.SnapshotVersion < before.SnapshotVersion || after.TransferredBytes != 42 {
		t.Fatalf("version regressed %+v %+v %v", before, after, err)
	}
}

func TestRepositorySyncIntentionalStopDoesNotMaskRealFailure(t *testing.T) {
	s, _ := newSyncTest(t)
	r := syncReq("true-failure")
	e := &syncEntry{request: r, status: initializeSyncStatus(SyncStatus{TaskID: r.TaskID, State: "pausing"}, r)}
	s.entries[r.TaskID] = e
	if err := s.transition(e, "failed", errors.New("disk write failed")); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(r.TaskID)
	if err != nil || st.Error != "disk write failed" || st.State != "failed" {
		t.Fatalf("failure masked %+v %v", st, err)
	}
	if got := decorateSyncStatus(SyncStatus{State: "paused", Error: "model.bin: context canceled"}); got.Error != "" {
		t.Fatal("legacy intentional error exposed")
	}
}
