// Copyright 2026 DataCanvas Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package repository

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func hfKey(repo string) RepoKey {
	return RepoKey{Namespace: HuggingFace, RepoType: "models", Repo: repo}
}

// writeCachedRepo materialises a cached upstream repository, including the
// paths-info subtree that makes a naive walk of the type directory expensive.
func writeCachedRepo(t *testing.T, root, repo string) {
	t.Helper()
	d := Remote(hfKey(repo))
	api := d.APIRoot(root)
	if err := os.MkdirAll(api, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(api, Marker), b, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		p := filepath.Join(api, "paths-info", "main", "weights", strconv.Itoa(i))
		if err = os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(p, "paths-info_post.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// countDirReads reports how many directories a conflict lookup opens, which is
// the work Register performs while holding the global registration lock.
func countDirReads(t *testing.T, root string, d Descriptor) int {
	t.Helper()
	opened := 0
	current := []string{filepath.Join(root, "api", d.RepoType)}
	parts := strings.Split(d.Repo, "/")
	for i, part := range parts {
		next := []string{}
		for _, dir := range current {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			opened++
			for _, e := range entries {
				if !e.IsDir() || !strings.EqualFold(e.Name(), part) {
					continue
				}
				if i < len(parts)-1 {
					next = append(next, filepath.Join(dir, e.Name()))
				}
			}
		}
		current = next
	}
	return opened
}

// TestHFRegistrationCostIsIndependentOfCacheSize pins the property Register's
// own comment promises: "registration cost stays independent of how many
// unrelated repositories a node accumulates".
//
// Before listAlongRepoPath, conflictScanRoots returned api/<repoType> for the
// HuggingFace namespace, so a first-time registration walked every cached
// upstream repository - including each one's paths-info tree - while holding
// the process-wide registration lock. On a mirror node with a large cache that
// turned every uncached download into a multi-minute stall.
func TestHFRegistrationCostIsIndependentOfCacheSize(t *testing.T) {
	build := func(unrelated int) (string, Descriptor) {
		root := t.TempDir()
		for i := 0; i < unrelated; i++ {
			writeCachedRepo(t, root, "unrelated-org-"+strconv.Itoa(i)+"/model-"+strconv.Itoa(i))
		}
		// A sibling must NOT be reported (it cannot overlap), while a
		// repository nested below the candidate must still be found.
		writeCachedRepo(t, root, "candidate-org/sibling")
		writeCachedRepo(t, root, "candidate-org/target/variant")
		return root, Remote(hfKey("candidate-org/target"))
	}

	smallRoot, cand := build(2)
	largeRoot, _ := build(200)

	smallDirs := countDirReads(t, smallRoot, cand)
	largeDirs := countDirReads(t, largeRoot, cand)

	smallFound, err := listConflicts(smallRoot, cand)
	if err != nil {
		t.Fatal(err)
	}
	largeFound, err := listConflicts(largeRoot, cand)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("2 unrelated cached repos:   %d dirs read, %d conflict candidates", smallDirs, len(smallFound))
	t.Logf("200 unrelated cached repos: %d dirs read, %d conflict candidates", largeDirs, len(largeFound))

	if largeDirs != smallDirs {
		t.Fatalf("registration cost grew with the cache: %d dirs at 2 repos vs %d dirs at 200", smallDirs, largeDirs)
	}
	if len(smallFound) != len(largeFound) {
		t.Fatalf("conflict set changed with unrelated repositories: %d vs %d", len(smallFound), len(largeFound))
	}
	if len(smallFound) == 0 {
		t.Fatal("the repository nested below the candidate was not found")
	}
}

// TestHFRegistrationStillDetectsConflicts guards the semantics the cheaper
// lookup must preserve: ancestors, descendants and case-folded twins all still
// clash, and unrelated cache entries are never needed to notice them.
func TestHFRegistrationStillDetectsConflicts(t *testing.T) {
	for _, tc := range []struct{ name, existing, candidate string }{
		{"ancestor", "org/model", "org/model/variant"},
		{"descendant", "org/model/variant", "org/model"},
		{"case-folded twin", "Org/Model", "org/model"},
		{"parent case", "Org/other", "org/model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeCachedRepo(t, root, tc.existing)
			for i := 0; i < 20; i++ {
				writeCachedRepo(t, root, "noise-"+strconv.Itoa(i)+"/m")
			}
			cand := Remote(hfKey(tc.candidate))
			all, err := listConflicts(root, cand)
			if err != nil {
				t.Fatal(err)
			}
			if err = registrationConflicts(root, cand, all); err == nil {
				t.Fatalf("existing %q vs candidate %q: expected a conflict, got none (%d candidates)", tc.existing, tc.candidate, len(all))
			}
		})
	}
}

// TestHFRegistrationAllowsUnrelated keeps the lookup from over-reporting: a
// repository that merely shares a prefix string must not block registration.
func TestHFRegistrationAllowsUnrelated(t *testing.T) {
	root := t.TempDir()
	writeCachedRepo(t, root, "org/model-large")
	writeCachedRepo(t, root, "other-org/model")
	cand := Remote(hfKey("org/model"))
	all, err := listConflicts(root, cand)
	if err != nil {
		t.Fatal(err)
	}
	if err = registrationConflicts(root, cand, all); err != nil {
		t.Fatalf("unrelated repositories must not conflict, got: %v", err)
	}
}
