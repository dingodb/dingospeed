// Package hfprojection reads existing HF cache facts without registration,
// network access, database access, or opening any file for writing.
package hfprojection

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dingospeed/pkg/repository"
)

type Revision struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}
type Repo struct {
	repository.RepoKey
	Revisions []Revision `json:"revisions"`
	Access    string     `json:"access"`
	Error     string     `json:"error,omitempty"`
}
type Catalog struct {
	Namespace string `json:"namespace"`
	RepoType  string `json:"repoType"`
	Repos     []Repo `json:"repos"`
}
type File struct {
	Path        string `json:"path"`
	OID         string `json:"oid"`
	Size        int64  `json:"size"`
	CachedBytes int64  `json:"cachedBytes"`
	CacheStatus string `json:"cacheStatus"`
	localPath   string
}
type Manifest struct {
	repository.RepoKey
	Revision          string `json:"revision"`
	Commit            string `json:"commit"`
	MetadataAvailable bool   `json:"metadataAvailable"`
	ManifestComplete  bool   `json:"manifestComplete"`
	Access            string `json:"access"`
	Files             []File `json:"files"`
}

// Reader has only a filesystem root. No downloader or database can be injected.
type Reader struct {
	Root      string
	Namespace string
}

func (r Reader) namespace() string {
	if r.Namespace != "" {
		return r.Namespace
	}
	return repository.HuggingFace
}

