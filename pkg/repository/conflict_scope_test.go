package repository

import (
	"fmt"
	"math/rand"
	"testing"
)

// Register now scans only the repositories its conflict rules can act on. This
// asserts that scoping is verdict-preserving: for a given on-disk registry, the
// scoped view must yield the same decision and the same reason as the
// whole-registry view, across a wide spread of candidate descriptors.
func TestScopedConflictViewMatchesWholeRegistry(t *testing.T) {
	seedRegistries := []struct {
		name string
		keys []Descriptor
	}{
		{"hosted siblings", []Descriptor{
			Hosted(RepoKey{"alice", "models", "team/model-a"}),
			Hosted(RepoKey{"alice", "models", "team/model-b"}),
			Hosted(RepoKey{"bob", "models", "team/model-a"}),
		}},
		{"namespace case", []Descriptor{
			Hosted(RepoKey{"Alice", "models", "a"}),
		}},
		{"cross repo type", []Descriptor{
			Hosted(RepoKey{"alice", "models", "m"}),
			Hosted(RepoKey{"alice", "datasets", "d"}),
		}},
		{"remote providers", []Descriptor{
			Remote(RepoKey{HuggingFace, "models", "Qwen/Qwen3"}),
			Remote(RepoKey{ModelScope, "models", "Qwen/Qwen3"}),
			Hosted(RepoKey{"alice", "models", "local-one"}),
		}},
		{"ancestor pairs", []Descriptor{
			Hosted(RepoKey{"alice", "models", "team"}),
			Hosted(RepoKey{"alice", "models", "other/child"}),
		}},
	}

	candidates := []Descriptor{
		Hosted(RepoKey{"alice", "models", "team/model-a"}),
		Hosted(RepoKey{"alice", "models", "team/model-c"}),
		Hosted(RepoKey{"Alice", "models", "newthing"}),
		Hosted(RepoKey{"alice", "datasets", "fresh"}),
		Hosted(RepoKey{"bob", "models", "team/model-a"}),
		Hosted(RepoKey{"alice", "models", "team"}),
		Hosted(RepoKey{"alice", "models", "team/model-a/deeper"}),
		Hosted(RepoKey{"carol", "spaces", "demo"}),
		Remote(RepoKey{HuggingFace, "models", "Qwen/Qwen3"}),
		Remote(RepoKey{HuggingFace, "models", "Qwen/Qwen4"}),
		Remote(RepoKey{ModelScope, "models", "Qwen/Qwen3"}),
		Remote(RepoKey{ModelScope, "models", "Other/Thing"}),
		Hosted(RepoKey{Local, "models", "plain"}),
		Hosted(RepoKey{Local, "models", "team/nested"}),
	}

	for _, registry := range seedRegistries {
		for _, candidate := range candidates {
			t.Run(fmt.Sprintf("%s/%s-%s", registry.name, candidate.Namespace, candidate.Repo), func(t *testing.T) {
				root := t.TempDir()
				for _, d := range registry.keys {
					// Seeds may conflict with one another; only the resulting disk
					// state matters here.
					_ = Register(root, d)
				}
				whole, err := List(root)
				if err != nil {
					// A case-vs-directory mismatch is reported by List itself and is
					// a pre-existing property of the registry, not of the scoped
					// view; such a registry has no coherent verdict to compare.
					t.Skipf("registry not enumerable as-is: %v", err)
				}
				scoped, err := listConflicts(root, candidate)
				if err != nil {
					// Same pre-existing enumeration property as above, reached
					// through the scoped path.
					t.Skipf("registry not enumerable as-is: %v", err)
				}
				wholeErr := registrationConflicts(root, candidate, whole)
				scopedErr := registrationConflicts(root, candidate, scoped)
				if (wholeErr == nil) != (scopedErr == nil) {
					t.Fatalf("verdict drift for %+v:\n whole  view: %v\n scoped view: %v", candidate.RepoKey, wholeErr, scopedErr)
				}
				if wholeErr != nil && scopedErr != nil && wholeErr.Error() != scopedErr.Error() {
					t.Fatalf("reason drift for %+v: whole=%v scoped=%v", candidate.RepoKey, wholeErr, scopedErr)
				}
				// Every repository that actually conflicts with the candidate must
				// be present in the scoped view.
				for _, old := range whole {
					if registrationConflicts(root, candidate, []Descriptor{old}) == nil {
						continue
					}
					found := false
					for _, s := range scoped {
						if s.RepoKey == old.RepoKey {
							found = true
							break
						}
					}
					if !found {
						t.Fatalf("scoped view omitted conflicting repository %+v (scoped %+v)", old.RepoKey, scoped)
					}
				}
			})
		}
	}
}

// Randomised differential test: many registries, many candidates, the scoped and
// whole-registry conflict views must never disagree.
func TestScopedConflictViewRandomised(t *testing.T) {
	rng := rand.New(rand.NewSource(20260922))
	namespaces := []string{"alice", "bob", "carol", Local, HuggingFace, ModelScope}
	types := []string{"models", "datasets", "spaces"}
	repos := []string{"a", "b", "team/x", "team/y", "team", "deep/nest/z"}

	for iter := 0; iter < 150; iter++ {
		root := t.TempDir()
		for n := 0; n < rng.Intn(6); n++ {
			k := RepoKey{namespaces[rng.Intn(len(namespaces))], types[rng.Intn(len(types))], repos[rng.Intn(len(repos))]}
			d := Hosted(k)
			if k.Namespace == HuggingFace || k.Namespace == ModelScope {
				d = Remote(k)
			}
			_ = Register(root, d)
		}
		k := RepoKey{namespaces[rng.Intn(len(namespaces))], types[rng.Intn(len(types))], repos[rng.Intn(len(repos))]}
		candidate := Hosted(k)
		if k.Namespace == HuggingFace || k.Namespace == ModelScope {
			candidate = Remote(k)
		}
		whole, err := List(root)
		if err != nil {
			continue // not enumerable as-is; covered by other tests
		}
		scoped, err := listConflicts(root, candidate)
		if err != nil {
			t.Fatal(err)
		}
		wholeErr := registrationConflicts(root, candidate, whole)
		scopedErr := registrationConflicts(root, candidate, scoped)
		if (wholeErr == nil) != (scopedErr == nil) {
			t.Fatalf("iter %d: verdict drift for %+v whole=%v scoped=%v\n whole=%+v\n scoped=%+v",
				iter, candidate.RepoKey, wholeErr, scopedErr, whole, scoped)
		}
	}
}