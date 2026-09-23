package repository

import (
	"strconv"
	"testing"
)

// buildNamespacedRegistry creates n namespaces with perNamespace hosted
// repositories each, so scoping can be observed against namespace count.
func buildNamespacedRegistry(t *testing.T, root string, namespaces, perNamespace int) {
	t.Helper()
	for n := 0; n < namespaces; n++ {
		ns := "ns-" + strconv.Itoa(n)
		for i := 0; i < perNamespace; i++ {
			k := RepoKey{Namespace: ns, RepoType: "models", Repo: "team/repo-" + strconv.Itoa(i)}
			if err := Register(root, Hosted(k)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The scoped conflict view must not grow with repositories in unrelated
// namespaces, while the whole-registry view does.
func TestScopedConflictSetDoesNotGrowWithUnrelatedNamespaces(t *testing.T) {
	candidate := Hosted(RepoKey{Namespace: "target", RepoType: "models", Repo: "team/new"})

	measure := func(namespaces, perNamespace int) (scopedCount, wholeCount int) {
		root := t.TempDir()
		buildNamespacedRegistry(t, root, namespaces, perNamespace)
		scoped, err := listConflicts(root, candidate)
		if err != nil {
			t.Fatal(err)
		}
		whole, err := List(root)
		if err != nil {
			t.Fatal(err)
		}
		return len(scoped), len(whole)
	}

	scopedSmall, wholeSmall := measure(2, 10)
	scopedLarge, wholeLarge := measure(40, 10)
	t.Logf("2 namespaces : scoped=%d whole=%d", scopedSmall, wholeSmall)
	t.Logf("40 namespaces: scoped=%d whole=%d", scopedLarge, wholeLarge)

	if wholeLarge <= wholeSmall {
		t.Fatalf("whole registry did not grow: %d -> %d", wholeSmall, wholeLarge)
	}
	if scopedLarge != scopedSmall {
		t.Fatalf("scoped conflict set grew with unrelated namespaces: %d -> %d", scopedSmall, scopedLarge)
	}
}

// End-to-end: registering a batch against an existing registry must not rescan
// the whole registry per call. Work may grow quadratically within the candidate's
// own namespace, but must never include the unrelated namespaces.
func TestRegisterBatchDoesNotRescanWholeRegistry(t *testing.T) {
	const seedNamespaces, seedPerNamespace = 20, 10
	const targetPerNamespace = 5

	work := func(batch int) int {
		root := t.TempDir()
		buildNamespacedRegistry(t, root, seedNamespaces, seedPerNamespace)
		total := 0
		for i := 0; i < batch; i++ {
			candidate := Hosted(RepoKey{Namespace: "target", RepoType: "models", Repo: "team/new-" + strconv.Itoa(i)})
			scoped, err := listConflicts(root, candidate)
			if err != nil {
				t.Fatal(err)
			}
			total += len(scoped)
			if err := Register(root, candidate); err != nil {
				t.Fatal(err)
			}
		}
		return total
	}

	small := work(targetPerNamespace)
	large := work(targetPerNamespace * 3)
	// The seeded registry alone holds 200 repositories. If any step rescanned it,
	// these totals would be at least that large.
	const seededRepositories = seedNamespaces * seedPerNamespace
	t.Logf("conflict entries examined: batch %d -> %d, batch %d -> %d (seeded registry holds %d)",
		targetPerNamespace, small, targetPerNamespace*3, large, seededRepositories)
	if small >= seededRepositories || large >= seededRepositories {
		t.Fatalf("batch work included the whole seeded registry: %d/%d, seeded=%d", small, large, seededRepositories)
	}
}