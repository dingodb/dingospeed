package hfprojection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManifestAndPreciseInvalidationDoNotWaitForCatalog(t *testing.T) {
	r, _ := fixture(t)
	i := NewIndex()
	i.readCatalog = func(context.Context, string, string) (Catalog, error) {
		t.Error("manifest must not scan catalogue")
		return Catalog{}, errors.New("unexpected scan")
	}
	// Simulate a slow full scan already occupying its gate.
	i.refreshGate <- struct{}{}
	defer func() { <-i.refreshGate }()
	done := make(chan error, 1)
	go func() {
		i.InvalidateRepository(r.Root, "models", "other/repo")
		i.InvalidateRevision(r.Root, "models", "other/repo", "main")
		_, err := i.Manifest(r.Root, "models", "Qwen/demo", "main")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("single repository read waited for unrelated catalogue scan")
	}
}

func TestPreciseInvalidationPreservesOtherRepositoriesAndCommits(t *testing.T) {
	i := NewIndex()
	key := scopeKey("root", "models")
	s := i.scope(key)
	s.manifests[manifestKey("org/a", "main")] = Manifest{Commit: "old"}
	s.manifests[manifestKey("org/a", "old")] = Manifest{Commit: "old"}
	s.manifests[manifestKey("org/a", "dev")] = Manifest{Commit: "devcommit"}
	s.manifests[manifestKey("org/b", "main")] = Manifest{Commit: "b"}
	i.InvalidateRevision("root", "models", "org/a", "main")
	s = i.scopes[key]
	if len(s.manifests) != 3 {
		t.Fatalf("alias invalidation discarded unrelated revisions: %+v", s.manifests)
	}
	if _, ok := s.manifests[manifestKey("org/a", "old")]; !ok {
		t.Fatal("moving alias evicted immutable old commit")
	}
	// Deleting a commit must also evict aliases cached against it.
	s.manifests[manifestKey("org/a", "tag")] = Manifest{Commit: "old"}
	i.InvalidateRevision("root", "models", "org/a", "old")
	if len(i.scopes[key].manifests) != 2 {
		t.Fatal("commit invalidation failed to remove dependent alias")
	}
	i.InvalidateRepository("root", "models", "org/a")
	if len(i.scopes[key].manifests) != 1 {
		t.Fatal("repository invalidation affected sibling or retained stale entry")
	}
}

func TestPreciseChangeCannotBeOverwrittenByOldFullRefresh(t *testing.T) {
	r, _ := fixture(t)
	i := NewIndex()
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	i.readCatalog = func(ctx context.Context, root, typ string) (Catalog, error) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
			return Catalog{RepoType: typ}, nil
		}
		return (Reader{Root: root, Context: ctx}).Catalog(typ)
	}
	result := make(chan Catalog, 1)
	errs := make(chan error, 1)
	go func() { c, err := i.Refresh(r.Root, "models"); result <- c; errs <- err }()
	<-entered
	i.InvalidateRevision(r.Root, "models", "Qwen/demo", "main")
	if _, err := i.RefreshRepositoryContext(context.Background(), r.Root, "models", "Qwen/demo"); err != nil {
		t.Fatal(err)
	}
	close(release)
	c := <-result
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(c.Repos) != 1 || len(i.scopes[scopeKey(r.Root, "models")].catalog.Repos) != 1 {
		t.Fatalf("stale refresh replaced precise state: calls=%d catalog=%+v", calls, c)
	}
}

func TestRepositoryRefreshDoesNotDiscoverSiblings(t *testing.T) {
	r, _ := fixture(t)
	i := NewIndex()
	i.readCatalog = func(context.Context, string, string) (Catalog, error) {
		t.Fatal("precise refresh scanned all repositories")
		return Catalog{}, nil
	}
	other := i.scope(scopeKey(r.Root, "models"))
	other.manifests[manifestKey("org/other", "main")] = Manifest{Commit: "keep"}
	repo, err := i.RefreshRepositoryContext(context.Background(), r.Root, "models", "Qwen/demo")
	if err != nil || repo.Repo != "Qwen/demo" {
		t.Fatalf("%+v %v", repo, err)
	}
	if _, ok := i.scopes[scopeKey(r.Root, "models")].manifests[manifestKey("org/other", "main")]; !ok {
		t.Fatal("lost unrelated manifest")
	}
	if _, err := i.RefreshRepositoryContext(context.Background(), r.Root, "models", "org/absent"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent repository: %v", err)
	}
}

func TestAliasInvalidationReadsNewMetadataWithoutCatalog(t *testing.T) {
	r, api := fixture(t)
	i := NewIndex()
	first, err := i.Manifest(r.Root, "models", "Qwen/demo", "main")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(api, "revision", "main", "meta_get.json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	response(t, path, map[string]any{"sha": "commit2", "siblings": []any{}})
	i.InvalidateRevision(r.Root, "models", "Qwen/demo", "main")
	next, err := i.Manifest(r.Root, "models", "Qwen/demo", "main")
	if err != nil || first.Commit == next.Commit || next.Commit != "commit2" {
		t.Fatalf("first=%s next=%+v err=%v", first.Commit, next, err)
	}
}

// A cache hit remains usable even if B's disk metadata is temporarily unreadable.
func TestOtherRepositoryChangePreservesCachedManifest(t *testing.T) {
	r, api := fixture(t)
	i := NewIndex()
	cached, err := i.Manifest(r.Root, "models", "Qwen/demo", "main")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(api, "revision", "main", "meta_get.json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("invalid: a disk reread must fail"), 0644); err != nil {
		t.Fatal(err)
	}
	i.InvalidateRevision(r.Root, "models", "org/a", "main")
	i.InvalidateRepository(r.Root, "models", "org/a")
	// Also exercise a precise catalogue update to A.
	response(t, filepath.Join(r.Root, "api", "models", "org", "a", "revision", "main", "meta_get.json"), map[string]any{"sha": "a1"})
	if _, err := i.RefreshRepositoryContext(context.Background(), r.Root, "models", "org/a"); err != nil {
		t.Fatal(err)
	}
	got, err := i.Manifest(r.Root, "models", "Qwen/demo", "main")
	if err != nil || got.Commit != cached.Commit {
		t.Fatalf("B cache was evicted or reread: %+v %v", got, err)
	}
}

func TestRepositoryReadDoesNotRetryForSiblingInvalidation(t *testing.T) {
	r, _ := fixture(t)
	i := NewIndex()
	calls := 0
	i.readRepository = func(ctx context.Context, root, typ, repo string) (Repo, error) {
		calls++
		// Every scan overlaps a sibling mutation. A scope-wide token would cause
		// endless retries until cancellation; a repository token needs one read.
		i.InvalidateRevision(root, typ, "org/b", "main")
		return (Reader{Root: root, Context: ctx}).Repository(typ, repo)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := i.RefreshRepositoryContext(ctx, r.Root, "models", "Qwen/demo"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("unrelated mutation caused %d reads", calls)
	}
}
