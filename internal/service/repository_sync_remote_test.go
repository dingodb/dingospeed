package service

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/proto/manager"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRepositorySyncPrepareDoesNotPublish(t *testing.T) {
	s, u := newSyncTest(t)
	seedSync(t, u, "seed", "a", "a")
	r := syncReq("prepare-upload", syncFile("a", "a"))
	st, err := s.Prepare(r)
	if err != nil || st.State != "prepared" {
		t.Fatalf("prepare=%+v %v", st, err)
	}
	if _, _, err := dao.ReadInventorySnapshot(r.key(), r.Revision); !os.IsNotExist(err) {
		t.Fatalf("prepared published: %v", err)
	}
	if _, err = s.Execute(r.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(r.TaskID); err != nil {
		t.Fatal(err)
	}
	if st = waitSync(t, s, r.TaskID); st.State != "completed" {
		t.Fatalf("%+v", st)
	}
	r.TaskID = "prepare-no-source"
	r.Revision = "missing"
	r.Files = []dao.LocalManifestFile{syncFile("missing", "missing")}
	s.resolve = func(context.Context, repository.RepoKey, dao.LocalManifestFile) ([]SyncSource, error) {
		return nil, nil
	}
	if _, err = s.Prepare(r); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestRepositorySyncNativeProvidersFixedVersion(t *testing.T) {
	for _, ns := range []string{repository.HuggingFace, repository.ModelScope} {
		t.Run(ns, func(t *testing.T) {
			s, u := newSyncTest(t)
			b := data.NewBaseData()
			lock := dao.NewLockDao(b)
			u.uploadDao = dao.NewUploadDao(dao.NewFileDao(dao.NewDownloaderDao(nil), b, lock), lock)
			config.SysConfig.Server.Online = true
			config.SysConfig.Retry.Attempts = 1
			config.SysConfig.Download.RemoteFileBufferSize = 4096
			payload := bytes.Repeat([]byte("native"), 1000)
			oid := fmt.Sprintf("%x", sha256.Sum256(payload))
			kind := "sha256"
			if ns == repository.HuggingFace {
				oid = fmt.Sprintf("%x", sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(payload))), payload...)))
				kind = "git-blob-sha1"
			}
			commit := strings.Repeat("a", 40)
			var requests atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if ns == repository.HuggingFace && !strings.Contains(r.URL.Path, "/resolve/"+commit+"/") {
					t.Errorf("unfixed HF path: %s", r.URL)
				}
				if ns == repository.ModelScope && r.URL.Query().Get("Revision") != commit {
					t.Errorf("unfixed MS: %s", r.URL)
				}
				http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(payload))
			}))
			defer origin.Close()
			parsed, _ := url.Parse(origin.URL)
			config.SysConfig.Server.HfScheme = "http"
			config.SysConfig.Server.HfNetLoc = parsed.Host
			config.SysConfig.Modelscope.OfficialBaseURL = origin.URL
			r := RemoteSyncRequest{TaskID: "native-" + ns, Namespace: ns, RepoType: "models", Repo: "org/demo", Revision: "release", ExpectedAbsent: true, SourceCommit: commit, Files: []dao.RemoteSyncFile{{Path: "a.bin", OID: oid, Size: int64(len(payload)), IdentityKind: kind}}}.Request()
			st, err := s.Prepare(r)
			if err != nil || st.State != "prepared" {
				t.Fatalf("prepare %+v %v", st, err)
			}
			if requests.Load() != 0 {
				t.Fatal("prepare fetched payload")
			}
			if _, err = s.Execute(r.TaskID); err != nil {
				t.Fatal(err)
			}
			st = waitSync(t, s, r.TaskID)
			if st.State != "completed" {
				t.Fatalf("%+v", st)
			}
			got, err := dao.ReadRemoteSyncManifest(r.key(), r.Revision)
			if err != nil || RemoteManifestDigest(got) != RemoteManifestDigest(*r.Remote) {
				t.Fatalf("readback %+v %v", got, err)
			}
			var gotBody bytes.Buffer
			if err = dao.CopyRemoteSyncContent(context.Background(), r.key(), commit, r.Remote.Files[0], 17, 3000, &gotBody); err != nil || !bytes.Equal(gotBody.Bytes(), payload[17:3000]) {
				t.Fatalf("native cache range mismatch %v", err)
			}
			before := requests.Load()
			r.TaskID += "-cached"
			r.Revision = "other"
			if _, err = s.Start(r); err != nil {
				t.Fatal(err)
			}
			if st = waitSync(t, s, r.TaskID); st.State != "completed" {
				t.Fatalf("cached %+v", st)
			}
			if requests.Load() != before {
				t.Fatal("complete cache downloaded again")
			}
			// Same upstream commit with another file list cannot mutate an immutable version.
			r.TaskID += "-bad"
			r.Revision = "bad"
			r.Remote = &dao.RemoteSyncManifest{Commit: commit, Files: []dao.RemoteSyncFile{}}
			if _, err = s.Start(r); err != nil {
				t.Fatal(err)
			}
			if st = waitSync(t, s, r.TaskID); st.State != "conflict" {
				t.Fatalf("immutable changed %+v", st)
			}
			got, err = dao.ReadRemoteSyncManifest(r.key(), "release")
			if err != nil || len(got.Files) != 1 {
				t.Fatalf("old revision changed %+v %v", got, err)
			}
		})
	}
}

