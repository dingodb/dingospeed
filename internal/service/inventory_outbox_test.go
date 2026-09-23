package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"dingospeed/internal/dao"
	"dingospeed/internal/data"
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
)

func TestOutboxFixedRetryRecoveryAndNoIdleOrUnrelatedScan(t *testing.T) {
	old := config.SysConfig
	t.Cleanup(func() { config.SysConfig = old })
	root := t.TempDir()
	epoch := "11111111-1111-1111-1111-111111111111"
	var reports [][]byte
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/upload-inventory/nodes/node-a/session" {
			_ = json.NewEncoder(w).Encode(inventory.Session{InstanceID: "node-a", PendingEpoch: epoch})
			return
		}
		if r.URL.Path != "/api/v1/upload-inventory/reports" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		reports = append(reports, b)
		var p inventory.Report
		_ = json.Unmarshal(b, &p)
		if fail {
			fail = false
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(inventory.Ack{Epoch: p.Epoch, Sequence: p.Sequence, Digest: p.Digest(), Status: "accepted"})
	}))
	defer server.Close()
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: root}, Download: config.Download{BlockSize: 16}, Upload: config.Upload{Namespace: "dingo-local"}, Scheduler: config.Scheduler{HTTPURL: server.URL, Discovery: config.Discovery{InstanceId: "node-a"}}}
	base := data.NewBaseData()
	locks := dao.NewLockDao(base)
	files := dao.NewFileDao(nil, base, locks)
	uploads := dao.NewUploadDao(files, locks)
	svc := &SchedulerService{metaService: NewMetaService(files, nil)}
	ctx := context.Background()
	if err := svc.openInventorySession(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncPublications(ctx); err != nil {
		t.Fatal(err)
	}
	key := inventory.Key{Namespace: "team", RepoType: "models", Repo: "nested/repo"}
	upload := func(path string) {
		t.Helper()
		body := []byte(path)
		hash := sha256.Sum256(body)
		_, err := uploads.UploadWholeFile(dao.LocalUploadParam{Namespace: key.Namespace, RepoType: key.RepoType, Repo: key.Repo, Revision: "main", FilePath: path, Size: int64(len(body)), Sha256: hex.EncodeToString(hash[:])}, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
	}
	upload("a")
	fail = true
	if err := svc.sendRepository(ctx, key); err == nil {
		t.Fatal("expected lost acknowledgement")
	}
	fixed := append([]byte{}, reports[len(reports)-1]...)
	upload("b")
	if err := svc.sendRepository(ctx, key); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fixed, reports[len(reports)-1]) {
		t.Fatal("retry report changed")
	}
	q, _ := inventory.Read(root)
	if len(q.Entries) != 1 {
		t.Fatal("ACK cleared concurrent change")
	}
	// A malformed unrelated repository would fail any accidental full scan.
	bad := repository.RepoKey{Namespace: "team", RepoType: "models", Repo: "broken"}
	if err := os.MkdirAll(bad.APIRoot(root), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad.APIRoot(root), repository.Marker), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := svc.sendRepository(ctx, key); err != nil {
		t.Fatal("scanned unrelated repository:", err)
	}
	before := len(reports)
	if err := svc.SyncPublications(ctx); err != nil {
		t.Fatal(err)
	}
	if len(reports) != before {
		t.Fatal("idle queue sent inventory")
	}
	q, _ = inventory.Read(root)
	if len(q.Entries) != 0 {
		t.Fatal("queue not drained")
	}
	// Simulate process death after a journaled publish started but before its
	// revision metadata finished. Recovery uses the fixed target, not a full scan.
	commit, manifest, err := dao.ReadInventorySnapshot(repoKey(key), "main")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"Revision": "main", "Commit": commit, "Files": manifest})
	if err = inventory.Update(root, func(q *inventory.State) error {
		q.Sequence++
		q.Entries[key.ID()] = &inventory.Entry{Key: key, Sequence: q.Sequence, Operation: &inventory.Operation{Kind: "metadata", Data: payload}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(repoKey(key).Revision(root, "main"), "meta_get.json")); err != nil {
		t.Fatal(err)
	}
	if err = svc.sendRepository(ctx, key); err != nil {
		t.Fatal("failed journal recovery:", err)
	}
	if _, _, err = dao.ReadInventorySnapshot(repoKey(key), "main"); err != nil {
		t.Fatal("revision not recovered:", err)
	}
}
