package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"

	"dingospeed/pkg/util"
)

func newSyncTest(t *testing.T) (*RepositorySyncService, *UploadService) {
	t.Helper()
	old := config.SysConfig
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir()}, Download: config.Download{BlockSize: 1024, RespChunkSize: 1024, RespChanSize: 4, GoroutineMaxNumPerFile: 2, RemoteFileRangeSize: 2048}, Upload: config.Upload{Namespace: repository.Local, ConcurrentLimit: 4}, Scheduler: config.Scheduler{Mode: "cluster"}}
	t.Cleanup(func() { config.SysConfig = old })
	t.Setenv("DINGO_SYNC_KEY", "")
	b := data.NewBaseData()
	lock := dao.NewLockDao(b)
	u := NewUploadService(dao.NewUploadDao(dao.NewFileDao(nil, b, lock), lock))
	return NewRepositorySyncService(u), u
}
func syncFile(path, content string) dao.LocalManifestFile {
	sum := sha256.Sum256([]byte(content))
	return dao.LocalManifestFile{Path: path, Sha256: hex.EncodeToString(sum[:]), Size: int64(len(content))}
}
func syncReq(id string, files ...dao.LocalManifestFile) SyncRequest {
	return SyncRequest{TaskID: id, Namespace: repository.Local, RepoType: "models", Repo: "demo", Revision: "release", ExpectedAbsent: true, Files: files}
}
func seedSync(t *testing.T, u *UploadService, revision, path, content string) string {
	t.Helper()
	f := syncFile(path, content)
	r, err := u.UploadWholeFile(dao.LocalUploadParam{Namespace: repository.Local, RepoType: "models", Repo: "demo", Revision: revision, FilePath: path, Sha256: f.Sha256, Overwrite: true}, fmt.Sprint(f.Size), "", bytes.NewBufferString(content))
	if err != nil {
		t.Fatal(err)
	}
	return r.Commit
}
func waitSync(t *testing.T, s *RepositorySyncService, id string) SyncStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		active := len(s.active)
		s.mu.Unlock()
		if !syncActive(st.State) && active == 0 {
			return st
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("sync did not stop")
	return SyncStatus{}
}
func useSyncSource(s *RepositorySyncService, url string) {
	s.resolve = func(context.Context, repository.RepoKey, dao.LocalManifestFile) ([]SyncSource, error) {
		return []SyncSource{{InstanceID: "source", ManagementURL: url}}, nil
	}
}

func serveSyncRange(t *testing.T, w http.ResponseWriter, r *http.Request, payload []byte) {
	t.Helper()
	if r.Header.Get("Range") == "" {
		t.Error("sync transfer did not use a Range request")
	}
	http.ServeContent(w, r, "payload", time.Time{}, bytes.NewReader(payload))
}

func TestRepositorySyncExactReplacementAndReceipt(t *testing.T) {
	s, u := newSyncTest(t)
	base := seedSync(t, u, "release", "extra.txt", "keep in dev")
	_, err := u.PublishFiles(dao.LocalPublishParam{Namespace: repository.Local, RepoType: "models", Repo: "demo", Revision: "dev", Files: []dao.LocalManifestFile{syncFile("extra.txt", "keep in dev")}})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected Authorization on peer request")
		}
		if r.URL.Query().Get("path") != "new.txt" {
			t.Error("wrong identity")
		}
		serveSyncRange(t, w, r, []byte("new payload"))
	}))
	defer peer.Close()
	useSyncSource(s, peer.URL)
	r := syncReq("replace", syncFile("new.txt", "new payload"))
	r.ExpectedAbsent = false
	r.BaseCommit = base
	if _, err = s.Start(r); err != nil {
		t.Fatal(err)
	}
	st := waitSync(t, s, r.TaskID)
	if st.State != "completed" {
		t.Fatalf("%+v", st)
	}
	_, files, err := dao.ReadInventorySnapshot(r.key(), "release")
	if err != nil || len(files) != 1 || files[0] != r.Files[0] {
		t.Fatalf("wrong target: %+v %v", files, err)
	}
	dev, _, err := dao.ReadInventorySnapshot(r.key(), "dev")
	if err != nil || dev != base {
		t.Fatalf("dev changed: %s %v", dev, err)
	}
	if _, err = s.Start(r); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate request retransferred")
	}
	restarted := NewRepositorySyncService(u)
	st, err = restarted.Status(r.TaskID)
	if err != nil || st.State != "completed" {
		t.Fatalf("receipt not persisted: %+v %v", st, err)
	}
	r.Files = []dao.LocalManifestFile{syncFile("other.txt", "other")}
	if _, err = s.Start(r); err == nil {
		t.Fatal("task id reused for changed request")
	}
}