func (r Reader) key(typ, repo string) (repository.RepoKey, error) {
	k := repository.RepoKey{Namespace: r.namespace(), RepoType: typ, Repo: repo}
	if err := k.Validate(); err != nil {
		return k, err
	}
	p := strings.Split(repo, "/")
	if len(p) > 2 || (r.namespace() == repository.HuggingFace && (strings.EqualFold(p[0], repository.Local) || strings.EqualFold(p[0], repository.ModelScope))) {
		return k, fmt.Errorf("not an upstream HF repository")
	}
	return k, nil
}
func (r Reader) safe(p string) error { return repository.SafePath(r.Root, p) }
func (r Reader) dirs(p string) ([]os.DirEntry, error) {
	if err := r.safe(p); err != nil {
		return nil, err
	}
	v, err := os.ReadDir(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return v, err
}

// Catalog discovers legacy layouts, including repositories without a marker.
// It never traverses hosted/modelscope roots or guesses identity from blobs.
func (r Reader) Catalog(typ string) (Catalog, error) {
	out := Catalog{Namespace: r.namespace(), RepoType: typ, Repos: []Repo{}}
	if _, err := r.key(typ, "placeholder"); err != nil {
		return out, err
	}
	if err := r.safe(r.Root); err != nil {
		return out, err
	}
	if _, err := os.Stat(r.Root); err != nil {
		return out, err
	}
	names := map[string]bool{}
	if r.namespace() == repository.ModelScope {
		descriptors, err := repository.List(r.Root)
		if err != nil {
			return out, err
		}
		for _, d := range descriptors {
			if d.Namespace == r.namespace() && d.RepoType == typ {
				names[d.Repo] = true
			}
		}
	} else {
		for _, area := range []string{"api", "files"} {
			base := filepath.Join(r.Root, area, typ)
			owners, err := r.dirs(base)
			if err != nil {
				return out, err
			}
			for _, owner := range owners {
				if !owner.IsDir() || strings.EqualFold(owner.Name(), repository.Local) || strings.EqualFold(owner.Name(), repository.ModelScope) {
					continue
				}
				children, err := r.dirs(filepath.Join(base, owner.Name()))
				if err != nil {
					return out, err
				}
				nested := map[string]bool{}
				for _, child := range children {
					if !child.IsDir() {
						continue
					}
					entries, err := r.dirs(filepath.Join(base, owner.Name(), child.Name()))
					if err != nil {
						return out, err
					}
					for _, e := range entries {
						if e.IsDir() && cacheArea(e.Name(), area) {
							names[owner.Name()+"/"+child.Name()] = true
							nested[child.Name()] = true
						}
					}
				}
				// A real two-segment repository can itself be named revision,
				// paths-info or resolve. Prefer its own layout over treating its
				// name as the enclosing single-segment repository's cache area.
				for _, child := range children {
					if child.IsDir() && cacheArea(child.Name(), area) && !nested[child.Name()] {
						names[owner.Name()] = true
					}
				}
			}
		}
	}
	for name := range names {
		k, err := r.key(typ, name)
		if err != nil {
			return out, err
		}
		repo := Repo{RepoKey: k, Revisions: []Revision{}, Access: "unknown"}
		repo.Revisions, err = r.revisions(k)
		if err == nil {
			repo.Access, err = r.access(k, repo.Revisions)
		}
		if err != nil {
			repo.Error = "cached repository metadata is unreadable or invalid"
			repo.Revisions = []Revision{}
		}
		out.Repos = append(out.Repos, repo)
	}
	sort.Slice(out.Repos, func(i, j int) bool { return out.Repos[i].Repo < out.Repos[j].Repo })
	return out, nil
}
func cacheArea(name, area string) bool {
	return area == "api" && (name == "revision" || name == "paths-info") || area == "files" && name == "resolve"
}

type metadata struct {
	SHA      string          `json:"sha"`
	Private  *bool           `json:"private"`
	Gated    json.RawMessage `json:"gated"`
	Siblings []struct {
		Path string `json:"rfilename"`
		Size *int64 `json:"size"`
		OID  string `json:"blobId"`
		LFS  *struct {
			OID  string `json:"sha256"`
			Size int64  `json:"size"`
		} `json:"lfs"`
	} `json:"siblings"`
}

func metadataAccess(m metadata) string {
	if m.Private != nil && *m.Private {
		return "restricted"
	}
	gated := strings.TrimSpace(string(m.Gated))
	if gated != "" && gated != "false" && gated != "null" {
		return "restricted"
	}
	if m.Private != nil && !*m.Private && gated == "false" {
		return "public"
	}
	return "unknown"
}

// No cached permission fact is inferred from successful download or a repo
// name. A missing permission field is unknown, not permission to publish it.
func (r Reader) access(k repository.RepoKey, revisions []Revision) (string, error) {
	access := "public"
	// Hidden fixed-commit metadata can still contain a restrictive permission
	// fact. Deduplicating the displayed branch list must not drop that fact.
	seen := map[string]bool{}
	all := append([]Revision(nil), revisions...)
	for _, rev := range all {
		seen[rev.Name] = true
	}
	entries, err := r.dirs(filepath.Join(k.APIRoot(r.Root), "revision"))
	if err != nil {
		return "unknown", err
	}
	for _, entry := range entries {
		if entry.IsDir() && !seen[entry.Name()] {
			all = append(all, Revision{Name: entry.Name()})
		}
	}
	if len(all) == 0 {
		return "unknown", nil
	}
	for _, rev := range all {
		var m metadata
		err := r.decode(filepath.Join(k.Revision(r.Root, rev.Name), "meta_get.json"), &m)
		if errors.Is(err, os.ErrNotExist) {
			access = "unknown"
			continue
		}
		if err != nil {
			return "unknown", err
		}
		switch metadataAccess(m) {
		case "restricted":
			return "restricted", nil
		case "unknown":
			access = "unknown"
		}
	}
	return access, nil
}

func (r Reader) decode(p string, out any) error {
	if err := r.safe(p); err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	// Cache responses are bounded; malformed/truncated data must fail closed.
	b, err := io.ReadAll(io.LimitReader(f, 32*1024*1024+1))
	if err != nil {
		return err
	}
	if len(b) > 32*1024*1024 {
		return fmt.Errorf("cached metadata exceeds projection limit")
	}
	var wrapper struct {
		Status  int    `json:"status_code"`
		Content string `json:"content"`
	}
	if err = json.Unmarshal(b, &wrapper); err != nil {
		return fmt.Errorf("invalid cached metadata: %w", err)
	}
	if wrapper.Status != 200 {
		return fmt.Errorf("cached metadata is not successful (HTTP %d)", wrapper.Status)
	}
	b, err = hex.DecodeString(wrapper.Content)
	if err != nil {
		return fmt.Errorf("invalid cached metadata encoding: %w", err)
	}
	if err = json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("invalid cached response body: %w", err)
	}
	return nil
}
func (r Reader) revisions(k repository.RepoKey) ([]Revision, error) {
	byName := map[string]string{}
	entries, err := r.dirs(filepath.Join(k.APIRoot(r.Root), "revision"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := repository.Segment(e.Name()); err != nil {
			return nil, err
		}
		var meta metadata
		err := r.decode(filepath.Join(k.Revision(r.Root, e.Name()), "meta_get.json"), &meta)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := repository.Segment(meta.SHA); err != nil {
			return nil, fmt.Errorf("invalid cached commit")
		}
		byName[e.Name()] = meta.SHA
	}
	if k.Namespace != repository.ModelScope {
		for _, base := range []string{filepath.Join(k.APIRoot(r.Root), "paths-info"), filepath.Join(k.FilesRoot(r.Root), "resolve")} {
			entries, err := r.dirs(base)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.IsDir() {
					if err := repository.Segment(e.Name()); err != nil {
						return nil, err
					}
					if _, ok := byName[e.Name()]; !ok {
						byName[e.Name()] = e.Name()
					}
				}
			}
		}
	}
	out := []Revision{}
	pointed := map[string]bool{}
	for name, commit := range byName {
		if name != commit {
			pointed[commit] = true
		}
	}
	for name, commit := range byName {
		if name == commit && pointed[commit] {
			continue
		}
		out = append(out, Revision{name, commit})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r Reader) Manifest(typ, repo, revision string) (Manifest, error) {
	k, err := r.key(typ, repo)
	out := Manifest{RepoKey: k, Revision: revision, Files: []File{}, Access: "unknown"}
	if err != nil {
		return out, err
	}
	if err = repository.Segment(revision); err != nil {
		return out, err
	}
	revisions, err := r.revisions(k)
	if err != nil {
		return out, err
	}
	for _, rev := range revisions {
		if rev.Name == revision {
			out.Commit = rev.Commit
			break
		}
	}
	// Fixed commits remain directly readable even when hidden from the branch
	// list to avoid counting the same cached revision twice.
	if out.Commit == "" {
		for _, rev := range revisions {
			if rev.Commit == revision {
				out.Commit = revision
				break
			}
		}
	}
	if out.Commit == "" {
		return out, os.ErrNotExist
	}
	out.Access, err = r.access(k, revisions)
	if err != nil {
		return out, err
	}
	files := map[string]File{}
	var meta metadata
	err = r.decode(filepath.Join(k.Revision(r.Root, revision), "meta_get.json"), &meta)
	if errors.Is(err, os.ErrNotExist) && revision != out.Commit {
		err = r.decode(filepath.Join(k.Revision(r.Root, out.Commit), "meta_get.json"), &meta)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	if err == nil {
		if meta.SHA != out.Commit {
			return out, fmt.Errorf("cached revision changed; refresh projection")
		}
		out.MetadataAvailable = true
		out.ManifestComplete = meta.Siblings != nil
		for _, s := range meta.Siblings {
			if err := repository.Relative(s.Path); err != nil {
				return out, err
			}
			v := File{Path: s.Path, OID: s.OID, Size: -1, CacheStatus: "unknown"}
			if s.Size != nil {
				v.Size = *s.Size
			}
			if s.LFS != nil {
				v.OID = s.LFS.OID
				v.Size = s.LFS.Size
			}
			files[s.Path] = v
		}
	}
	infoRoot := filepath.Join(k.APIRoot(r.Root), "paths-info", out.Commit)
	err = r.walk(infoRoot, func(p string, e fs.DirEntry) error {
		if e.Name() != "paths-info_post.json" {
			return nil
		}
		rel, _ := filepath.Rel(infoRoot, filepath.Dir(p))
		path := filepath.ToSlash(rel)
		if err := repository.Relative(path); err != nil {
			return err
		}
		var items []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
			LFS  struct {
				OID string `json:"oid"`
			} `json:"lfs"`
		}
		if err := r.decode(p, &items); err != nil {
			return err
		}
		if len(items) != 1 {
			return fmt.Errorf("invalid cached file metadata")
		}
		v := files[path]
		v.Path = path
		v.Size = items[0].Size
		v.OID = items[0].OID
		if items[0].LFS.OID != "" {
			v.OID = items[0].LFS.OID
		}
		files[path] = v
		return nil
	})
	if err != nil {
		return out, err
	}
	resolveRoot := filepath.Join(k.FilesRoot(r.Root), "resolve", out.Commit)
	err = r.walk(resolveRoot, func(p string, e fs.DirEntry) error {
		rel, _ := filepath.Rel(resolveRoot, p)
		path := filepath.ToSlash(rel)
		if err := repository.Relative(path); err != nil {
			return err
		}
		v, ok := files[path]
		if !ok {
			v = File{Path: path, Size: -1}
		}
		v.localPath = p
		files[path] = v
		return nil
	})
	if err != nil {
		return out, err
	}
	for _, v := range files {
		if v.OID != "" {
			if err := repository.Segment(v.OID); err != nil {
				return out, fmt.Errorf("invalid cached object identity")
			}
			v.localPath = k.Blob(r.Root, v.OID)
		}
		// Catalogue manifests describe the cached upstream snapshot. Payload
		// completeness belongs to transfer/file-serving paths and is deliberately
		// not inspected while listing repositories or files.
		v.CacheStatus = "unknown"
		out.Files = append(out.Files, v)
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out, nil
}

func (r Reader) walk(root string, visit func(string, fs.DirEntry) error) error {
	if err := r.safe(root); err != nil {
		return err
	}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		return visit(p, e)
	})
	if errors.Is(err, os.ErrNotExist) {
		if _, rootErr := os.Stat(root); errors.Is(rootErr, os.ErrNotExist) {
			return nil
		}
	}
	return err
}