type nativeSchedulerClient struct {
	manager.ManagerClient
	host     string
	port     int32
	size     int64
	calls    atomic.Int32
	reported atomic.Int32
}

func (c *nativeSchedulerClient) SchedulerFile(ctx context.Context, r *manager.SchedulerFileRequest, _ ...grpc.CallOption) (*manager.SchedulerFileResponse, error) {
	c.calls.Add(1)
	return &manager.SchedulerFileResponse{SchedulerType: consts.SchedulerYes, Host: c.host, Port: c.port, MaxOffset: c.size}, nil
}
func TestRepositorySyncNativePeerWorksOffline(t *testing.T) {
	for _, ns := range []string{repository.HuggingFace, repository.ModelScope} {
		t.Run(ns, func(t *testing.T) {
			s, u := newSyncTest(t)
			config.SysConfig.Download.RemoteFileBufferSize = 4096
			payload := bytes.Repeat([]byte("peer"), 1500)
			commit := strings.Repeat("b", 40)
			oid := fmt.Sprintf("%x", sha256.Sum256(payload))
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if !strings.HasSuffix(r.URL.Path, "/remote-content") || r.URL.Query().Get("commit") != commit || r.URL.Query().Get("oid") != oid {
					t.Errorf("bad peer URL %s", r.URL)
				}
				http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(payload))
			}))
			defer peer.Close()
			parsed, _ := url.Parse(peer.URL)
			host, port, _ := net.SplitHostPort(parsed.Host)
			portNo, _ := strconv.Atoi(port)
			client := &nativeSchedulerClient{host: host, port: int32(portNo), size: int64(len(payload))}
			b := data.NewBaseData()
			lock := dao.NewLockDao(b)
			u.uploadDao = dao.NewUploadDao(dao.NewFileDao(dao.NewDownloaderDao(&dao.SchedulerDao{Client: client}), b, lock), lock)
			r := RemoteSyncRequest{TaskID: "peer-" + ns, Namespace: ns, RepoType: "models", Repo: "org/demo", Revision: "main", ExpectedAbsent: true, SourceCommit: commit, Files: []dao.RemoteSyncFile{{Path: "f", OID: oid, Size: int64(len(payload)), IdentityKind: "sha256"}}}.Request()
			if _, err := s.Start(r); err != nil {
				t.Fatal(err)
			}
			st := waitSync(t, s, r.TaskID)
			if st.State != "completed" {
				t.Fatalf("%+v", st)
			}
			if client.calls.Load() == 0 || calls.Load() == 0 {
				t.Fatal("native file bypassed scheduler/peer")
			}
		})
	}
}
func TestRepositorySyncRemotePreviewResolvesWithoutWriting(t *testing.T) {
	for _, ns := range []string{repository.HuggingFace, repository.ModelScope} {
		t.Run(ns, func(t *testing.T) {
			s, _ := newSyncTest(t)
			config.SysConfig.Server.Online = true
			config.SysConfig.Retry.Attempts = 1
			commit := strings.Repeat("c", 40)
			oid := strings.Repeat("d", 64)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ns == repository.ModelScope {
					fmt.Fprintf(w, `{"Code":200,"Data":{"LatestCommitter":{"Id":%q},"Files":[{"Path":"a","Type":"blob","Sha256":%q,"Size":4,"Revision":%q}]}}`, commit, oid, commit)
					return
				}
				if strings.Contains(r.URL.Path, "paths-info") {
					fmt.Fprintf(w, `[{"path":"a","type":"file","size":4,"oid":%q,"lfs":{"oid":%q,"size":4}}]`, strings.Repeat("e", 40), oid)
					return
				}
				fmt.Fprintf(w, `{"sha":%q,"siblings":[{"rfilename":"a"}]}`, commit)
			}))
			defer origin.Close()
			parsed, _ := url.Parse(origin.URL)
			config.SysConfig.Server.HfScheme = "http"
			config.SysConfig.Server.HfNetLoc = parsed.Host
			config.SysConfig.Modelscope.OfficialBaseURL = origin.URL
			k := repository.RepoKey{Namespace: ns, RepoType: "models", Repo: "org/demo"}
			m, err := s.RemoteManifest(context.Background(), k, "main", "upstream", "")
			if err != nil || m.Commit != commit || len(m.Files) != 1 || m.Files[0].OID != oid {
				t.Fatalf("%+v %v", m, err)
			}
			if _, err = dao.ReadRemoteSyncManifest(k, "main"); !os.IsNotExist(err) {
				t.Fatalf("preview mutated alias: %v", err)
			}
		})
	}
}

