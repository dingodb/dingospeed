package dao

import (
	"bytes"
	"context"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSyncManifestPinsProtectOnlyTheirRepository(t *testing.T) {
	u, _ := newTestUploadDao(t)
	content := []byte("pending synchronization")
	p := deferredParam("weights", content)
	p.Namespace = "alice"
	p.Repo = "demo"
	a := mustStage(t, u, p, content)
	p.Namespace = "bob"
	b := mustStage(t, u, p, content)
	ageBlob(t, "models", "alice/demo", a.Sha256, 48*time.Hour)
	ageBlob(t, "models", "bob/demo", b.Sha256, 48*time.Hour)
	k := repository.RepoKey{RepoType: "models", Namespace: "alice", Repo: "demo"}
	release := PinSyncManifest(k, []LocalManifestFile{{Path: "weights", Sha256: a.Sha256, Size: int64(len(content))}})
	release2 := PinSyncManifest(k, []LocalManifestFile{{Path: "weights", Sha256: a.Sha256, Size: int64(len(content))}})
	release()
	release()
	if count, err := u.CleanupUnreferencedBlobs(time.Hour); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if !blobExists("models", "alice/demo", a.Sha256) {
		t.Fatal("active task lost content")
	}
	release2()
	if count, err := u.CleanupUnreferencedBlobs(time.Hour); err != nil || count != 1 {
		t.Fatalf("after release count=%d err=%v", count, err)
	}
}

func TestSyncPartialCleanupSkipsActiveAndRecentContent(t *testing.T) {
	u, _ := newTestUploadDao(t)
	dir := filepath.Join(config.SysConfig.Repos(), ".repository-sync", "content")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	names := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	old := time.Now().Add(-48 * time.Hour)
	for i, name := range names {
		path := filepath.Join(dir, name+".partial")
		if err := os.WriteFile(path, []byte("partial"), 0644); err != nil {
			t.Fatal(err)
		}
		if i != 2 {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	value, _ := syncBlobTransfers.LoadOrStore(names[0], make(chan struct{}, 1))
	gate := value.(chan struct{})
	gate <- struct{}{}
	n, err := u.CleanupSyncPartials(context.Background(), time.Hour, 1)
	<-gate
	if err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	for _, i := range []int{0, 2} {
		if _, err := os.Stat(filepath.Join(dir, names[i]+".partial")); err != nil {
			t.Fatal("protected content lost", err)
		}
	}
	n, err = u.CleanupSyncPartials(context.Background(), time.Hour, 256)
	if err != nil || n != 1 {
		t.Fatalf("released count=%d err=%v", n, err)
	}
}

func TestReclaimRechecksEligibilityUnderBlobLock(t *testing.T) {
	u, _ := newTestUploadDao(t)
	content := []byte("recent replacement")
	result := mustStage(t, u, deferredParam("file", content), content)
	sha := result.Sha256
	path := localBlobPath("models", "dingo-local/demo", sha)
	ageBlob(t, "models", "dingo-local/demo", sha, 48*time.Hour)
	observed, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	removed, err := reclaimBlobFileIf("models", "dingo-local/demo", sha, func(latest os.FileInfo) bool { return latest.ModTime().Equal(observed.ModTime()) })
	if err != nil || removed {
		t.Fatalf("recent upload removed=%v err=%v", removed, err)
	}
	if !blobExists("models", "dingo-local/demo", sha) {
		t.Fatal("replacement was deleted")
	}
}

func TestHostedCleanupBatchAdvancesAcrossRepositories(t *testing.T) {
	u, _ := newTestUploadDao(t)
	for i := 0; i < 9; i++ {
		content := []byte(fmt.Sprintf("orphan %d", i))
		p := deferredParam("file", content)
		p.Namespace = "batch"
		p.Repo = fmt.Sprintf("repo-%02d", i)
		out := mustStage(t, u, p, content)
		ageBlob(t, "models", p.Namespace+"/"+p.Repo, out.Sha256, 48*time.Hour)
	}
	after, err := u.cleanupHostedBatch(context.Background(), time.Hour, "")
	if err != nil || after == "" {
		t.Fatalf("first batch cursor=%q err=%v", after, err)
	}
	repos, err := collectLocalBlobs(filepath.Join(config.SysConfig.Repos(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	remaining := 0
	for _, blobs := range repos {
		remaining += len(blobs)
	}
	if remaining != 1 {
		t.Fatalf("remaining %d, want 1 after eight repositories", remaining)
	}
	after, err = u.cleanupHostedBatch(context.Background(), time.Hour, after)
	if err != nil || after != "" {
		t.Fatalf("last cursor=%q err=%v", after, err)
	}
	repos, err = collectLocalBlobs(filepath.Join(config.SysConfig.Repos(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	for _, blobs := range repos {
		if len(blobs) != 0 {
			t.Fatal("last repository was not reclaimed")
		}
	}
}

func TestDeferredReuseRenewsUnreferencedContentRetention(t *testing.T) {
	u, _ := newTestUploadDao(t)
	content := []byte("reuse after a long pause")
	p := deferredParam("file", content)
	out := mustStage(t, u, p, content)
	ageBlob(t, "models", "dingo-local/demo", out.Sha256, 48*time.Hour)
	again := mustStage(t, u, p, content)
	if !again.BlobReused {
		t.Fatal("expected reuse without writing the payload")
	}
	if n, err := u.CleanupUnreferencedBlobs(time.Hour); err != nil || n != 0 {
		t.Fatalf("freshly reused content reclaimed=%d err=%v", n, err)
	}
}

func TestZeroBodyStageReservationDoesNotCreateMissingSequentialStage(t *testing.T) {
	u, _ := newTestUploadDao(t)
	content := []byte("reserve complete content without source IO")
	p := deferredParam("file", content)
	out := mustStage(t, u, p, content)
	ageBlob(t, "models", "dingo-local/demo", out.Sha256, 48*time.Hour)
	end := int64(len(content))
	p.Start = &end
	renewed, err := u.UploadWholeFile(p, bytes.NewReader(nil))
	if err != nil || !renewed.BlobReused {
		t.Fatalf("renewed=%#v err=%v", renewed, err)
	}
	path := localBlobPath("models", "dingo-local/demo", out.Sha256)
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("not renewed: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := u.UploadWholeFile(p, bytes.NewReader(nil)); err == nil {
		t.Fatal("missing cache reservation unexpectedly succeeded")
	}
	if _, err := os.Stat(path + localUploadStageSuffix); !os.IsNotExist(err) {
		t.Fatalf("reservation created a sequential stage: %v", err)
	}
}
