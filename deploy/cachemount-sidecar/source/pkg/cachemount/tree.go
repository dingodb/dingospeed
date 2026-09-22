package cachemount

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"dingospeed/pkg/hfprojection"
	"dingospeed/pkg/repository"
)

// Selection is an administrator-selected local model version. This service is
// local filesystem access, not a substitute for the web console's user ACLs.
type Selection struct {
	CacheRoot string `json:"cache_root"`
	Namespace string `json:"namespace"`
	Repo      string `json:"repo"`
	Revision  string `json:"revision"`
}
type TreeFile struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	ContentID string `json:"content_id"`
	Backing   string `json:"backing"`
}
type Version struct {
	Selection
	Commit         string     `json:"commit"`
	SourceFilePath string     `json:"source_file_path"`
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
	Files          []TreeFile `json:"files"`
}

func readJSON(root, p string, out any) error {
	if err := repository.SafePath(root, p); err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 32*1024*1024+1))
	if err != nil {
		return err
	}
	if len(b) > 32*1024*1024 {
		return fmt.Errorf("metadata exceeds 32 MiB")
	}
	return json.Unmarshal(b, out)
}
func commitAt(root, p string) (string, error) {
	var envelope struct {
		Status  int    `json:"status_code"`
		Content string `json:"content"`
	}
	if err := readJSON(root, p, &envelope); err != nil {
		return "", err
	}
	if envelope.Status != 200 {
		return "", fmt.Errorf("metadata status %d", envelope.Status)
	}
	b, err := hex.DecodeString(envelope.Content)
	if err != nil {
		return "", err
	}
	var meta struct {
		SHA string `json:"sha"`
	}
	if err = json.Unmarshal(b, &meta); err != nil {
		return "", err
	}
	return meta.SHA, repository.Segment(meta.SHA)
}

