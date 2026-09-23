// Package repository owns platform repository identity, layout and registration.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gofrs/flock"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const Marker = "repository.json"
const HuggingFace = "huggingface"
const ModelScope = "modelscope"
const Local = "dingo-local"
const Company = "datacanvas"

type RepoKey struct {
	Namespace string `json:"namespace"`
	RepoType  string `json:"repoType"`
	Repo      string `json:"repo"`
}

func (k RepoKey) ID() string { return k.Namespace + "/" + k.Repo }
func (k RepoKey) Validate() error {
	if k.RepoType != "models" && k.RepoType != "datasets" && k.RepoType != "spaces" {
		return fmt.Errorf("invalid repoType")
	}
	if err := Segment(k.Namespace); err != nil {
		return fmt.Errorf("namespace: %w", err)
	}
	if err := Relative(k.Repo); err != nil {
		return fmt.Errorf("repo: %w", err)
	}
	return nil
}

// ParseID is only for values already known to be full platform repository IDs.
func ParseID(repoType, id string) (RepoKey, error) {
	p := strings.SplitN(id, "/", 2)
	if len(p) != 2 {
		return RepoKey{}, fmt.Errorf("repository ID must include namespace")
	}
	k := RepoKey{p[0], repoType, p[1]}
	return k, k.Validate()
}

func Segment(s string) error {
	if s == "" || s == "." || s == ".." || !utf8.ValidString(s) || len(s) > 255 || strings.ContainsAny(s, `/\:*?"<>|`) || strings.TrimSpace(s) != s || strings.HasSuffix(s, ".") {
		return fmt.Errorf("unsafe path segment %q", s)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("control character in path")
		}
	}
	base := strings.ToLower(strings.SplitN(s, ".", 2)[0])
	if base == "con" || base == "prn" || base == "aux" || base == "nul" || (len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9') {
		return fmt.Errorf("reserved platform path name")
	}
	return nil
}
func Relative(s string) error {
	if len(s) > 1024 {
		return fmt.Errorf("relative path exceeds 1024 bytes")
	}
	for _, p := range strings.Split(s, "/") {
		if err := Segment(p); err != nil {
			return err
		}
	}
	return nil
}
func (k RepoKey) FilesRoot(root string) string {
	if k.Namespace == ModelScope {
		return filepath.Join(root, "modelscope", k.RepoType, filepath.FromSlash(k.Repo))
	}
	return filepath.Join(root, "files", k.RepoType, k.storagePath())
}
func (k RepoKey) APIRoot(root string) string {
	if k.Namespace == ModelScope {
		return filepath.Join(root, "api", k.RepoType, ModelScope, filepath.FromSlash(k.Repo))
	}
	return filepath.Join(root, "api", k.RepoType, k.storagePath())
}

