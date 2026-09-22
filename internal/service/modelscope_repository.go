package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"github.com/labstack/echo/v4"
)

type ProviderRevision struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

type modelscopeEnvelope struct {
	Code    int
	Message string
	Data    struct {
		LatestCommitter struct{ Id, ShortId string }
		Files           []struct {
			Type, Path, Name, Sha256, Sha1, Revision string
			Size                                     int64
		}
		RevisionMap struct {
			Branches, Tags []struct{ Revision, Sha, CommitId string }
		}
	}
}

// ModelScope's own protocol remains byte-for-byte proxied. These methods only
// project its published Files/RevisionMap response into the control-plane API.
func (m *ModelscopeService) repositoryJSON(c echo.Context, k repository.RepoKey, operation string, q url.Values) (*modelscopeEnvelope, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	if k.Namespace != repository.ModelScope {
		return nil, fmt.Errorf("not a ModelScope repository")
	}
	if err := repository.Register(config.SysConfig.Repos(), repository.Remote(k)); err != nil {
		return nil, err
	}
	uri := strings.TrimRight(config.SysConfig.Modelscope.OfficialBaseURL, "/") + "/api/v1/" + k.RepoType + "/" + escapeRepoPath(k.Repo) + operation
	if len(q) > 0 {
		uri += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"Authorization", "Cookie", "User-Agent"} {
		if v := c.Request().Header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	resp, err := util.DoRequestWithRetry(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, echo.NewHTTPError(resp.StatusCode, "ModelScope request rejected")
	}
	var envelope modelscopeEnvelope
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 32<<20))
	if err = decoder.Decode(&envelope); err != nil {
		return nil, err
	}
	if envelope.Code != 0 && envelope.Code != 200 {
		return nil, echo.NewHTTPError(502, "ModelScope application error")
	}
	return &envelope, nil
}
func (m *ModelscopeService) RepositoryRevisions(c echo.Context, k repository.RepoKey) ([]ProviderRevision, error) {
	envelope, err := m.repositoryJSON(c, k, "/revisions", nil)
	if err != nil {
		return nil, err
	}
	revisions := []ProviderRevision{}
	for _, entry := range append(envelope.Data.RevisionMap.Branches, envelope.Data.RevisionMap.Tags...) {
		if repository.Segment(entry.Revision) != nil {
			return nil, fmt.Errorf("unsafe ModelScope revision")
		}
		commit := entry.CommitId
		if commit == "" {
			commit = entry.Sha
		}
		if commit == "" {
			commit = entry.Revision
		}
		if repository.Segment(commit) != nil {
			return nil, fmt.Errorf("unsafe ModelScope commit")
		}
		revisions = append(revisions, ProviderRevision{Name: entry.Revision, Commit: commit})
	}
	return revisions, nil
}
func (m *ModelscopeService) RepositoryTree(c echo.Context, k repository.RepoKey, revision, path string, recursive bool) ([]RepoTreeItem, error) {
	q := url.Values{"Revision": {revision}, "Recursive": {fmt.Sprint(recursive)}}
	if path != "" {
		q.Set("Root", path)
	}
	envelope, err := m.repositoryJSON(c, k, "/repo/files", q)
	if err != nil {
		return nil, err
	}
	items := []RepoTreeItem{}
	for _, entry := range envelope.Data.Files {
		name := entry.Path
		if name == "" {
			name = entry.Name
		}
		if err := repository.Relative(name); err != nil {
			return nil, err
		}
		typ := "file"
		if entry.Type == "tree" || entry.Type == "directory" {
			typ = "directory"
		}
		oid := entry.Sha256
		if oid == "" {
			oid = entry.Sha1
		}
		item := RepoTreeItem{Type: typ, Path: name, Oid: oid, Size: entry.Size}
		if entry.Sha256 != "" {
			item.Lfs = &common.Lfs{Oid: entry.Sha256, Size: entry.Size}
		}
		items = append(items, item)
	}
	return items, nil
}
func (m *ModelscopeService) RepositoryMetadata(c echo.Context, k repository.RepoKey, revision string) (map[string]interface{}, error) {
	if revision == "" {
		revision = "master"
	}
	envelope, err := m.repositoryJSON(c, k, "/repo/files", url.Values{"Revision": {revision}, "Recursive": {"true"}})
	if err != nil {
		return nil, err
	}
	if envelope.Data.LatestCommitter.Id == "" && envelope.Data.LatestCommitter.ShortId == "" {
		refs, refErr := m.RepositoryRevisions(c, k)
		if refErr != nil {
			return nil, refErr
		}
		for _, ref := range refs {
			if (ref.Name == revision || ref.Commit == revision) && ref.Commit != ref.Name {
				envelope.Data.LatestCommitter.Id = ref.Commit
				break
			}
		}
	}
	return m.cacheRepositoryMetadata(k, revision, envelope)
}
func (m *ModelscopeService) RepositoryFile(c echo.Context, k repository.RepoKey, revision, path string) error {
	return m.serveFile(c, k, revision, path)
}
