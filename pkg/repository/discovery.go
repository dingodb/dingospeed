package repository

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Discover reads only the requested namespace. It never registers legacy data.
// Hosted names may contain multiple segments; remote names have at most two.
func Discover(ctx context.Context, root, typ, namespace string) ([]RepoKey, error) {
	probe := RepoKey{Namespace: namespace, RepoType: typ, Repo: "placeholder"}
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	base := filepath.Dir(probe.APIRoot(root))
	result := []RepoKey{}
	err := filepath.WalkDir(base, func(p string, e fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if p == base && errors.Is(walkErr, os.ErrNotExist) {
				return filepath.SkipDir
			}
			return walkErr
		}
		if !e.IsDir() || p == base {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		if namespace == HuggingFace && (strings.Split(rel, string(filepath.Separator))[0] == Local || strings.Split(rel, string(filepath.Separator))[0] == ModelScope) {
			return filepath.SkipDir
		}
		key := RepoKey{Namespace: namespace, RepoType: typ, Repo: filepath.ToSlash(rel)}
		if err := key.Validate(); err != nil {
			return filepath.SkipDir
		}
		if err := SafePath(root, p); err != nil {
			return err
		}
		if err := SafePath(root, filepath.Join(p, Marker)); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(p, Marker))
		if err == nil {
			var d Descriptor
			if err := json.Unmarshal(data, &d); err != nil {
				return err
			}
			if err := d.Validate(); err != nil {
				return err
			}
			if filepath.Clean(d.APIRoot(root)) != filepath.Clean(p) {
				return errors.New("repository marker path mismatch")
			}
			if d.Namespace == namespace && d.RepoType == typ {
				result = append(result, d.RepoKey)
			}
			return filepath.SkipDir
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if namespace == HuggingFace || namespace == ModelScope {
			if len(strings.Split(key.Repo, "/")) >= 2 {
				return filepath.SkipDir
			}
			return nil
		}
		legacy, err := legacyHosted(ctx, root, p)
		if err != nil {
			return err
		}
		if legacy {
			result = append(result, key)
			return filepath.SkipDir
		}

		return nil
	})
	return result, err
}

// Inspect fixed manifest locations, never paths-info or model data. A valid
// manifest array is the legacy hosted format; ordinary folders do not qualify.
func legacyHosted(ctx context.Context, root, p string) (bool, error) {
	revisions := filepath.Join(p, "revision")
	if err := SafePath(root, revisions); err != nil {
		return false, err
	}
	entries, err := os.ReadDir(revisions)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !e.IsDir() {
			continue
		}
		marker := filepath.Join(revisions, e.Name(), "dingo-local-manifest.json")
		if err := SafePath(root, marker); err != nil {
			return false, err
		}
		b, err := os.ReadFile(marker)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		var files []struct {
			Path   string `json:"path"`
			Sha256 string `json:"sha256"`
			Size   int64  `json:"size"`
		}
		if json.Unmarshal(b, &files) != nil || files == nil {
			return false, errors.New("invalid legacy repository manifest")
		}
		for _, file := range files {
			if Relative(file.Path) != nil || len(file.Sha256) != 64 || file.Size < 0 {
				return false, errors.New("invalid legacy repository manifest entry")
			}
			if _, err := hex.DecodeString(file.Sha256); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	return false, nil
}