// Provider identity is virtual: HF keeps its original upstream directory and
// hosted namespaces live exclusively below the local upload root.
func (k RepoKey) storagePath() string {
	if k.Namespace == HuggingFace {
		return filepath.FromSlash(k.Repo)
	}
	// Existing pre-namespace uploads remain addressable until explicitly migrated.
	if k.Namespace == Local {
		return filepath.Join(Local, filepath.FromSlash(k.Repo))
	}
	return filepath.Join(Local, k.Namespace, filepath.FromSlash(k.Repo))
}
func (k RepoKey) Blob(root, sha string) string { return filepath.Join(k.FilesRoot(root), "blobs", sha) }
func (k RepoKey) Resolve(root, commit, path string) string {
	return filepath.Join(k.FilesRoot(root), "resolve", commit, filepath.FromSlash(path))
}
func EscapeURLPath(value string) string {
	parts := strings.Split(value, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func (k RepoKey) PathsInfo(root, commit, path string) string {
	return filepath.Join(k.APIRoot(root), "paths-info", commit, filepath.FromSlash(path))
}
func (k RepoKey) Revision(root, revision string) string {
	return filepath.Join(k.APIRoot(root), "revision", revision)
}

type Descriptor struct {
	RepoKey
	Version      int    `json:"version"`
	Source       string `json:"source"`
	Persistent   bool   `json:"persistent"`
	Provider     string `json:"provider,omitempty"`
	UpstreamRepo string `json:"upstreamRepo,omitempty"`
	Format       string `json:"format"`
}

func Hosted(k RepoKey) Descriptor {
	return Descriptor{RepoKey: k, Version: 1, Source: "hosted", Persistent: true, Format: "dingcache"}
}
func Remote(k RepoKey) Descriptor {
	return Descriptor{RepoKey: k, Version: 1, Source: "remote", Provider: k.Namespace, UpstreamRepo: k.Repo, Format: "dingcache"}
}
func (d Descriptor) Validate() error {
	if err := d.RepoKey.Validate(); err != nil {
		return err
	}
	if d.Version != 1 {
		return fmt.Errorf("unsupported repository descriptor version")
	}
	switch d.Source {
	case "hosted":
		if !d.Persistent || d.Provider != "" || d.UpstreamRepo != "" || d.Format != "dingcache" || strings.EqualFold(d.Namespace, HuggingFace) || strings.EqualFold(d.Namespace, ModelScope) {
			return fmt.Errorf("invalid hosted source policy")
		}
	case "remote":
		if d.Persistent || (d.Provider != HuggingFace && d.Provider != ModelScope) || d.Namespace != d.Provider || d.UpstreamRepo != d.Repo {
			return fmt.Errorf("invalid remote source policy")
		}
		if d.Format != "dingcache" {
			return fmt.Errorf("invalid remote format")
		}
	default:
		return fmt.Errorf("unknown repository source")
	}
	return nil
}
func Read(root string, k RepoKey) (Descriptor, error) {
	if err := k.Validate(); err != nil {
		return Descriptor{}, err
	}
	if err := SafePath(root, filepath.Join(k.APIRoot(root), Marker)); err != nil {
		return Descriptor{}, err
	}
	b, err := os.ReadFile(filepath.Join(k.APIRoot(root), Marker))
	if err != nil {
		return Descriptor{}, err
	}
	var d Descriptor
	if err = json.Unmarshal(b, &d); err != nil {
		return d, err
	}
	if d.RepoKey != k {
		return d, fmt.Errorf("repository marker identity mismatch")
	}
	return d, d.Validate()
}

func List(root string) ([]Descriptor, error) {
	return listIn(filepath.Join(root, "api"), root)
}

// ListHosted enumerates only hosted repositories, which live exclusively below
// api/<repoType>/dingo-local. Remote upstream caches share the same api/ tree and
// outnumber uploads by orders of magnitude, so scanning all of api/ to then drop
// every non-hosted descriptor costs a full-tree walk per call on cheap triggers
// such as the inventory ticker. Walking the hosted roots instead keeps the scan
// proportional to the uploads we actually report.
//
// This is an enumeration-scope change only: a descriptor the caller could not
// reach before is unreachable now, because hosted keys always resolve below the
// roots walked here.
func ListHosted(root string) ([]Descriptor, error) {
	result := []Descriptor{}
	for _, repoType := range []string{"models", "datasets", "spaces"} {
		found, err := listIn(filepath.Join(root, "api", repoType, Local), root)
		if err != nil {
			return result, err
		}
		result = append(result, found...)
	}
	return result, nil
}

func isDataSubtree(name string) bool {
	switch name {
	case "paths-info", "revision", "recycle", "blobs", "resolve":
		return true
	}
	return false
}

// isRepositoryDataSubtree reports whether a directory walk must stop descending
// here because the directory is platform-owned repository data rather than a
// candidate repository root.
//
// A data subtree is identified by the marker above it, never by its own name:
// repository names are user input, so "team/resolve/model" and even
// "paths-info/repo" are legitimate repositories whose own paths contain a
// data-subtree word. Inside a recognised repository, though, these names are
// reserved platform layout, so nothing below one can be a repository root.
func isRepositoryDataSubtree(scanRoot, p, name string) bool {
	if p == scanRoot {
		return false
	}
	if !isDataSubtree(name) {
		return false
	}
	// Either this directory itself is data (a marker above it), or it is nested
	// inside a data subtree that the walk already declined to enter.
	return hasRepositoryAncestor(scanRoot, p) || hasDataSubtreeAncestor(scanRoot, p)
}

// hasDataSubtreeAncestor reports whether any strict ancestor of p below the scan
// root is a data subtree.
func hasDataSubtreeAncestor(scanRoot, p string) bool {
	scanRoot = filepath.Clean(scanRoot)
	for dir := filepath.Dir(filepath.Clean(p)); dir != scanRoot; dir = filepath.Dir(dir) {
		if len(dir) <= len(scanRoot) {
			return false
		}
		if isDataSubtree(filepath.Base(dir)) {
			return true
		}
	}
	return false
}

// hasRepositoryAncestor reports whether any strict ancestor of p below the scan
// root carries a repository marker. Walking a handful of parents through Lstat
// is far cheaper than descending a data subtree that can hold one directory per
// stored file.
func hasRepositoryAncestor(scanRoot, p string) bool {
	scanRoot = filepath.Clean(scanRoot)
	for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
		if len(dir) < len(scanRoot) || (dir != scanRoot && !strings.HasPrefix(dir, scanRoot+string(filepath.Separator))) {
			return false
		}
		if _, err := os.Lstat(filepath.Join(dir, Marker)); err == nil {
			return true
		}
		if dir == scanRoot {
			return false
		}
	}
}

func listIn(scanRoot, root string) ([]Descriptor, error) {
	result := []Descriptor{}
	err := filepath.WalkDir(scanRoot, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			return nil
		}
		// Data subtrees under a repository are not repositories themselves, and
		// descending them turns discovery into a walk of every stored file.
		if isRepositoryDataSubtree(scanRoot, p, e.Name()) {
			return filepath.SkipDir
		}
		marker := filepath.Join(p, Marker)
		info, statErr := os.Lstat(marker)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(marker)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = SafePath(root, marker); err != nil {
			return err
		}
		var d Descriptor
		if err = json.Unmarshal(b, &d); err != nil {
			return err
		}
		if err = d.Validate(); err != nil {
			return err
		}
		if filepath.Clean(filepath.Join(d.APIRoot(root), Marker)) != filepath.Clean(marker) {
			return fmt.Errorf("repository marker path mismatch: %s", marker)
		}
		result = append(result, d)
		return filepath.SkipDir
	})
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	return result, err
}

