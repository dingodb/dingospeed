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

// ListHosted must return exactly the hosted subset List returns, so callers that
// only consume hosted descriptors can stop paying for a whole api/ walk.
func TestListHostedMatchesHostedSubsetOfList(t *testing.T) {
	root := t.TempDir()
	hosted := []RepoKey{
		{Namespace: "alice", RepoType: "models", Repo: "team/model-a"},
		{Namespace: "alice", RepoType: "datasets", Repo: "corpus"},
		{Namespace: "bob", RepoType: "spaces", Repo: "demo"},
	}
	for _, k := range hosted {
		if err := Register(root, Hosted(k)); err != nil {
			t.Fatalf("register hosted %s: %v", k.ID(), err)
		}
	}
	for _, k := range []RepoKey{
		{Namespace: HuggingFace, RepoType: "models", Repo: "Qwen/Qwen3-32B"},
		{Namespace: ModelScope, RepoType: "models", Repo: "Qwen/Qwen3"},
	} {
		if err := Register(root, Remote(k)); err != nil {
			t.Fatalf("register remote %s: %v", k.ID(), err)
		}
	}

	all, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Descriptor{}
	for _, d := range all {
		if d.Source == "hosted" {
			want[d.ID()] = d
		}
	}
	if len(want) != len(hosted) {
		t.Fatalf("fixture mismatch: List returned %d hosted of %d total", len(want), len(all))
	}

	got, err := ListHosted(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListHosted returned %d descriptors, want %d: %+v", len(got), len(want), got)
	}
	for _, d := range got {
		expected, ok := want[d.ID()]
		if !ok {
			t.Fatalf("ListHosted returned non-hosted or unknown descriptor: %+v", d)
		}
		if d != expected {
			t.Fatalf("descriptor mismatch for %s: %+v want %+v", d.ID(), d, expected)
		}
	}
}

// Remote cache trees dominate api/ on a heavily used node. A hosted-scoped scan
// must not descend into them at all.
func TestListHostedDoesNotDescendIntoRemoteNamespace(t *testing.T) {
	root := t.TempDir()
	hostedKey := RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/model-a"}
	if err := Register(root, Hosted(hostedKey)); err != nil {
		t.Fatal(err)
	}
	remote := RepoKey{Namespace: HuggingFace, RepoType: "models", Repo: "Qwen/Qwen3-32B"}
	if err := Register(root, Remote(remote)); err != nil {
		t.Fatal(err)
	}
	// A marker planted deep inside the remote cache must never be enumerated by
	// the hosted scan, even though it is a structurally valid repository root.
	decoy := filepath.Join(remote.APIRoot(root), "paths-info", "commit", "nested", Marker)
	if err := os.MkdirAll(filepath.Dir(decoy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decoy, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ListHosted(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID() != hostedKey.ID() {
		t.Fatalf("hosted scan leaked remote cache entries: %+v", got)
	}
}

// Data subtrees are not repositories. Descending them is what turns discovery
// into a walk of every stored file.
func TestListStopsAtRepositoryDataSubtrees(t *testing.T) {
	root := t.TempDir()
	k := RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/model-a"}
	if err := Register(root, Hosted(k)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"paths-info", "revision", "recycle"} {
		decoy := filepath.Join(k.APIRoot(root), name, "commit", "nested", Marker)
		if err := os.MkdirAll(filepath.Dir(decoy), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(decoy, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, list := range []struct {
		name string
		fn   func(string) ([]Descriptor, error)
	}{{"List", List}, {"ListHosted", ListHosted}} {
		all, err := list.fn(root)
		if err != nil {
			t.Fatalf("%s: %v", list.name, err)
		}
		if len(all) != 1 || all[0].ID() != k.ID() {
			t.Fatalf("%s descended into repository data subtrees: %+v", list.name, all)
		}
	}
}

// Empty or absent hosted roots are normal, not an error.
func TestListHostedToleratesMissingRoots(t *testing.T) {
	root := t.TempDir()
	got, err := ListHosted(root)
	if err != nil {
		t.Fatalf("missing api/ must not fail: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no repositories, got %+v", got)
	}
	k := RepoKey{Namespace: "alice", RepoType: "models", Repo: "only-models"}
	if err := Register(root, Hosted(k)); err != nil {
		t.Fatal(err)
	}
	got, err = ListHosted(root)
	if err != nil {
		t.Fatalf("absent datasets/spaces roots must not fail: %v", err)
	}
	if len(got) != 1 || got[0].ID() != k.ID() {
		t.Fatalf("unexpected hosted enumeration: %+v", got)
	}
}

// Orphan residue is a deleted repository whose marker is gone but whose
// paths-info tree was left behind. It contributes nothing to enumeration.
//
// It also cannot be pruned by directory name alone: a repository may legitimately
// be named "paths-info/repo", and that tree is byte-for-byte indistinguishable
// from orphan residue under a markerless parent. What matters here is that the
// residue is not enumerated as a repository and does not fail the scan.
func TestListDoesNotEnumerateOrphanResidue(t *testing.T) {
	root := t.TempDir()
	keep := RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/keeper"}
	if err := Register(root, Hosted(keep)); err != nil {
		t.Fatal(err)
	}
	// A removed repository: api/models/alice/ghost/ with paths-info but no marker.
	orphanRoot := filepath.Join(root, "api", "models", "alice", "ghost")
	deep := filepath.Join(orphanRoot, "paths-info", "commit", "weights", "file.bin")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	// A marker planted at the bottom of the orphan tree is not a valid
	// descriptor, so enumeration must either skip it or report the malformed
	// marker; it must never materialise a bogus repository. A valid marker there
	// would describe a repository whose path does not match, which is rejected
	// separately. Here the residue is simply never enumerated as a repository.
	for _, list := range []struct {
		name string
		fn   func(string) ([]Descriptor, error)
	}{{"List", List}, {"ListHosted", ListHosted}} {
		all, _ := list.fn(root)
		for _, d := range all {
			if d.ID() == "alice/ghost" || d.Repo == "ghost" {
				t.Fatalf("%s enumerated orphan residue as a repository: %+v", list.name, d)
			}
		}
	}
}

// Even with no marker anywhere, a bare data subtree must not yield descriptors.
func TestListDoesNotDescendBareDataSubtree(t *testing.T) {
	root := t.TempDir()
	orphan := filepath.Join(root, "api", "models", "alice", "ghost")
	deep := filepath.Join(orphan, "paths-info", "commit", "weights", "file.bin")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	all, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("bare data subtree produced descriptors: %+v", all)
	}
}

// A repository whose own name contains a data-subtree word stays discoverable.
// This is why pruning can never be decided by directory name alone.
func TestListKeepsRepositoryNamedLikeDataSubtree(t *testing.T) {
	root := t.TempDir()
	for _, repo := range []string{"team/resolve/model", "team/revision/model", "team/recycle/model", "paths-info/repo"} {
		k := RepoKey{Namespace: "alice", RepoType: "models", Repo: repo}
		if err := Register(root, Hosted(k)); err != nil {
			t.Fatalf("register %s: %v", repo, err)
		}
	}
	all, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("data-subtree-worded repositories were pruned: %+v", all)
	}
	hosted, err := ListHosted(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosted) != 4 {
		t.Fatalf("ListHosted pruned data-subtree-worded repositories: %+v", hosted)
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
