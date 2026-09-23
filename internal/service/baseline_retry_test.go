package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/inventory"
	"dingospeed/pkg/repository"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type baselineFixture struct {
	svc         *SchedulerService
	root, epoch string
	fail        bool
	reports     [][]byte
	statuses    []string
	upload      func()
}

func newBaselineFixture(t *testing.T) *baselineFixture {
	t.Helper()
	f := &baselineFixture{root: t.TempDir(), epoch: "11111111-1111-1111-1111-111111111111"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/upload-inventory/nodes/node-a/session":
			json.NewEncoder(w).Encode(inventory.Session{InstanceID: "node-a", PendingEpoch: f.epoch})
		case "/api/v1/upload-inventory/nodes/node-a/reconcile-progress":
			var p map[string]string
			json.NewDecoder(r.Body).Decode(&p)
			f.statuses = append(f.statuses, p["status"])
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		case "/api/v1/upload-inventory/reports":
			b, _ := io.ReadAll(r.Body)
			f.reports = append(f.reports, b)
			if f.fail {
				w.WriteHeader(503)
				return
			}
			var p inventory.Report
			json.Unmarshal(b, &p)
			json.NewEncoder(w).Encode(inventory.Ack{Epoch: p.Epoch, Sequence: p.Sequence, Digest: p.Digest(), Status: "accepted"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: f.root}, Download: config.Download{BlockSize: 16}, Upload: config.Upload{Namespace: "dingo-local"}, Scheduler: config.Scheduler{HTTPURL: server.URL, Discovery: config.Discovery{InstanceId: "node-a"}}}
	base := data.NewBaseData()
	locks := dao.NewLockDao(base)
	files := dao.NewFileDao(nil, base, locks)
	uploads := dao.NewUploadDao(files, locks)
	f.svc = &SchedulerService{metaService: NewMetaService(files, nil)}
	f.upload = func() {
		t.Helper()
		body := []byte("new commit")
		hash := sha256.Sum256(body)
		_, err := uploads.UploadWholeFile(dao.LocalUploadParam{Namespace: "team", RepoType: "models", Repo: "changed", Revision: "main", FilePath: "a", Size: int64(len(body)), Sha256: hex.EncodeToString(hash[:])}, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.openInventorySession(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}
func retryNow(t *testing.T, root string) {
	t.Helper()
	if err := inventory.Update(root, func(s *inventory.State) error { s.RetryAt = time.Time{}; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestBaselineTrustsPublishedMetadataWithoutHashingPayload(t *testing.T) {
	f := newBaselineFixture(t)
	f.upload()
	hash := sha256.Sum256([]byte("new commit"))
	digest := hex.EncodeToString(hash[:])
	blob := dao.RepositoryKey("models", "team/changed").Blob(f.root, digest)
	cache, err := downloader.NewDingCache(blob, 16)
	if err != nil {
		t.Fatal(err)
	}
	// Change the payload while preserving size and complete block metadata.
	// Inventory reconciliation trusts the published SHA256, not these bytes.
	err = cache.WriteBlock(0, bytes.Repeat([]byte("x"), 16))
	cache.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.SyncPublications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.reports) != 1 {
		t.Fatalf("reports=%d, want one baseline", len(f.reports))
	}
	var p inventory.Report
	if err = json.Unmarshal(f.reports[0], &p); err != nil {
		t.Fatal(err)
	}
	if !p.Baseline || len(p.Files) != 1 || p.Files[0].SHA256 != digest || p.Files[0].Size != 10 {
		t.Fatalf("published metadata was not preserved: %+v", p)
	}
	q, err := inventory.Read(f.root)
	if err != nil || q.ReconcileEpoch != "" || len(q.Entries) != 0 {
		t.Fatalf("baseline not confirmed: %+v %v", q, err)
	}
}

func TestBaselineRejectsMissingPublishedFile(t *testing.T) {
	f := newBaselineFixture(t)
	f.upload()
	hash := sha256.Sum256([]byte("new commit"))
	blob := dao.RepositoryKey("models", "team/changed").Blob(f.root, hex.EncodeToString(hash[:]))
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SyncPublications(context.Background()); err == nil {
		t.Fatal("missing published file accepted")
	}
	if len(f.reports) != 0 {
		t.Fatal("incomplete baseline sent to Scheduler")
	}
}
func TestBaselineBuildFailureStopsScanningAndExplicitResetResumes(t *testing.T) {
	f := newBaselineFixture(t)
	ctx := context.Background()
	bad := filepath.Join(f.root, "api", "models", repository.Local, "broken", repository.Marker)
	if err := os.MkdirAll(filepath.Dir(bad), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("invalid"), 0644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxBaselineBuildAttempts; i++ {
		retryNow(t, f.root)
		if err := f.svc.SyncPublications(ctx); err == nil {
			t.Fatal("expected build failure")
		}
	}
	q, err := inventory.Read(f.root)
	if err != nil || !q.BuildBlocked || q.BuildAttempts != 3 {
		t.Fatalf("budget not persisted: %+v %v", q, err)
	}
	if len(f.reports) != 0 || f.statuses[len(f.statuses)-1] != "needs_attention" {
		t.Fatal("partial report sent or attention not reported")
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	f.upload()
	// Reconnection and duplicate delivery must not reset the budget.
	if err := f.svc.openInventorySession(ctx); err != nil {
		t.Fatal(err)
	}
	if err := inventory.RequestReconcile(f.root, f.epoch); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SyncPublications(ctx); err != nil {
		t.Fatal(err)
	}
	q, _ = inventory.Read(f.root)
	if !q.BuildBlocked || len(q.Entries) != 1 || len(f.reports) != 0 {
		t.Fatal("blocked baseline consumed new commit or scanned again")
	}
	if nextInventoryWake() < time.Hour {
		t.Fatal("blocked baseline busy polls")
	}
	f.epoch = "22222222-2222-2222-2222-222222222222"
	if err := inventory.RequestReconcile(f.root, f.epoch); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SyncPublications(ctx); err != nil {
		t.Fatal(err)
	}
	q, _ = inventory.Read(f.root)
	if q.BuildBlocked || q.BuildAttempts != 0 || len(q.Entries) != 0 || len(f.reports) != 1 {
		t.Fatalf("reset failed: %+v", q)
	}
	var p inventory.Report
	json.Unmarshal(f.reports[0], &p)
	if len(p.Files) != 1 {
		t.Fatal("new commit missing from rebuilt baseline")
	}
}
func TestBaselineNetworkRetryKeepsBytesAndLaterCommit(t *testing.T) {
	f := newBaselineFixture(t)
	ctx := context.Background()
	f.fail = true
	if err := f.svc.SyncPublications(ctx); err == nil {
		t.Fatal("expected HTTP failure")
	}
	fixed := append([]byte(nil), f.reports[0]...)
	f.upload()
	for i := 0; i < 4; i++ {
		retryNow(t, f.root)
		if err := f.svc.SyncPublications(ctx); err == nil {
			t.Fatal("expected HTTP failure")
		}
	}
	q, _ := inventory.Read(f.root)
	if q.BuildBlocked || q.BuildAttempts != 0 {
		t.Fatal("network error consumed build budget")
	}
	f.fail = false
	retryNow(t, f.root)
	if err := f.svc.SyncPublications(ctx); err != nil {
		t.Fatal(err)
	}
	for _, b := range f.reports {
		if !bytes.Equal(b, fixed) {
			t.Fatal("baseline retry changed")
		}
	}
	q, _ = inventory.Read(f.root)
	if len(q.Entries) != 1 {
		t.Fatal("baseline ACK removed later commit")
	}
	if err := f.svc.SyncPublications(ctx); err != nil {
		t.Fatal(err)
	}
	q, _ = inventory.Read(f.root)
	if len(q.Entries) != 0 {
		t.Fatal("later commit not delivered")
	}
}