var registrationMu sync.Mutex

// listConflicts returns exactly the registered repositories that the conflict
// checks in Register can act on, without scanning the whole registry.
//
// Every check in registrationConflicts compares against the candidate's namespace
// or one of its own path prefixes, so a repository can only conflict when its
// storage path can overlap the candidate's. Hosted namespaces occupy disjoint
// directories below dingo-local, so repositories under other namespaces can never
// overlap - with one exception: a bare local-namespace repository is stored at
// dingo-local/<repo>, so its path can be a prefix of any other namespace's
// directory and must be considered for every hosted candidate.
func listConflicts(root string, d Descriptor) ([]Descriptor, error) {
	result := []Descriptor{}
	seen := map[RepoKey]struct{}{}
	scan := func(scanRoot string) error {
		found, err := listIn(scanRoot, root)
		if err != nil {
			return err
		}
		for _, old := range found {
			if _, ok := seen[old.RepoKey]; ok {
				continue
			}
			seen[old.RepoKey] = struct{}{}
			result = append(result, old)
		}
		return nil
	}
	for _, repoType := range []string{"models", "datasets", "spaces"} {
		// Namespace case conflicts cross repository types, so the candidate's
		// namespace directory is visited under every type as well.
		for _, scanRoot := range conflictScanRoots(root, repoType, d.Namespace) {
			if err := scan(scanRoot); err != nil {
				return result, err
			}
		}
		if d.Namespace == HuggingFace {
			// Only repositories lying along the candidate's own path can overlap
			// it, so read those directory levels instead of the whole type tree.
			found, err := listAlongRepoPath(root, repoType, d.Repo)
			if err != nil {
				return result, err
			}
			for _, old := range found {
				if _, ok := seen[old.RepoKey]; ok {
					continue
				}
				seen[old.RepoKey] = struct{}{}
				result = append(result, old)
			}
		}
		if d.Namespace != Local && d.Namespace != HuggingFace && d.Namespace != ModelScope {
			bare, err := listBareLocalRepositories(root, repoType, d.Namespace)
			if err != nil {
				return result, err
			}
			for _, old := range bare {
				if _, ok := seen[old.RepoKey]; ok {
					continue
				}
				seen[old.RepoKey] = struct{}{}
				result = append(result, old)
			}
		}
	}
	return result, nil
}

