package hfprojection

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Index caches local projection metadata. Specific manifest reads never build
// the catalogue and never wait for its potentially slow full refresh.
type Index struct {
	mu             sync.RWMutex
	refreshGate    chan struct{}
	scopes         map[string]indexedScope
	readCatalog    func(context.Context, string, string) (Catalog, error)
	readRepository func(context.Context, string, string, string) (Repo, error)
}
type indexedScope struct {
	generation            *int
	reset                 *int
	repositoryGenerations map[string]*int
	catalog               Catalog
	manifests             map[string]Manifest
	errors                map[string]error
}

func newScope() indexedScope {
	return indexedScope{generation: new(int), reset: new(int), repositoryGenerations: make(map[string]*int), manifests: make(map[string]Manifest), errors: make(map[string]error)}
}
func NewIndex() *Index {
	return &Index{scopes: make(map[string]indexedScope), refreshGate: make(chan struct{}, 1),
		readCatalog: func(ctx context.Context, root, typ string) (Catalog, error) {
			return (Reader{Root: root, Context: ctx}).Catalog(typ)
		}, readRepository: func(ctx context.Context, root, typ, repo string) (Repo, error) {
			return (Reader{Root: root, Context: ctx}).Repository(typ, repo)
		}}
}

var DefaultIndex = NewIndex()

func scopeKey(root, repoType string) string    { return root + "\x00" + repoType }
func manifestKey(repo, revision string) string { return repo + "\x00" + revision }
func (i *Index) scope(key string) indexedScope {
	i.mu.Lock()
	defer i.mu.Unlock()
	s, ok := i.scopes[key]
	if !ok {
		s = newScope()
		i.scopes[key] = s
	}
	return s
}
func (i *Index) Refresh(root, repoType string) (Catalog, error) {
	return i.RefreshContext(context.Background(), root, repoType)
}

// RefreshContext retains explicit full-catalogue refresh. Concurrent precise
// changes prevent an older disk read from replacing newer index state.
func (i *Index) RefreshContext(ctx context.Context, root, repoType string) (Catalog, error) {
	select {
	case i.refreshGate <- struct{}{}:
		defer func() { <-i.refreshGate }()
	case <-ctx.Done():
		return Catalog{}, ctx.Err()
	}
	key := scopeKey(root, repoType)
	for {
		before := i.scope(key)
		catalog, err := i.readCatalog(ctx, root, repoType)
		if err != nil {
			return Catalog{}, err
		}
		if err := ctx.Err(); err != nil {
			return Catalog{}, err
		}
		i.mu.Lock()
		current := i.scopes[key]
		if current.generation != before.generation {
			i.mu.Unlock()
			continue
		}
		next := newScope()
		next.catalog = catalog
		i.scopes[key] = next
		i.mu.Unlock()
		return catalog, nil
	}
}
func (i *Index) Manifest(root, repoType, repo, revision string) (Manifest, error) {
	return i.ManifestContext(context.Background(), root, repoType, repo, revision)
}
func (i *Index) ManifestContext(ctx context.Context, root, repoType, repo, revision string) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	key, mk := scopeKey(root, repoType), manifestKey(repo, revision)
	scope := i.scope(key)
	i.mu.RLock()
	repoGeneration := scope.repositoryGenerations[repo]
	manifest, ok := scope.manifests[mk]
	i.mu.RUnlock()
	if ok {
		return manifest, nil
	}
	manifest, err := (Reader{Root: root, Context: ctx}).Manifest(repoType, repo, revision)
	if err != nil {
		return Manifest{}, err
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	i.mu.Lock()
	current := i.scopes[key]
	if current.reset == scope.reset && current.repositoryGenerations[repo] == repoGeneration {
		current.manifests[mk] = manifest
	}
	i.mu.Unlock()
	return manifest, nil
}

// InvalidateRevision evicts a named alias/commit and aliases resolving to that
// commit. Moving an alias preserves cached old immutable commits and other
// revisions. Call after the metadata write completes.
func (i *Index) InvalidateRevision(root, repoType, repo, revision string) {
	i.invalidateRepository(root, repoType, repo, revision)
}

// InvalidateRepository evicts manifests of one repository, without waiting for
// full-catalogue I/O or invalidating any sibling repository.
func (i *Index) InvalidateRepository(root, repoType, repo string) {
	i.invalidateRepository(root, repoType, repo, "")
}
func (i *Index) invalidateRepository(root, repoType, repo, revision string) {
	if i == nil {
		return
	}
	key := scopeKey(root, repoType)
	i.mu.Lock()
	defer i.mu.Unlock()
	s, ok := i.scopes[key]
	if !ok {
		s = newScope()
	}
	s.generation = new(int)
	s.repositoryGenerations[repo] = new(int)
	// This is an in-memory O(cached manifests + catalogue entries) filter;
	// it performs no filesystem work while holding the index lock.
	for k, m := range s.manifests {
		if strings.HasPrefix(k, repo+"\x00") && (revision == "" || k == manifestKey(repo, revision) || m.Commit == revision) {
			delete(s.manifests, k)
		}
	}
	// Catalog callers explicitly refresh; discard only this stale entry until
	// RefreshRepositoryContext supplies current local metadata.
	repos := make([]Repo, 0, len(s.catalog.Repos))
	for _, r := range s.catalog.Repos {
		if r.Repo != repo {
			repos = append(repos, r)
		}
	}
	s.catalog.Repos = repos
	i.scopes[key] = s
}

// RefreshRepositoryContext updates only this repository's catalogue metadata.
// It never enumerates sibling repositories or builds file manifests.
func (i *Index) RefreshRepositoryContext(ctx context.Context, root, repoType, repo string) (Repo, error) {
	key := scopeKey(root, repoType)
	for {
		if err := ctx.Err(); err != nil {
			return Repo{}, err
		}
		before := i.scope(key)
		i.mu.RLock()
		repoGeneration := before.repositoryGenerations[repo]
		i.mu.RUnlock()
		r, err := i.readRepository(ctx, root, repoType, repo)
		if err != nil {
			return Repo{}, err
		}
		if err := ctx.Err(); err != nil {
			return Repo{}, err
		}
		i.mu.Lock()
		s := i.scopes[key]
		if s.reset != before.reset || s.repositoryGenerations[repo] != repoGeneration {
			i.mu.Unlock()
			continue
		}
		s.generation = new(int)
		s.repositoryGenerations[repo] = new(int)
		repos := make([]Repo, 0, len(s.catalog.Repos)+1)
		for _, old := range s.catalog.Repos {
			if old.Repo != repo {
				repos = append(repos, old)
			}
		}
		repos = append(repos, r)
		sort.Slice(repos, func(a, b int) bool { return repos[a].Repo < repos[b].Repo })
		s.catalog = Catalog{Namespace: r.Namespace, RepoType: repoType, Repos: repos}
		i.scopes[key] = s
		i.mu.Unlock()
		return r, nil
	}
}

// Invalidate retains whole-type/root semantics without waiting for disk I/O.
// Generation tokens stop an in-flight refresh resurrecting old data.
func (i *Index) Invalidate(root, repoType string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if repoType == "" {
		for key := range i.scopes {
			if strings.HasPrefix(key, root+"\x00") {
				i.scopes[key] = newScope()
			}
		}
	} else {
		i.scopes[scopeKey(root, repoType)] = newScope()
	}
}
