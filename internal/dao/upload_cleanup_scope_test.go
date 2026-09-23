package dao

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dingospeed/pkg/repository"
)

func TestUploadCleanupIgnoresBrokenRemoteRegistry(t *testing.T) {
	u, root := newTestUploadDao(t)
	k := repository.RepoKey{Namespace: "team", RepoType: "models", Repo: "nested/model"}
	if err := RegisterHosted(k.RepoType, k.Namespace, k.Repo); err != nil {
		t.Fatal(err)
	}
	blobs := filepath.Join(k.FilesRoot(root), "blobs")
	if err := os.MkdirAll(blobs, 0755); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(blobs, "pending"+localUploadStageSuffix)
	final := filepath.Join(blobs, "kept")
	for _, p := range []string{stage, final} {
		if err := os.WriteFile(p, []byte("fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stage, old, old); err != nil {
		t.Fatal(err)
	}
	// Any accidental full registry scan (including per-file Locate) fails here.
	remote := filepath.Join(root, "api", "models", "remote", "model", repository.Marker)
	if err := os.MkdirAll(filepath.Dir(remote), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remote, []byte("invalid descriptor"), 0644); err != nil {
		t.Fatal(err)
	}
	if n, err := u.CleanupExpiredStagedUploads(24 * time.Hour); err != nil || n != 1 {
		t.Fatalf("cleanup: removed=%d err=%v", n, err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("expired stage remains: %v", err)
	}
	found, err := collectLocalBlobs(filepath.Join(root, "files"))
	if err != nil {
		t.Fatal(err)
	}
	got := found[localRepoKey{k.RepoType, k.ID()}]
	if len(got) != 1 || got[0] != "kept" {
		t.Fatalf("unexpected blobs: %v", found)
	}
	if b, err := os.ReadFile(remote); err != nil || string(b) != "invalid descriptor" {
		t.Fatalf("remote fixture changed: %q %v", b, err)
	}
}

func TestStagedBlobIdentityStaysWithinKnownRepository(t *testing.T) {
	root := t.TempDir()
	d := repository.Hosted(repository.RepoKey{Namespace: "team", RepoType: "models", Repo: "nested/model"})
	base := d.FilesRoot(root)
	for _, tc := range []struct {
		path  string
		valid bool
	}{
		{filepath.Join(base, "blobs", "hash"+localUploadStageSuffix), true},
		{filepath.Join(base, "blobs", "nested", "hash"+localUploadStageSuffix), false},
		{filepath.Join(base, "blobs", localUploadStageSuffix), false},
		{filepath.Join(base, "resolve", "hash"+localUploadStageSuffix), false},
		{filepath.Join(base+"-other", "blobs", "hash"+localUploadStageSuffix), false},
	} {
		typ, id, sha, ok := stagedBlobIdentity(root, tc.path, d)
		if ok != tc.valid || (ok && (typ != d.RepoType || id != d.ID() || sha != "hash")) {
			t.Fatalf("%s: got %s %s %s %v", tc.path, typ, id, sha, ok)
		}
	}
}