// conflictScanRoots lists the directories that can hold a repository whose stored
// path can overlap a candidate of the given repository type and namespace. The
// paths mirror APIRoot/storagePath; a path that does not exist yields nothing.
// listAlongRepoPath finds the repositories that can physically overlap a
// HuggingFace candidate stored at api/<repoType>/<repo>.
//
// registrationConflicts only rejects a pair when one storage path is a
// case-insensitive prefix of the other, when one repo path is an ancestor or
// descendant of the other, or when a shared path component differs only by
// case. Every one of those requires the existing repository to sit *along the
// candidate's own path*, so the levels of that path are the only ones worth
// reading. Sibling subtrees cannot overlap and are never opened, which keeps
// registration cost proportional to the path depth and the fan-out of the
// directories on it rather than to the number of cached repositories.
//
// Case variants are followed because a case-folded twin is a separate
// directory on Linux but still a conflict under the registry contract; this
// mirrors how listBareLocalRepositories handles namespace aliases.
func listAlongRepoPath(root, repoType, repo string) ([]Descriptor, error) {
	result := []Descriptor{}
	parts := strings.Split(repo, "/")
	// Directories to inspect at the current level, starting at the type root.
	current := []string{filepath.Join(root, "api", repoType)}
	for i, part := range parts {
		next := []string{}
		for _, dir := range current {
			entries, err := os.ReadDir(dir)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return result, err
			}
			for _, entry := range entries {
				if !entry.IsDir() || !strings.EqualFold(entry.Name(), part) {
					continue
				}
				child := filepath.Join(dir, entry.Name())
				if entry.Name() != part {
					// A component that matches only when case is folded is a
					// separate directory on Linux, but every repository below it
					// clashes with the candidate on "parent directory case".
					// Collect that subtree whole, the way
					// listBareLocalRepositories treats namespace aliases. Such
					// directories are the rare exception, so this does not
					// reintroduce a cache-wide walk.
					variants, err := listIn(child, root)
					if err != nil {
						return result, err
					}
					result = append(result, variants...)
					if d, ok := readDescriptorAt(root, child); ok {
						result = append(result, d)
					}
					continue
				}
				// A marker on an ancestor of the candidate is an
				// ancestor/descendant conflict; on the final component it is the
				// candidate itself.
				if d, ok := readDescriptorAt(root, child); ok {
					result = append(result, d)
				}
				if i == len(parts)-1 {
					// Repositories nested below the candidate also conflict.
					// listIn prunes data subtrees, so this stays bounded by the
					// candidate's own directory.
					nested, err := listIn(child, root)
					if err != nil {
						return result, err
					}
					result = append(result, nested...)
					continue
				}
				next = append(next, child)
			}
		}
		current = next
		if len(current) == 0 {
			break
		}
	}
	return result, nil
}

// readDescriptorAt reads the repository marker in dir, if there is a valid one.
func readDescriptorAt(root, dir string) (Descriptor, bool) {
	marker := filepath.Join(dir, Marker)
	b, err := os.ReadFile(marker)
	if err != nil {
		return Descriptor{}, false
	}
	if err = SafePath(root, marker); err != nil {
		return Descriptor{}, false
	}
	var d Descriptor
	if err = json.Unmarshal(b, &d); err != nil {
		return Descriptor{}, false
	}
	if err = d.Validate(); err != nil {
		return Descriptor{}, false
	}
	if filepath.Clean(filepath.Join(d.APIRoot(root), Marker)) != filepath.Clean(marker) {
		return Descriptor{}, false
	}
	return d, true
}

func conflictScanRoots(root, repoType, namespace string) []string {
	switch namespace {
	case ModelScope:
		// modelscope/<repo>: disjoint from everything else.
		return []string{filepath.Join(root, "api", repoType, ModelScope)}
	case HuggingFace:
		// <repo> sits directly below the type directory, so the only tree that
		// could hold an overlap is the type directory itself - which on a mirror
		// node holds every cached upstream repository. Walking it would make
		// registration cost grow with the cache, so HuggingFace candidates are
		// resolved by listAlongRepoPath instead and contribute no scan root here.
		return nil
	case Local:
		// dingo-local/<repo>: only the shared upload root can hold overlaps.
		return []string{filepath.Join(root, "api", repoType, Local)}
	default:
		// dingo-local/<namespace>/<repo>. Siblings in the same namespace can
		// overlap the candidate. A bare local repository is stored at
		// dingo-local/<repo>, so it can also be a prefix of dingo-local/<namespace>;
		// only namespaces without a marker of their own are those bare repositories,
		// which is why the second root is read shallowly rather than walked.
		return []string{filepath.Join(root, "api", repoType, Local, namespace)}
	}
}

// listBareLocalRepositories returns repositories stored directly below the local
// upload root, i.e. those using the local namespace. They are the only hosted
// repositories outside the candidate's namespace whose path can overlap it.
func listBareLocalRepositories(root, repoType, namespace string) ([]Descriptor, error) {
	result := []Descriptor{}
	dir := filepath.Join(root, "api", repoType, Local)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return result, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Case-folded namespace aliases are physically separate on Linux but
		// still forbidden by the registry contract. Inspect that matching tree
		// only, not every unrelated namespace.
		if entry.Name() != namespace && strings.EqualFold(entry.Name(), namespace) {
			aliases, err := listIn(filepath.Join(dir, entry.Name()), root)
			if err != nil {
				return nil, err
			}
			result = append(result, aliases...)
		}
		marker := filepath.Join(dir, entry.Name(), Marker)
		b, err := os.ReadFile(marker)
		if err != nil {
			continue // a namespace directory, or a bare tree without a marker
		}
		var d Descriptor
		if err = json.Unmarshal(b, &d); err != nil || d.Validate() != nil {
			continue
		}
		if d.Namespace == Local {
			result = append(result, d)
		}
	}
	return result, nil
}

