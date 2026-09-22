package repository

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMultiLevelIdentityAndNamespaceIsolation(t *testing.T) {
	root := t.TempDir()
	keys := []RepoKey{
		{Namespace: HuggingFace, RepoType: "models", Repo: "Qwen/Qwen3"},
		{Namespace: ModelScope, RepoType: "models", Repo: "Qwen/Qwen3"},
		{Namespace: "alice", RepoType: "models", Repo: "team/model-a"},
		{Namespace: "bob", RepoType: "models", Repo: "team/model-a"},
	}
	paths := map[string]bool{}
	for _, k := range keys {
		parsed, err := ParseID(k.RepoType, k.ID())
		if err != nil || parsed != k {
			t.Fatalf("lost identity %v %v", parsed, err)
		}
		blob := k.Blob(root, "same-content")
		if paths[blob] {
			t.Fatal("namespaces share a blob path")
		}
		paths[blob] = true
		d := Hosted(k)
		if k.Namespace == HuggingFace || k.Namespace == ModelScope {
			d = Remote(k)
		}
		if err = Register(root, d); err != nil {
			t.Fatal(err)
		}
		got, err := Read(root, k)
		if err != nil || got != d {
			t.Fatalf("descriptor roundtrip: %v %v", got, err)
		}
		located, rel, err := Locate(root, k.Resolve(root, "commit", "weights/a.bin"))
		if err != nil || located != d || rel != "resolve/commit/weights/a.bin" {
			t.Fatalf("wrong root: %+v %s %v", located, rel, err)
		}
	}
	all, err := List(root)
	if err != nil || len(all) != 4 {
		t.Fatalf("enumeration: %d %v", len(all), err)
	}
}
func TestInvalidRepositoryLocators(t *testing.T) {
	invalid := []RepoKey{
		{Namespace: "", RepoType: "models", Repo: "repo"},
		{Namespace: "alice/bob", RepoType: "models", Repo: "repo"},
		{Namespace: "alice", RepoType: "unknown", Repo: "repo"},
	}
	for _, repo := range []string{"", "/repo", "repo/", "a//b", "a/../b", "./a", `a`, "a/nul.txt", "a/name.", "a/\x00bad"} {
		invalid = append(invalid, RepoKey{Namespace: "alice", RepoType: "models", Repo: repo})
	}
	for _, k := range invalid {
		if k.Validate() == nil {
			t.Fatalf("accepted %+v", k)
		}
	}
	for _, name := range []string{"team/resolve/model", "tree/revision/model", "Qwen/Qwen3"} {
		if err := (RepoKey{Namespace: "alice", RepoType: "models", Repo: name}).Validate(); err != nil {
			t.Fatalf("HTTP operation name incorrectly reserved: %s %v", name, err)
		}
	}
}
func TestRegistrationRejectsCaseAndAncestorConflicts(t *testing.T) {
	for _, repo := range []string{"team", "team/model-a/child", "TEAM/model-a"} {
		root := t.TempDir()
		first := Hosted(RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/model-a"})
		if err := Register(root, first); err != nil {
			t.Fatal(err)
		}
		next := Hosted(RepoKey{Namespace: "alice", RepoType: "models", Repo: repo})
		if err := Register(root, next); err == nil {
			t.Fatalf("accepted conflict %s", repo)
		}
	}
	root := t.TempDir()
	if err := Register(root, Hosted(RepoKey{Namespace: "Alice", RepoType: "models", Repo: "a"})); err != nil {
		t.Fatal(err)
	}
	if err := Register(root, Hosted(RepoKey{Namespace: "alice", RepoType: "datasets", Repo: "b"})); err == nil {
		t.Fatal("namespace case collision crosses repo types")
	}
	// Segment-aware comparison must allow sibling names.
	if err := Register(root, Hosted(RepoKey{Namespace: "Alice", RepoType: "models", Repo: "a-b"})); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentIdempotentRegistrationAndConflicts(t *testing.T) {
	root := t.TempDir()
	d := Hosted(RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/model-a"})
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- Register(root, d) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	all, err := List(root)
	if err != nil || len(all) != 1 {
		t.Fatalf("concurrent registration duplicated: %v %v", all, err)
	}
	root = t.TempDir()
	results := make(chan error, 2)
	for _, repo := range []string{"team", "team/model-a"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			results <- Register(root, Hosted(RepoKey{Namespace: "alice", RepoType: "models", Repo: name}))
		}(repo)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("conflicting concurrent registrations succeeded %d times", success)
	}
}
func TestInvalidDescriptorDoesNotBecomeRemoteOrDeletable(t *testing.T) {
	root := t.TempDir()
	k := RepoKey{Namespace: "alice", RepoType: "models", Repo: "model"}
	d := Hosted(k)
	d.Persistent = false
	if Register(root, d) == nil {
		t.Fatal("accepted nonpersistent hosted data")
	}
	d = Remote(k)
	if Register(root, d) == nil {
		t.Fatal("accepted user namespace as remote")
	}
	d = Hosted(RepoKey{Namespace: HuggingFace, RepoType: "models", Repo: "Qwen/Qwen3"})
	if Register(root, d) == nil {
		t.Fatal("accepted hosted upload into reserved remote namespace")
	}
	if err := Register(root, Hosted(k)); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(k.APIRoot(root), Marker)
	if err := os.WriteFile(marker, []byte("{broken"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root, k); err == nil {
		t.Fatal("read ignored bad descriptor")
	}
	if _, err := List(root); err == nil {
		t.Fatal("scan ignored bad descriptor")
	}
}
func TestMarkerIdentityMustMatchItsLocation(t *testing.T) {
	root := t.TempDir()
	k := RepoKey{Namespace: "alice", RepoType: "models", Repo: "model"}
	if err := Register(root, Hosted(k)); err != nil {
		t.Fatal(err)
	}
	wrong := Hosted(RepoKey{Namespace: "bob", RepoType: "models", Repo: "model"})
	b, _ := json.Marshal(wrong)
	if err := os.WriteFile(filepath.Join(k.APIRoot(root), Marker), b, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root, k); err == nil {
		t.Fatal("accepted marker belonging to another namespace")
	}
	if _, err := List(root); err == nil {
		t.Fatal("scan accepted wrong marker location")
	}
}
func TestSafePathAndSymlinkRegistration(t *testing.T) {
	root := t.TempDir()
	if err := SafePath(root, filepath.Join(root, "..", "escape")); err == nil {
		t.Fatal("allowed traversal")
	}
	if err := SafePath(root, filepath.Join(root, "new", "repo")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	parent := filepath.Join(root, "api", "models", "dingo-local")
	if err := os.MkdirAll(parent, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "alice")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := Register(root, Hosted(RepoKey{Namespace: "alice", RepoType: "models", Repo: "model"})); err == nil {
		t.Fatal("registered through an escaping directory symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("registration wrote outside storage root")
	}
}
func TestNestedMetadataMarkerNameDoesNotCreateAnotherRepository(t *testing.T) {
	root := t.TempDir()
	k := RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/resolve/model"}
	if err := Register(root, Hosted(k)); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(k.APIRoot(root), "revision", "commit", Marker)
	if err := os.MkdirAll(filepath.Dir(nested), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("ordinary nested metadata"), 0644); err != nil {
		t.Fatal(err)
	}
	all, err := List(root)
	if err != nil || len(all) != 1 {
		t.Fatalf("scanned repository contents as roots: %v %v", all, err)
	}
}

func TestMarkerWordIsAllowedAsRepositoryOrNamespaceDirectory(t *testing.T) {
	root := t.TempDir()
	keys := []RepoKey{
		{Namespace: "alice", RepoType: "models", Repo: "team/repository.json/model"},
		{Namespace: "alice", RepoType: "models", Repo: "repository.json"},
		{Namespace: "repository.json", RepoType: "models", Repo: "model"},
	}
	for _, k := range keys {
		if err := Register(root, Hosted(k)); err != nil {
			t.Fatalf("register %s: %v", k.ID(), err)
		}
		if _, err := Read(root, k); err != nil {
			t.Fatalf("read %s: %v", k.ID(), err)
		}
	}
	all, err := List(root)
	if err != nil || len(all) != len(keys) {
		t.Fatalf("marker-named directories broke discovery: %v %v", all, err)
	}
}

func TestCommonDirectoryCasingCannotAliasAcrossRepositoryRoots(t *testing.T) {
	root := t.TempDir()
	base := RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/model-a"}
	if err := Register(root, Hosted(base)); err != nil {
		t.Fatal(err)
	}
	for _, k := range []RepoKey{
		{Namespace: "ALICE", RepoType: "models", Repo: "team/model-b"},
		{Namespace: "alice", RepoType: "models", Repo: "Team/model-b"},
	} {
		if err := Register(root, Hosted(k)); err == nil {
			t.Fatalf("accepted casing alias in common directory: %s", k.ID())
		}
	}
	if err := Register(root, Hosted(RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/model-b"})); err != nil {
		t.Fatalf("ordinary sibling rejected: %v", err)
	}
	all, err := List(root)
	if err != nil || len(all) != 2 {
		t.Fatalf("directory casing damaged registry: %+v %v", all, err)
	}
}