// Only resolve symlinks into this repository's blob directory. Never follow
// arbitrary links out of the cache or into a different repository.
func (r Reader) contentPath(k repository.RepoKey, p string) (string, error) {
	if err := r.safe(filepath.Dir(p)); err != nil {
		return "", err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", err
		}
		abs, _ := filepath.Abs(filepath.Join(k.FilesRoot(r.Root), "blobs"))
		target, _ = filepath.Abs(target)
		if filepath.Dir(target) != abs {
			return "", fmt.Errorf("cached resolve link leaves repository blobs")
		}
		p = target
	}
	if err := r.safe(p); err != nil {
		return "", err
	}
	return p, nil
}

// inspect validates a bounded OLAH v8 header and actual bytes for every cached
// block. Corrupt OLAH is unknown, never treated as a complete plain file.
func inspect(f *os.File, expected int64) (status string, size, cached, offset int64, err error) {
	info, err := f.Stat()
	if err != nil {
		return "unknown", 0, 0, 0, err
	}
	if !info.Mode().IsRegular() {
		return "unknown", 0, 0, 0, fmt.Errorf("cache content is not a regular file")
	}
	var raw [36]byte
	n, readErr := f.ReadAt(raw[:], 0)
	if n < 4 || string(raw[:4]) != "OLAH" {
		if expected < 0 || expected != info.Size() {
			return "unknown", info.Size(), 0, 0, nil
		}
		return "complete", info.Size(), info.Size(), 0, nil
	}
	if readErr != nil {
		return "unknown", expected, 0, 0, nil
	}
	version := binary.LittleEndian.Uint64(raw[4:12])
	block := binary.LittleEndian.Uint64(raw[12:20])
	length := binary.LittleEndian.Uint64(raw[20:28])
	bits := binary.LittleEndian.Uint64(raw[28:36])
	if version != 8 || block == 0 || block > math.MaxInt64 || length > math.MaxInt64 || bits > 8*1024*1024 || bits == 0 {
		return "unknown", expected, 0, 0, nil
	}
	count := length / block
	if length%block != 0 {
		count++
	}
	if count > bits {
		return "unknown", expected, 0, 0, nil
	}
	mask := make([]byte, (bits+7)/8)
	if _, err := f.ReadAt(mask, 36); err != nil {
		return "unknown", expected, 0, 0, nil
	}
	size = int64(length)
	offset = 36 + int64(len(mask))
	if expected >= 0 && expected != size {
		return "unknown", size, 0, offset, nil
	}
	complete := true
	for i := uint64(0); i < count; i++ {
		start := i * block
		end := start + min(block, length-start)
		if mask[i/8]&(1<<(i%8)) != 0 && info.Size()-offset >= int64(end) {
			cached += int64(end - start)
		} else {
			complete = false
		}
	}
	if complete {
		return "complete", size, cached, offset, nil
	}
	return "partial", size, cached, offset, nil
}

var ErrIncomplete = errors.New("file is not completely cached")

// OpenFile returns only an already complete local payload, pinned to an open
// read-only descriptor. Callers must close it; no cache manager is involved.
func (r Reader) OpenFile(typ, repo, revision, path string) (*os.File, *io.SectionReader, error) {
	if err := repository.Relative(path); err != nil {
		return nil, nil, err
	}
	m, err := r.Manifest(typ, repo, revision)
	if err != nil {
		return nil, nil, err
	}
	for _, v := range m.Files {
		if v.Path == path {
			if v.localPath == "" {
				return nil, nil, os.ErrNotExist
			}
			p, err := r.contentPath(m.RepoKey, v.localPath)
			if err != nil {
				return nil, nil, err
			}
			f, err := os.Open(p)
			if err != nil {
				return nil, nil, err
			}
			state, size, _, offset, err := inspect(f, v.Size)
			if err != nil {
				f.Close()
				return nil, nil, err
			}
			if state != "complete" {
				f.Close()
				return nil, nil, ErrIncomplete
			}
			return f, io.NewSectionReader(f, offset, size), nil
		}
	}
	return nil, nil, os.ErrNotExist
}