func TestRepositorySyncConcurrentCreationDoesNotOverwrite(t *testing.T) {
	s, u := newSyncTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		serveSyncRange(t, w, r, []byte("payload"))
	}))
	defer peer.Close()
	useSyncSource(s, peer.URL)
	r := syncReq("creation-race", syncFile("a", "payload"))
	if _, err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	<-entered
	other := r
	other.TaskID = "same-content"
	if st, err := s.Start(other); err != nil || st.TaskID != r.TaskID {
		t.Fatalf("identical active request not reused: %+v %v", st, err)
	}
	other.TaskID = "competing-task"
	other.Files = []dao.LocalManifestFile{syncFile("different", "different")}
	if _, err := s.Start(other); err == nil {
		t.Fatal("parallel revision accepted")
	}
	concurrent := seedSync(t, u, "release", "someone.txt", "concurrent")
	close(release)
	st := waitSync(t, s, r.TaskID)
	if st.State != "conflict" {
		t.Fatalf("%+v", st)
	}
	commit, _, _ := dao.ReadInventorySnapshot(r.key(), r.Revision)
	if commit != concurrent {
		t.Fatal("concurrent revision overwritten")
	}
}

func TestRepositorySyncPartialReceiptCannotChangeManifest(t *testing.T) {
	s, _ := newSyncTest(t)
	r := syncReq("partial")
	if err := util.WriteDataToFileAtomic(s.taskPath(r.TaskID, "manifest"), r); err != nil {
		t.Fatal(err)
	}
	changed := r
	changed.Files = []dao.LocalManifestFile{syncFile("a", "different")}
	if _, err := s.Start(changed); err == nil {
		t.Fatal("overwrote partial receipt with different request")
	}
	if st, err := s.Start(r); err != nil || st.State != "interrupted" {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestRepositorySyncFailureAndCancellationPreserveRevision(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			s, u := newSyncTest(t)
			base := seedSync(t, u, "release", "old", "old")
			payload := bytes.Repeat([]byte("correct"), 1024)
			corrupt := bytes.Repeat([]byte("corrupt"), 1024)
			entered := make(chan struct{})
			var once sync.Once
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(entered) })
				if cancel {
					<-r.Context().Done()
					return
				}
				serveSyncRange(t, w, r, corrupt)
			}))
			defer peer.Close()
			useSyncSource(s, peer.URL)
			r := syncReq("failure", syncFile("a", string(payload)))
			r.ExpectedAbsent = false
			r.BaseCommit = base
			if _, err := s.Start(r); err != nil {
				t.Fatal(err)
			}
			<-entered
			if cancel {
				if _, err := s.Cancel(r.TaskID); err != nil {
					t.Fatal(err)
				}
			}
			st := waitSync(t, s, r.TaskID)
			want := "failed"
			if cancel {
				want = "cancelled"
			}
			if st.State != want {
				t.Fatalf("%+v", st)
			}
			commit, _, _ := dao.ReadInventorySnapshot(r.key(), r.Revision)
			if commit != base {
				t.Fatal("failed transfer changed revision")
			}
		})
	}
}

func TestRepositorySyncReusesLocalContentAndEmptyRevision(t *testing.T) {
	s, u := newSyncTest(t)
	seedSync(t, u, "dev", "a", "cached")
	s.resolve = func(context.Context, repository.RepoKey, dao.LocalManifestFile) ([]SyncSource, error) {
		t.Error("local complete content performed source lookup")
		return nil, errors.New("unreachable")
	}
	r := syncReq("local", syncFile("a", "cached"))
	if _, err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	st := waitSync(t, s, r.TaskID)
	if st.State != "completed" || st.TransferredBytes != 0 {
		t.Fatalf("%+v", st)
	}
	r = syncReq("empty")
	r.ExpectedAbsent = false
	r.BaseCommit = st.Commit
	if _, err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	st = waitSync(t, s, r.TaskID)
	if st.State != "completed" {
		t.Fatalf("%+v", st)
	}
	_, files, err := dao.ReadInventorySnapshot(r.key(), "release")
	if err != nil || len(files) != 0 {
		t.Fatalf("not empty: %+v %v", files, err)
	}
}

