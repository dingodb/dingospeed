package hfprojection

import "fmt"

// MountSources resolves one observed manifest, without re-reading a mutable
// branch for each file. The caller must validate and pin every payload before
// publishing the mount. Paths are local administrative data, not an HTTP API.
func (r Reader) MountSources(typ, repo, revision string) (Manifest, map[string]string, error) {
	m, err := r.Manifest(typ, repo, revision)
	if err != nil {
		return m, nil, err
	}
	if !m.ManifestComplete {
		return m, nil, fmt.Errorf("complete cached repository manifest required")
	}
	paths := make(map[string]string, len(m.Files))
	for _, f := range m.Files {
		if f.localPath == "" {
			return m, nil, fmt.Errorf("missing content: %s", f.Path)
		}
		p, err := r.contentPath(m.RepoKey, f.localPath)
		if err != nil {
			return m, nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		paths[f.Path] = p
	}
	return m, paths, nil
}