func TestRepositorySyncPreparedExpiryOnlyRemovesUnexecuted(t *testing.T) {
	s, _ := newSyncTest(t)
	r := syncReq("expired-preview")
	st, err := s.Prepare(r)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	e := s.entries[r.TaskID]
	e.status.ExpiresAt = time.Now().Add(-time.Minute)
	st = e.status
	s.mu.Unlock()
	if err = util.WriteDataToFileAtomic(s.taskPath(r.TaskID, "status"), st); err != nil {
		t.Fatal(err)
	}
	s.expirePrepared(context.Background())
	if _, err = os.Stat(s.taskPath(r.TaskID, "manifest")); !os.IsNotExist(err) {
		t.Fatalf("not expired %v", err)
	}
	r.TaskID = "executed-preview"
	if _, err = s.Prepare(r); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(r.TaskID); err != nil {
		t.Fatal(err)
	}
	st = waitSync(t, s, r.TaskID)
	s.mu.Lock()
	e = s.entries[r.TaskID]
	e.status.ExpiresAt = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	s.expirePrepared(context.Background())
	if _, err = os.Stat(s.taskPath(r.TaskID, "manifest")); err != nil {
		t.Fatal("executed receipt removed", err)
	}
}

func TestRepositorySyncNativeConcurrentBaselineAndReceipt(t *testing.T) {
	s, u := newSyncTest(t)
	k := repository.RepoKey{Namespace: repository.HuggingFace, RepoType: "models", Repo: "org/demo"}
	first := dao.RemoteSyncManifest{Commit: strings.Repeat("1", 40), Files: []dao.RemoteSyncFile{}}
	if err := u.uploadDao.PublishRemoteSync(k, "main", "", true, "", first, "first"); err != nil {
		t.Fatal(err)
	}
	r := RemoteSyncRequest{TaskID: "native-baseline", Namespace: k.Namespace, RepoType: k.RepoType, Repo: k.Repo, Revision: "main", BaseCommit: first.Commit, BaseManifestDigest: RemoteManifestDigest(first), SourceCommit: strings.Repeat("2", 40), Files: []dao.RemoteSyncFile{}}.Request()
	if _, err := s.Prepare(r); err != nil {
		t.Fatal(err)
	}
	other := dao.RemoteSyncManifest{Commit: strings.Repeat("3", 40), Files: []dao.RemoteSyncFile{}}
	if err := u.uploadDao.PublishRemoteSync(k, "main", first.Commit, false, RemoteManifestDigest(first), other, "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(r.TaskID); err == nil {
		t.Fatal("stale preview executed")
	}
	if st, err := s.Status(r.TaskID); err != nil || st.State != "conflict" {
		t.Fatalf("conflict not durable %+v %v", st, err)
	}
	got, err := dao.ReadRemoteSyncManifest(k, "main")
	if err != nil || got.Commit != other.Commit {
		t.Fatalf("target overwritten %+v %v", got, err)
	}
	if okay, err := u.uploadDao.RemotePublicationMatches(k, "main", "first", first); err != nil || !okay {
		t.Fatalf("later alias erased durable receipt %v %v", okay, err)
	}
}

