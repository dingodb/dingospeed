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
	result := []Descriptor{}
	err := filepath.WalkDir(filepath.Join(root, "api"), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			return nil
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
	all, err := List(root)
	if err != nil {
		return err
	}
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
				return nil
			}
			return fmt.Errorf("repository identity or source conflict")
		}
		if strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
			return fmt.Errorf("repository ancestor/descendant conflict")
		}
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