func TestRepositorySyncInterruptedReceiptAndStandalone(t *testing.T) {
	s, _ := newSyncTest(t)
	r := syncReq("interrupted")
	if err := util.WriteDataToFileAtomic(s.taskPath(r.TaskID, "manifest"), r); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteDataToFileAtomic(s.taskPath(r.TaskID, "status"), SyncStatus{TaskID: r.TaskID, State: "downloading"}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(r.TaskID)
	if err != nil || st.State != "interrupted" {
		t.Fatalf("%+v %v", st, err)
	}
	// No task enumeration: an unrelated corrupt task cannot break this lookup.
	if err := os.MkdirAll(s.taskPath("unrelated", "bad"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Status(r.TaskID); err != nil {
		t.Fatal(err)
	}
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir()}}
	if _, err = s.Start(syncReq("standalone")); err == nil {
		t.Fatal("standalone accepted")
	}
}

func TestRepositorySyncSchedulerIdentityWithoutKeyAndNoRedirect(t *testing.T) {
	s, _ := newSyncTest(t)
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer peer.Close()
	useSyncSource(s, peer.URL)
	r := syncReq("redirect", syncFile("a", "payload"))
	if _, err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	if st := waitSync(t, s, r.TaskID); st.State != "failed" {
		t.Fatalf("%+v", st)
	}
	if leaked.Load() {
		t.Fatal("followed untrusted redirect")
	}
	// A scheduler lookup is exact and does not require a sync credential.
	lookup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/upload-inventory/sources" || r.URL.Query().Get("sha256") != syncFile("a", "payload").Sha256 || r.Header.Get("Authorization") != "" {
			t.Error("wrong lookup")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []SyncSource{}})
	}))
	defer lookup.Close()
	// Use a fresh config to avoid mutating the persisted registration snapshot.
	old := config.SysConfig
	config.SysConfig = &config.Config{Server: old.Server, Scheduler: config.Scheduler{Mode: "cluster", HTTPURL: lookup.URL}}
	if _, err := s.schedulerSources(context.Background(), r.key(), r.Files[0]); err != nil {
		t.Fatal(err)
	}
}

func TestRepositorySyncCancelledRangesAreReusedByNextTask(t *testing.T) {
	s, _ := newSyncTest(t)
	payload := bytes.Repeat([]byte("range payload "), 2048)
	f := syncFile("large.bin", string(payload))
	var retry atomic.Bool
	var replayedFirst atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, _, err := dao.ParseSyncRange(r.Header.Get("Range"), f.Size)
		if err != nil {
			t.Error(err)
			http.Error(w, "range", 400)
			return
		}
		if !retry.Load() && start != 0 {
			<-r.Context().Done()
			return
		}
		if retry.Load() && start == 0 {
			replayedFirst.Store(true)
		}
		serveSyncRange(t, w, r, payload)
	}))
	defer peer.Close()
	useSyncSource(s, peer.URL)
	request := syncReq("cancel-ranges", f)
	if _, err := s.Start(request); err != nil {
		t.Fatal(err)
	}
	// Wait for a durable first block, not merely bytes received by the socket.
	complete := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		paths, _ := filepath.Glob(filepath.Join(config.SysConfig.Repos(), ".repository-sync", "content", "*.partial"))
		if len(paths) == 1 {
			file, err := os.Open(paths[0])
			if err == nil {
				h := &downloader.DingCacheHeader{}
				if h.Read(file) == nil {
					complete, _ = h.BlockMask.Test(0)
				}
				file.Close()
			}
		}
		if complete {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !complete {
		_, _ = s.Cancel(request.TaskID)
		waitSync(t, s, request.TaskID)
		t.Fatal("first range was not durably cached")
	}
	if _, err := s.Cancel(request.TaskID); err != nil {
		t.Fatal(err)
	}
	if st := waitSync(t, s, request.TaskID); st.State != "cancelled" {
		t.Fatalf("%+v", st)
	}
	if _, _, err := dao.ReadInventorySnapshot(request.key(), request.Revision); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled transfer published: %v", err)
	}
	retry.Store(true)
	request.TaskID = "reuse-ranges"
	if _, err := s.Start(request); err != nil {
		t.Fatal(err)
	}
	st := waitSync(t, s, request.TaskID)
	if st.State != "completed" || replayedFirst.Load() || st.TransferredBytes >= f.Size {
		t.Fatalf("range resume failed: %+v replayFirst=%v", st, replayedFirst.Load())
	}
}
