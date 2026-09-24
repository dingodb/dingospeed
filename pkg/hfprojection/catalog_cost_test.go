package hfprojection

import (
	"context"
	"dingospeed/pkg/repository"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCatalogRefreshDoesNotBuildFileManifests(t *testing.T) {
	r, _ := fixture(t)
	index := NewIndex()
	cat, err := index.RefreshContext(context.Background(), r.Root, "models")
	if err != nil || len(cat.Repos) != 1 {
		t.Fatalf("%+v %v", cat, err)
	}
	index.mu.RLock()
	count := len(index.scopes[scopeKey(r.Root, "models")].manifests)
	index.mu.RUnlock()
	if count != 0 {
		t.Fatalf("catalog eagerly built %d manifests", count)
	}

	m, err := index.Manifest(r.Root, "models", "Qwen/demo", "main")
	if err != nil || len(m.Files) != 3 {
		t.Fatalf("%+v %v", m, err)
	}
}
func TestModelScopeLegacyCatalogSkipsUnrelatedAndDeepTrees(t *testing.T) {
	root := t.TempDir()
	api := filepath.Join(root, "api", "models", "modelscope", "org", "old")
	response(t, filepath.Join(api, "revision", "main", "meta_get.json"), map[string]any{"sha": "commit1"})
	write(t, filepath.Join(root, "api", "models", "Qwen", "old", repository.Marker), []byte("broken"))
	write(t, filepath.Join(api, "paths-info", "deep", "file", repository.Marker), []byte("broken"))
	r := Reader{Root: root, Namespace: repository.ModelScope}
	cat, err := r.Catalog("models")
	if err != nil || len(cat.Repos) != 1 || cat.Repos[0].Repo != "org/old" {
		t.Fatalf("%+v %v", cat, err)
	}
	if _, err := os.Stat(filepath.Join(api, repository.Marker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote marker")
	}
}
func TestCancelledCatalogDoesNotReplaceIndex(t *testing.T) {
	r, _ := fixture(t)
	i := NewIndex()
	if _, err := i.Refresh(r.Root, "models"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := i.RefreshContext(ctx, r.Root, "models"); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if _, err := i.Manifest(r.Root, "models", "Qwen/demo", "main"); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogCancellationWhileWaitingForRefresh(t *testing.T) {
	i := NewIndex()
	i.refreshGate <- struct{}{}
	defer func() { <-i.refreshGate }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := i.RefreshContext(ctx, t.TempDir(), "models"); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestLargeLegacyCatalogDoesNotTraverseFileTrees(t *testing.T) {
	root := t.TempDir()
	for n := 0; n < 100; n++ {
		api := filepath.Join(root, "api", "models", "org", fmt.Sprintf("old-%03d", n))
		response(t, filepath.Join(api, "revision", "main", "meta_get.json"), map[string]any{"sha": "commit1"})
		for f := 0; f < 20; f++ {
			write(t, filepath.Join(api, "paths-info", "commit1", fmt.Sprintf("dir-%03d", f), "nested", "paths-info_post.json"), []byte("invalid file metadata must not be read by catalog"))
		}
	}
	write(t, filepath.Join(root, "api", "models", "ordinary", "folder", "note.txt"), []byte("not a repository"))
	start := time.Now()
	i := NewIndex()
	cat, err := i.Refresh(root, "models")
	if err != nil || len(cat.Repos) != 100 {
		t.Fatalf("repos=%d err=%v", len(cat.Repos), err)
	}
	for _, repo := range cat.Repos {
		if repo.Error != "" {
			t.Fatalf("%+v", repo)
		}
	}
	if len(i.scopes[scopeKey(root, "models")].manifests) != 0 || len(i.scopes[scopeKey(root, "models")].errors) != 0 {
		t.Fatal("catalog touched file manifests")
	}
	t.Logf("100 legacy repositories, 2000 deep file entries: catalog %s", time.Since(start))
}
