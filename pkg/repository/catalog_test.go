package repository

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// buildMixedNode models a mirror node upgraded from a release that predates
// repository markers: a few registered repositories next to unmarked caches whose
// metadata trees run deep. Each unmarked tree hides a malformed marker at the
// bottom, so any scan that opens it fails instead of passing silently.
func buildMixedNode(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	registered := []RepoKey{
		{Namespace: HuggingFace, RepoType: "models", Repo: "Qwen/registered"},
		{Namespace: HuggingFace, RepoType: "models", Repo: "gpt2"},
		{Namespace: ModelScope, RepoType: "models", Repo: "owner/registered"},
		{Namespace: HuggingFace, RepoType: "datasets", Repo: "org/registered-set"},
	}
	for _, k := range registered {
		if err := Register(root, Remote(k)); err != nil {
			t.Fatalf("register %s: %v", k.ID(), err)
		}
	}
	hosted := RepoKey{Namespace: "alice", RepoType: "models", Repo: "team/hosted"}
	if err := Register(root, Hosted(hosted)); err != nil {
		t.Fatal(err)
	}
	api := filepath.Join(root, "api")
	for _, legacy := range []string{
		"models/Qwen/legacy/paths-info/commit/weights",
		"models/bert-base-uncased/revision/main/nested",
		"models/modelscope/owner/legacy/revision/main/nested",
		"datasets/org/legacy-set/paths-info/commit/data",
	} {
		dir := filepath.Join(api, filepath.FromSlash(legacy))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, Marker), []byte("not a descriptor"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An organisation directory with nothing cached is neither registered nor a
	// cache, so it is not reported.
	if err := os.MkdirAll(filepath.Join(api, "models", "Qwen", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	ids := []string{hosted.ID()}
	for _, k := range registered {
		ids = append(ids, k.ID())
	}
	sort.Strings(ids)
	return root, ids
}

func descriptorIDs(all []Descriptor) []string {
	ids := make([]string, 0, len(all))
	for _, d := range all {
		ids = append(ids, d.ID())
	}
	sort.Strings(ids)
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// List must not open the metadata trees of unmarked upstream caches: on a mirror
// node those trees are where nearly all of the directories are.
func TestListDoesNotWalkUnmarkedUpstreamCaches(t *testing.T) {
	root, want := buildMixedNode(t)
	all, err := List(root)
	if err != nil {
		t.Fatalf("List opened an unmarked cache tree: %v", err)
	}
	if got := descriptorIDs(all); !equalStrings(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
}

// ListCatalog reports unmarked caches next to the registered repositories,
// including Hugging Face IDs without an organisation and ModelScope caches.
func TestListCatalogReportsUnregisteredUpstreamCaches(t *testing.T) {
	root, want := buildMixedNode(t)
	c, err := ListCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := descriptorIDs(c.Registered); !equalStrings(got, want) {
		t.Fatalf("Registered = %v, want %v", got, want)
	}
	got := []string{}
	for _, k := range c.Unregistered {
		got = append(got, k.RepoType+":"+k.ID())
	}
	sort.Strings(got)
	wantUnregistered := []string{
		"datasets:huggingface/org/legacy-set",
		"models:huggingface/Qwen/legacy",
		"models:huggingface/bert-base-uncased",
		"models:modelscope/owner/legacy",
	}
	if !equalStrings(got, wantUnregistered) {
		t.Fatalf("Unregistered = %v, want %v", got, wantUnregistered)
	}
}

// On a node where every repository is registered, the bounded scan returns
// exactly what a full walk of api/ returns.
func TestListMatchesFullWalkOnRegisteredNode(t *testing.T) {
	root := buildHeavyNode(t, 3, 5, 4)
	for _, k := range []RepoKey{
		{Namespace: HuggingFace, RepoType: "datasets", Repo: "bigscience/set"},
		{Namespace: HuggingFace, RepoType: "models", Repo: "gpt2"},
		{Namespace: ModelScope, RepoType: "models", Repo: "owner/model"},
	} {
		if err := Register(root, Remote(k)); err != nil {
			t.Fatal(err)
		}
	}
	walked, err := listIn(filepath.Join(root, "api"), root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(descriptorIDs(got), descriptorIDs(walked)) {
		t.Fatalf("List = %v, full walk = %v", descriptorIDs(got), descriptorIDs(walked))
	}
}
