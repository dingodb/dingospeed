package hfprojection

import (
	"os"
	"sync"
)

// Index is the read-only projection used by repository catalogue queries. A
// refresh scans the existing HF cache once and atomically replaces one repo
// type; manifest reads then reuse that snapshot without walking the cache.
type Index struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex
	scopes    map[string]indexedScope
}

type indexedScope struct {
	catalog   Catalog
	manifests map[string]Manifest
	errors    map[string]error
}

func NewIndex() *Index { return &Index{scopes: make(map[string]indexedScope)} }

var DefaultIndex = NewIndex()

func scopeKey(root, repoType string) string    { return root + "\x00" + repoType }
func manifestKey(repo, revision string) string { return repo + "\x00" + revision }

// Refresh rebuilds only from local cache files. It never calls HF and never
// writes the provider cache or its database.
func (i *Index) Refresh(root, repoType string) (Catalog, error) {
	i.refreshMu.Lock()
	defer i.refreshMu.Unlock()
	return i.refresh(root, repoType)
}

func (i *Index) refresh(root, repoType string) (Catalog, error) {
	reader := Reader{Root: root}
	catalog, err := reader.Catalog(repoType)
	if err != nil {
		return Catalog{}, err
	}
	scope := indexedScope{catalog: catalog, manifests: make(map[string]Manifest), errors: make(map[string]error)}
	for _, repo := range catalog.Repos {
		if repo.Error != "" {
			continue
		}
		for _, revision := range repo.Revisions {
			manifest, readErr := reader.Manifest(repoType, repo.Repo, revision.Name)
			key := manifestKey(repo.Repo, revision.Name)
			if readErr != nil {
				scope.errors[key] = readErr
				continue
			}
			scope.manifests[key] = manifest
			commitKey := manifestKey(repo.Repo, revision.Commit)
			if _, seen := scope.manifests[commitKey]; seen {
				continue
			}
			commitManifest := manifest
			commitManifest.Revision = revision.Commit
			scope.manifests[commitKey] = commitManifest
		}
	}
	i.mu.Lock()
	i.scopes[scopeKey(root, repoType)] = scope
	i.mu.Unlock()
	return catalog, nil
}

func (i *Index) Manifest(root, repoType, repo, revision string) (Manifest, error) {
	key := scopeKey(root, repoType)
	i.mu.RLock()
	_, exists := i.scopes[key]
	i.mu.RUnlock()
	if !exists {
		i.refreshMu.Lock()
		i.mu.RLock()
		_, exists = i.scopes[key]
		i.mu.RUnlock()
		if !exists {
			_, err := i.refresh(root, repoType)
			if err != nil {
				i.refreshMu.Unlock()
				return Manifest{}, err
			}
		}
		i.refreshMu.Unlock()
	}
	i.mu.RLock()
	scope := i.scopes[key]
	manifest, ok := scope.manifests[manifestKey(repo, revision)]
	readErr := scope.errors[manifestKey(repo, revision)]
	i.mu.RUnlock()
	if readErr != nil {
		return Manifest{}, readErr
	}
	if !ok {
		return Manifest{}, os.ErrNotExist
	}
	return manifest, nil
}

// Invalidate makes the next catalogue or manifest read rebuild local facts.
// It is called after a cache task changes HF bytes.
func (i *Index) Invalidate(root, repoType string) {
	if i == nil {
		return
	}
	i.refreshMu.Lock()
	defer i.refreshMu.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if repoType == "" {
		for key := range i.scopes {
			if len(key) > len(root) && key[:len(root)+1] == root+"\x00" {
				delete(i.scopes, key)
			}
		}
		return
	}
	delete(i.scopes, scopeKey(root, repoType))
}
