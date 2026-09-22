package dao

import (
	"os"
	"testing"
	"time"
)

func TestHostedCleanupIsolatesSameContentAcrossNamespacesAndRejectsBadManifest(t *testing.T) {
	u, _ := newTestUploadDao(t)
	content := []byte("shared content in independent namespaces")
	p := uploadParam("nested/file.bin", content)
	p.Namespace, p.Repo = "alice", "team/resolve/model-a"
	alice := mustUpload(t, u, p, content)
	p.Namespace, p.Deferred = "bob", true
	bob := mustUpload(t, u, p, content)
	old := time.Now().Add(-48 * time.Hour)
	for _, ns := range []string{"alice", "bob"} {
		if err := os.Chtimes(localBlobPath("models", ns+"/"+p.Repo, bob.Sha256), old, old); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := u.CleanupUnreferencedBlobs(time.Hour)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(localBlobPath("models", "alice/"+p.Repo, alice.Sha256)); err != nil {
		t.Fatal("alice's referenced content lost:", err)
	}
	if _, err := os.Stat(localBlobPath("models", "bob/"+p.Repo, bob.Sha256)); !os.IsNotExist(err) {
		t.Fatal("bob's unreferenced content was not reclaimed")
	}

	orphan := []byte("unreferenced but unsafe to reclaim without valid reference data")
	p = uploadParam("new.bin", orphan)
	p.Namespace, p.Repo, p.Deferred = "alice", "team/resolve/model-a", true
	staged := mustUpload(t, u, p, orphan)
	blob := localBlobPath("models", "alice/"+p.Repo, staged.Sha256)
	if err := os.Chtimes(blob, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LocalManifestPath("models", "alice/"+p.Repo, alice.Commit), []byte("broken manifest"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := u.CleanupUnreferencedBlobs(time.Hour); err == nil {
		t.Fatal("bad manifest treated as no references")
	}
	if _, err := os.Stat(blob); err != nil {
		t.Fatal("content reclaimed despite unreadable reference data:", err)
	}
}