// registrationConflicts applies the same conflict rules Register enforces, given
// a candidate and the repositories visible to it. It is shared with the
// whole-registry reference used in tests so both paths cannot drift.
func registrationConflicts(root string, d Descriptor, all []Descriptor) error {
	for _, old := range all {
		if old.RepoKey != d.RepoKey {
			for _, pair := range [][2]string{{old.FilesRoot(root), d.FilesRoot(root)}, {old.APIRoot(root), d.APIRoot(root)}} {
				a, b := strings.ToLower(filepath.Clean(pair[0])), strings.ToLower(filepath.Clean(pair[1]))
				if a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator)) {
					return fmt.Errorf("repository physical path conflict")
				}
			}
		}
		if strings.EqualFold(old.Namespace, d.Namespace) && old.Namespace != d.Namespace {
			return fmt.Errorf("namespace case conflict")
		}
		if old.RepoType != d.RepoType || old.Namespace != d.Namespace {
			continue
		}
		oldParts, newParts := strings.Split(old.Repo, "/"), strings.Split(d.Repo, "/")
		for i := 0; i < len(oldParts) && i < len(newParts); i++ {
			if !strings.EqualFold(oldParts[i], newParts[i]) {
				break
			}
			if oldParts[i] != newParts[i] {
				return fmt.Errorf("repository parent directory case conflict")
			}
		}
		a, b := strings.ToLower(old.Repo), strings.ToLower(d.Repo)
		if a == b {
			if old.RepoKey == d.RepoKey && old == d {
				continue
			}
			return fmt.Errorf("repository identity or source conflict")
		}
		if strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
			return fmt.Errorf("repository ancestor/descendant conflict")
		}
	}
	return nil
}

// Register serializes case and ancestor checks with marker creation. The lock
// file also rejects concurrent registration by another process sharing root.
func Register(root string, d Descriptor) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if err := SafePath(root, filepath.Join(d.APIRoot(root), Marker)); err != nil {
		return err
	}
	if err := SafePath(root, filepath.Join(d.FilesRoot(root), ".check")); err != nil {
		return err
	}
	registrationMu.Lock()
	defer registrationMu.Unlock()
	if err := os.MkdirAll(filepath.Join(root, "api"), 0755); err != nil {
		return err
	}
	lockPath := filepath.Join(root, "api", ".repository-registration.lock")
	fileLock := flock.New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	locked, err := fileLock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("repository registration busy")
	}
	defer fileLock.Unlock()
	// An existing descriptor is immutable through Register. Validate it directly
	// under both registration locks instead of enumerating unrelated repositories.
	if old, readErr := Read(root, d.RepoKey); readErr == nil {
		if old == d {
			return nil
		}
		return fmt.Errorf("repository identity or source conflict")
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	// Conflict detection only ever compares against repositories that share this
	// repository type and namespace, plus this descriptor's own ancestors. Those
	// are reachable directly from the storage root, so registration cost stays
	// independent of how many unrelated repositories a node accumulates.
	all, err := listConflicts(root, d)
	if err != nil {
		return err
	}
	for _, old := range all {
		if old.RepoKey == d.RepoKey && old == d {
			return nil // already registered; nothing to do
		}
	}
	if err = registrationConflicts(root, d, all); err != nil {
		return err
	}
	dest := filepath.Join(d.APIRoot(root), Marker)
	if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".repository-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dest)
}

// Locate finds the descriptor for a file by registered roots, never by guessing
// how many repository path segments precede blobs/resolve/provider-cache.
func Locate(root, file string) (Descriptor, string, error) {
	all, err := List(root)
	if err != nil {
		return Descriptor{}, "", err
	}
	return LocateRegistered(root, file, all)
}

// LocateRegistered reuses one validated registry snapshot for batch traversal.
func LocateRegistered(root, file string, all []Descriptor) (Descriptor, string, error) {
	for _, d := range all {
		rel, err := filepath.Rel(d.FilesRoot(root), file)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return d, filepath.ToSlash(rel), nil
		}
	}
	return Descriptor{}, "", os.ErrNotExist
}

func SafePath(root, target string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path escapes repository root")
	}
	for current := targetAbs; current != rootAbs; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in repository path: %s", current)
		}
	}
	return nil
}