func TestRepositorySyncNativeDamagedReceiptDoesNotFallback(t *testing.T) {
	_, u := newSyncTest(t)
	k := repository.RepoKey{Namespace: repository.HuggingFace, RepoType: "models", Repo: "org/demo"}
	m := dao.RemoteSyncManifest{Commit: strings.Repeat("f", 40), Files: []dao.RemoteSyncFile{}}
	if err := u.uploadDao.PublishRemoteSync(k, "main", "", true, "", m, "receipt"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(k.APIRoot(config.SysConfig.Repos()), "sync-native-receipts", "receipt.json")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if okay, err := u.uploadDao.RemotePublicationMatches(k, "main", "receipt", m); err == nil || okay {
		t.Fatalf("corrupt receipt guessed success %v %v", okay, err)
	}
}

func TestRepositorySyncNativeStalePeerFallsBack(t *testing.T) {
	s, u := newSyncTest(t)
	config.SysConfig.Server.Online = true
	config.SysConfig.Download.RemoteFileBufferSize = 4096
	payload := bytes.Repeat([]byte("fallback"), 700)
	oid := fmt.Sprintf("%x", sha256.Sum256(payload))
	commit := strings.Repeat("e", 40)
	var peers, origins atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { peers.Add(1); w.WriteHeader(404) }))
	defer peer.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origins.Add(1)
		if !strings.Contains(r.URL.Path, commit) {
			t.Error("lost pinned commit")
		}
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(payload))
	}))
	defer origin.Close()
	parsed, _ := url.Parse(peer.URL)
	host, port, _ := net.SplitHostPort(parsed.Host)
	portNo, _ := strconv.Atoi(port)
	client := &nativeSchedulerClient{host: host, port: int32(portNo), size: int64(len(payload))}
	parsed, _ = url.Parse(origin.URL)
	config.SysConfig.Server.HfScheme = "http"
	config.SysConfig.Server.HfNetLoc = parsed.Host
	b := data.NewBaseData()
	lock := dao.NewLockDao(b)
	u.uploadDao = dao.NewUploadDao(dao.NewFileDao(dao.NewDownloaderDao(&dao.SchedulerDao{Client: client}), b, lock), lock)
	r := RemoteSyncRequest{TaskID: "stale-peer", Namespace: repository.HuggingFace, RepoType: "models", Repo: "org/demo", Revision: "main", ExpectedAbsent: true, SourceCommit: commit, Files: []dao.RemoteSyncFile{{Path: "f", OID: oid, Size: int64(len(payload)), IdentityKind: "sha256"}}}.Request()
	if _, err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	if st := waitSync(t, s, r.TaskID); st.State != "completed" {
		t.Fatalf("%+v", st)
	}
	if peers.Load() == 0 || origins.Load() == 0 {
		t.Fatal("stale peer did not fallback")
	}
}

func (c *nativeSchedulerClient) SyncFileProcess(ctx context.Context, r *manager.SyncFileProcessReq, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	c.calls.Add(int32(len(r.FileProcessEntries)))
	return &emptypb.Empty{}, nil
}
func TestRepositorySyncSourceReportOnlyCompleteFixedFiles(t *testing.T) {
	s, u := newSyncTest(t)
	client := &nativeSchedulerClient{}
	b := data.NewBaseData()
	lock := dao.NewLockDao(b)
	u.uploadDao = dao.NewUploadDao(dao.NewFileDao(dao.NewDownloaderDao(&dao.SchedulerDao{Client: client}), b, lock), lock)
	k := repository.RepoKey{Namespace: repository.ModelScope, RepoType: "models", Repo: "org/report"}
	commit := strings.Repeat("a", 40)
	empty := dao.RemoteSyncFile{Path: "empty", OID: fmt.Sprintf("%x", sha256.Sum256(nil)), IdentityKind: "sha256", Size: 0}
	missing := dao.RemoteSyncFile{Path: "missing", OID: strings.Repeat("b", 64), IdentityKind: "sha256", Size: 12}
	if err := u.uploadDao.DownloadRemoteSyncFile(context.Background(), k, commit, empty, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	m := dao.RemoteSyncManifest{Commit: commit, Files: []dao.RemoteSyncFile{empty, missing}}
	raw, _ := json.Marshal(map[string]any{"sha": commit, "siblings": []map[string]any{{"rfilename": empty.Path, "blobId": empty.OID, "size": empty.Size}, {"rfilename": missing.Path, "blobId": missing.OID, "size": missing.Size}}})
	if err := util.WriteDataToFileAtomic(filepath.Join(k.Revision(config.SysConfig.Repos(), commit), "meta_get.json"), common.CacheContent{Version: consts.VersionSnapshot, StatusCode: 200, Content: hex.EncodeToString(raw)}); err != nil {
		t.Fatal(err)
	}
	result, err := s.ReportRemoteSource(k, RemoteSourceReportRequest{Repo: k.Repo, Revision: "main", Commit: commit, ManifestDigest: RemoteManifestDigest(m)})
	if err != nil || result.TotalFiles != 2 || result.ReportedFiles != 1 || client.calls.Load() != 1 {
		t.Fatalf("report %+v calls%d err%v", result, client.calls.Load(), err)
	}
	if _, err = s.ReportRemoteSource(k, RemoteSourceReportRequest{Repo: k.Repo, Revision: "main", Commit: commit, ManifestDigest: "wrong"}); err == nil {
		t.Fatal("mismatched manifest reported")
	}
}