// Discover uses registered identities, legacy upload manifests and HF cached
// metadata. It never registers repositories or downloads content.
func Discover(root string) ([]Selection, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(root); err != nil {
		return nil, err
	}
	descriptors, err := repository.List(root)
	if err != nil {
		return nil, err
	}
	keys := map[string]repository.RepoKey{}
	for _, d := range descriptors {
		if d.RepoType == "models" && d.Source == "hosted" {
			keys[d.ID()] = d.RepoKey
		}
	}
	local := filepath.Join(root, "api", "models", repository.Local)
	err = filepath.WalkDir(local, func(p string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if e.IsDir() {
			return nil
		}
		if e.Name() != "dingo-local-manifest.json" {
			return nil
		}
		repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(p)))
		for _, k := range keys {
			if filepath.Clean(k.APIRoot(root)) == repoRoot {
				return nil
			}
		}
		rel, err := filepath.Rel(local, repoRoot)
		if err != nil {
			return err
		}
		k := repository.RepoKey{Namespace: repository.Local, RepoType: "models", Repo: filepath.ToSlash(rel)}
		if err = k.Validate(); err != nil {
			return err
		}
		keys[k.ID()] = k
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	out := []Selection{}
	for _, k := range keys {
		entries, err := os.ReadDir(filepath.Join(k.APIRoot(root), "revision"))
		if err != nil {
			return nil, err
		}
		revisions := map[string]string{}
		pointed := map[string]bool{}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := k.Revision(root, e.Name())
			if _, err := os.Stat(filepath.Join(p, "dingo-superseded.json")); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				return nil, err
			}
			commit, err := commitAt(root, filepath.Join(p, "meta_get.json"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			revisions[e.Name()] = commit
			if e.Name() != commit {
				pointed[commit] = true
			}
		}
		for rev, c := range revisions {
			if rev == c && pointed[c] {
				continue
			}
			out = append(out, Selection{root, k.Namespace, k.Repo, rev})
		}
	}
	catalog, err := (hfprojection.Reader{Root: root}).Catalog("models")
	if err != nil {
		return nil, err
	}
	for _, repo := range catalog.Repos {
		if repo.Error != "" {
			return nil, fmt.Errorf("%s: %s", repo.Repo, repo.Error)
		}
		for _, rev := range repo.Revisions {
			out = append(out, Selection{root, repository.HuggingFace, repo.Repo, rev.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Repo+"/"+out[i].Revision < out[j].Namespace+"/"+out[j].Repo+"/"+out[j].Revision
	})
	return out, nil
}

func resolve(s Selection) (Version, error) {
	v := Version{Selection: s, Status: "unavailable", Files: []TreeFile{}}
	k := repository.RepoKey{Namespace: s.Namespace, RepoType: "models", Repo: s.Repo}
	if err := k.Validate(); err != nil {
		return v, err
	}
	if err := repository.Segment(s.Revision); err != nil {
		return v, err
	}
	if !filepath.IsAbs(s.CacheRoot) {
		return v, fmt.Errorf("cache_root must be absolute")
	}
	if s.Namespace == repository.HuggingFace {
		m, paths, err := (hfprojection.Reader{Root: s.CacheRoot}).MountSources("models", s.Repo, s.Revision)
		if err != nil {
			return v, err
		}
		v.Commit = m.Commit
		for _, f := range m.Files {
			v.Files = append(v.Files, TreeFile{f.Path, f.Size, f.OID, paths[f.Path]})
		}
	} else {
		if s.Namespace == repository.ModelScope {
			return v, fmt.Errorf("modelscope adapter is not implemented")
		}
		if s.Namespace != repository.Local {
			d, err := repository.Read(s.CacheRoot, k)
			if err != nil {
				return v, err
			}
			if d.Source != "hosted" {
				return v, fmt.Errorf("not a hosted repository")
			}
		}
		commit, err := commitAt(s.CacheRoot, filepath.Join(k.Revision(s.CacheRoot, s.Revision), "meta_get.json"))
		if err != nil {
			return v, err
		}
		v.Commit = commit
		var manifest []struct {
			Path string `json:"path"`
			SHA  string `json:"sha256"`
			Size int64  `json:"size"`
		}
		if err = readJSON(s.CacheRoot, filepath.Join(k.Revision(s.CacheRoot, commit), "dingo-local-manifest.json"), &manifest); err != nil {
			return v, err
		}
		if manifest == nil {
			return v, fmt.Errorf("null manifest")
		}
		for _, f := range manifest {
			digest, err := hex.DecodeString(f.SHA)
			if err != nil || len(digest) != 32 || f.Size < 0 {
				return v, fmt.Errorf("invalid manifest entry %s", f.Path)
			}
			p := k.Blob(s.CacheRoot, f.SHA)
			if err = repository.SafePath(s.CacheRoot, p); err != nil {
				return v, err
			}
			v.Files = append(v.Files, TreeFile{f.Path, f.Size, f.SHA, p})
		}
	}
	seen := map[string]bool{}
	for _, f := range v.Files {
		if err := repository.Relative(f.Path); err != nil {
			return v, err
		}
		if seen[f.Path] {
			return v, fmt.Errorf("duplicate path %s", f.Path)
		}
		seen[f.Path] = true
	}
	sort.Slice(v.Files, func(i, j int) bool { return v.Files[i].Path < v.Files[j].Path })
	return v, nil
}

// Historical HF caches can contain ordinary files. Accept those only with a
// known matching size; malformed OLAH must never fall back to ordinary bytes.
func openTreePayload(name string, size int64, allowPlain bool) (*Payload, error) {
	p, err := Open(name)
	if err == nil || !allowPlain || size < 0 {
		return p, err
	}
	f, openErr := os.Open(name)
	if openErr != nil {
		return nil, openErr
	}
	var magic [4]byte
	_, _ = f.ReadAt(magic[:], 0)
	st, statErr := f.Stat()
	if statErr != nil || !st.Mode().IsRegular() || st.Size() != size || string(magic[:]) == "OLAH" {
		f.Close()
		return nil, err
	}
	return &Payload{File: f, Size: size}, nil
}

// PrepareTree pins every ready version atomically. Unavailable versions remain
// visible as error directories, never as misleading partial/empty models.
func PrepareTree(selections []Selection, mountpoint string) (*Snapshot, []Version, error) {
	snapshot := &Snapshot{Files: map[string]*Payload{}, Directories: map[string]string{}}
	versions := []Version{}
	prefixes := []string{}
	for _, s := range selections {
		k := repository.RepoKey{Namespace: s.Namespace, RepoType: "models", Repo: s.Repo}
		if err := k.Validate(); err != nil {
			snapshot.Close()
			return nil, nil, err
		}
		if err := repository.Segment(s.Revision); err != nil {
			snapshot.Close()
			return nil, nil, err
		}
		prefix := path.Join(s.Namespace, s.Repo, s.Revision)
		for _, old := range prefixes {
			if prefix == old || strings.HasPrefix(prefix, old+"/") || strings.HasPrefix(old, prefix+"/") {
				snapshot.Close()
				return nil, nil, fmt.Errorf("ambiguous version paths %s and %s", old, prefix)
			}
		}
		prefixes = append(prefixes, prefix)
		v, err := resolve(s)
		v.SourceFilePath = filepath.Join(mountpoint, filepath.FromSlash(prefix))
		opened := map[string]*Payload{}
		if err == nil {
			for _, f := range v.Files {
				var p *Payload
				p, err = openTreePayload(f.Backing, f.Size, s.Namespace == repository.HuggingFace)
				if err != nil {
					break
				}
				opened[f.Path] = p
				if f.Size >= 0 && p.Size != f.Size {
					err = fmt.Errorf("size mismatch: %s", f.Path)
					break
				}
			}
			for name := range opened {
				for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
					if _, ok := opened[parent]; ok {
						err = fmt.Errorf("file/directory collision: %s", parent)
					}
				}
			}
		}
		if err != nil {
			for _, p := range opened {
				p.File.Close()
			}
			v.Error = err.Error()
			snapshot.Directories[prefix] = v.Error
		} else {
			v.Status = "ready"
			snapshot.Directories[prefix] = ""
			for name, p := range opened {
				snapshot.Files[prefix+"/"+name] = p
			}
		}
		versions = append(versions, v)
	}
	return snapshot, versions, nil
}
