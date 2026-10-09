package dao

import (
	"bytes"
	"context"
	"dingospeed/pkg/repository"
	"errors"
	"testing"
)

func TestSyncContentRequiresActivePublishedReference(t *testing.T) {
	u, _ := newTestUploadDao(t)
	content := []byte("payload")
	p := uploadParam("a", content)
	p.Deferred = true
	mustUpload(t, u, p, content)
	k := repository.RepoKey{Namespace: p.Namespace, RepoType: p.RepoType, Repo: p.Repo}
	f := LocalManifestFile{Path: p.FilePath, Sha256: p.Sha256, Size: p.Size}
	var out bytes.Buffer
	if err := u.CopySyncContent(k, f, &out); err == nil {
		t.Fatal("served staged content")
	}
	if _, err := u.PublishFiles(LocalPublishParam{Namespace: p.Namespace, RepoType: p.RepoType, Repo: p.Repo, Revision: p.Revision, Files: []LocalManifestFile{f}}); err != nil {
		t.Fatal(err)
	}
	if err := u.CopySyncContent(k, f, &out); err != nil || !bytes.Equal(out.Bytes(), content) {
		t.Fatalf("published content: %q %v", out.Bytes(), err)
	}
	f.Path = "not-the-published-path"
	out.Reset()
	if err := u.CopySyncContent(k, f, &out); err == nil {
		t.Fatal("served hash without matching published path")
	}
	f.Path = p.FilePath
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := u.CopySyncContentRange(ctx, k, f, 0, f.Size, &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("verification ignored cancellation: %v", err)
	}
}
