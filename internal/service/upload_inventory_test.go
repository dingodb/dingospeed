package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"dingospeed/internal/dao"
	"dingospeed/internal/data"
	"dingospeed/pkg/config"
)

func TestBuildUploadInventoryUnionsLiveRevisionsAndRejectsDamage(t *testing.T) {
	old := config.SysConfig
	config.SysConfig = &config.Config{Server: config.ServerConfig{Repos: t.TempDir()}, Download: config.Download{BlockSize: 16}, Upload: config.Upload{Namespace: "dingo-local"}, Scheduler: config.Scheduler{Discovery: config.Discovery{InstanceId: "node-a"}}}
	t.Cleanup(func() { config.SysConfig = old })
	base := data.NewBaseData()
	locks := dao.NewLockDao(base)
	files := dao.NewFileDao(nil, base, locks)
	uploads := dao.NewUploadDao(files, locks)
	meta := NewMetaService(files, nil)
	svc := &SchedulerService{metaService: meta}

	upload := func(revision, path string, body []byte) string {
		t.Helper()
		h := sha256.Sum256(body)
		result, err := uploads.UploadWholeFile(dao.LocalUploadParam{RepoType: "models", Namespace: "team", Repo: "demo", Revision: revision, FilePath: path, Size: int64(len(body)), Sha256: hex.EncodeToString(h[:])}, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return result.Commit
	}
	shared := []byte("shared")
	mainCommit := upload("main", "a.bin", shared)
	v2Commit := upload("v2", "a.bin", shared)
	upload("v2", "b.bin", shared)                 // same content, different path
	upload("other", "a.bin", []byte("different")) // same path, different content

	snap, err := svc.buildUploadInventory()
	if err != nil || !snap.Complete {
		t.Fatalf("snapshot complete=%v err=%v detail=%s", snap.Complete, err, snap.Error)
	}
	if len(snap.Items) != 3 {
		t.Fatalf("items=%d, want 3: %+v", len(snap.Items), snap.Items)
	}
	if err = persistUploadInventory(snap); err != nil {
		t.Fatal(err)
	}
	next, err := svc.buildUploadInventory()
	if err != nil || next.Epoch != snap.Epoch || next.Sequence != snap.Sequence+1 {
		t.Fatalf("next=%+v err=%v", next, err)
	}

	// Removing one local revision reference must not remove the node holding while
	// another live revision still references the same path/content.
	if _, err = uploads.PublishTree(dao.LocalPublishTreeParam{RepoType: "models", Namespace: "team", Repo: "demo", Revision: "main", BaseCommit: mainCommit, Files: []dao.LocalManifestFile{}}); err != nil {
		t.Fatal(err)
	}
	afterOne, err := svc.buildUploadInventory()
	if err != nil || !afterOne.Complete {
		t.Fatalf("after one complete=%v err=%v %s", afterOne.Complete, err, afterOne.Error)
	}
	foundSharedA := false
	for _, got := range afterOne.Items {
		if got.Path == "a.bin" && got.Size == int64(len(shared)) {
			foundSharedA = true
		}
	}
	if !foundSharedA {
		t.Fatal("shared a.bin disappeared while v2 still referenced it")
	}

	// A physically missing/corrupt published blob makes the scan incomplete; it
	// must not be emitted as authoritative absence.
	_ = v2Commit
	sharedHash := sha256.Sum256(shared)
	blob := dao.RepositoryKey("models", "team/demo").Blob(config.SysConfig.Repos(), hex.EncodeToString(sharedHash[:]))
	if err = os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	damaged, err := svc.buildUploadInventory()
	if err != nil || damaged.Complete {
		t.Fatalf("damaged complete=%v err=%v", damaged.Complete, err)
	}
}
