package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverLegacyAndScopedIsolation(t *testing.T) {
	root := t.TempDir()
	k := RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/resolve/model"}
	p := filepath.Join(k.APIRoot(root), "revision", "main", "dingo-local-manifest.json")
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("[]"), 0644); err != nil {
		t.Fatal(err)
	}
	// Poison markers prove neither unrelated providers nor a recognised repository's
	// internal tree is visited, regardless of how many entries it contains.
	for _, dir := range []string{filepath.Join(root, "api", "models", "Qwen", "old"), filepath.Join(k.APIRoot(root), "paths-info", "deep", "file")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, Marker), []byte("broken"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Discover(context.Background(), root, "models", "alice")
	if err != nil || len(got) != 1 || got[0] != k {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(k.APIRoot(root), Marker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("discovery wrote marker")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, root, "models", "alice"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
func TestDiscoverLocalDoesNotMisidentifyNamedNamespace(t *testing.T) {
	root := t.TempDir()
	k := RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/model"}
	if err := Register(root, Hosted(k)); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(context.Background(), root, "models", Local)
	if err != nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}
