package repository

import (
	"path/filepath"
	"testing"
)

func TestLegacyRemoteAndNamespacedUploadPhysicalPaths(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ namespace, repo, files, api string }{
		{HuggingFace, "Qwen/demo", "files/models/Qwen/demo", "api/models/Qwen/demo"},
		{ModelScope, "Qwen/demo", "modelscope/models/Qwen/demo", "api/models/modelscope/Qwen/demo"},
		{"alice", "demo", "files/models/dingo-local/alice/demo", "api/models/dingo-local/alice/demo"},
		{"bob", "demo", "files/models/dingo-local/bob/demo", "api/models/dingo-local/bob/demo"},
		{Company, "demo", "files/models/dingo-local/datacanvas/demo", "api/models/dingo-local/datacanvas/demo"},
	} {
		k := RepoKey{Namespace: tc.namespace, RepoType: "models", Repo: tc.repo}
		if k.FilesRoot(root) != filepath.Join(root, filepath.FromSlash(tc.files)) || k.APIRoot(root) != filepath.Join(root, filepath.FromSlash(tc.api)) {
			t.Fatalf("wrong layout for %+v: %s %s", k, k.FilesRoot(root), k.APIRoot(root))
		}
	}
}

func TestLegacyAliasCannotOverlapPersonalRepository(t *testing.T) {
	root := t.TempDir()
	if err := Register(root, Hosted(RepoKey{Namespace: "alice", RepoType: "models", Repo: "demo"})); err != nil {
		t.Fatal(err)
	}
	if err := Register(root, Hosted(RepoKey{Namespace: Local, RepoType: "models", Repo: "alice/demo"})); err == nil {
		t.Fatal("legacy alias shares physical storage with another namespace")
	}
}
