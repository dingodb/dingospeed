package hfprojection

import (
	"context"
	"os"
	"sync"
)

// Index is the read-only projection used by repository catalogue queries. A
// refresh reads repository metadata only. File manifests are loaded lazily for
// the requested repository and revision, then reused until refresh/invalidation.
type Index struct {
	mu          sync.RWMutex
	refreshGate chan struct{}
	scopes      map[string]indexedScope
}

type indexedScope struct {
	generation *int
	catalog    Catalog
	manifests  map[string]Manifest
	errors     map[string]error
}

func NewIndex() *Index {
	return &Index{scopes: make(map[string]indexedScope), refreshGate: make(chan struct{}, 1)}
}

var DefaultIndex = NewIndex()

func scopeKey(root, repoType string) string    { return root + "\x00" + repoType }
func manifestKey(repo, revision string) string { return repo + "\x00" + revision }

// Refresh rebuilds only from local cache files. It never calls HF and never
// writes the provider cache or its database.
func (i *Index) Refresh(root, repoType string) (Catalog, error) {
	return i.RefreshContext(context.Background(), root, repoType)
}

func (i *Index) RefreshContext(ctx context.Context, root, repoType string) (Catalog, error) {
	select {
	case i.refreshGate <- struct{}{}:
		defer func() { <-i.refreshGate }()
	case <-ctx.Done():
		return Catalog{}, ctx.Err()
	}
	return i.refreshContext(ctx, root, repoType)
}

func (i *Index) refreshContext(ctx context.Context, root, repoType string) (Catalog, error) {
	reader := Reader{Root: root, Context: ctx}
	catalog, err := reader.Catalog(repoType)
	if err != nil {
		return Catalog{}, err
	}
	if err := ctx.Err(); err != nil {
		return Catalog{}, err
	}
	scope := indexedScope{generation: new(int), catalog: catalog, manifests: make(map[string]Manifest), errors: make(map[string]error)}

	i.mu.Lock()
	i.scopes[scopeKey(root, repoType)] = scope
	i.mu.Unlock()
	return catalog, nil
}

func (i *Index) Manifest(root, repoType, repo, revision string) (Manifest, error) {
	return i.ManifestContext(context.Background(), root, repoType, repo, revision)
}
func (i *Index) ManifestContext(ctx context.Context, root, repoType, repo, revision string) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	key := scopeKey(root, repoType)
	i.mu.RLock()
	_, exists := i.scopes[key]
	i.mu.RUnlock()
	if !exists {
		if _, err := i.RefreshContext(ctx, root, repoType); err != nil {
			return Manifest{}, err
		}
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
		known := false
		for _, candidate := range scope.catalog.Repos {
			if candidate.Repo == repo {
				for _, rev := range candidate.Revisions {
					if rev.Name == revision || rev.Commit == revision {
						known = true
					}
				}
			}
		}
		if !known {
			return Manifest{}, os.ErrNotExist
		}
		manifest, readErr = (Reader{Root: root, Context: ctx}).Manifest(repoType, repo, revision)
		if readErr != nil {
			return Manifest{}, readErr
		}
		i.mu.Lock()
		current, exists := i.scopes[key]
		if exists && current.manifests != nil && current.generation == scope.generation {
			current.manifests[manifestKey(repo, revision)] = manifest
		}
		i.mu.Unlock()
	}
	return manifest, nil
}

// Invalidate makes the next catalogue or manifest read rebuild local facts.
// It is called after a cache task changes HF bytes.
func (i *Index) Invalidate(root, repoType string) {
	if i == nil {
		return
	}
	i.refreshGate <- struct{}{}
	defer func() { <-i.refreshGate }()
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
