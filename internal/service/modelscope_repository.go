package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

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
	return m.repositoryJSONWithRegistration(c, k, operation, q, true)
}
func (m *ModelscopeService) repositoryJSONReadOnly(c echo.Context, k repository.RepoKey, operation string, q url.Values) (*modelscopeEnvelope, error) {
	return m.repositoryJSONWithRegistration(c, k, operation, q, false)
}
func (m *ModelscopeService) repositoryJSONWithRegistration(c echo.Context, k repository.RepoKey, operation string, q url.Values, register bool) (*modelscopeEnvelope, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	if k.Namespace != repository.ModelScope {
		return nil, fmt.Errorf("not a ModelScope repository")
	}
	base, upstream, err := modelscopeOrigin(k.Repo)
	if err != nil {
		return nil, err
	}
	if err := func() error {
		if register {
			return repository.CheckRegistration(config.SysConfig.Repos(), repository.Remote(k))
		}
		return nil
	}(); err != nil {
		return nil, err
	}
	uri := base + "/api/v1/" + k.RepoType + "/" + escapeRepoPath(upstream) + operation
	if len(q) > 0 {
		uri += "?" + q.Encode()
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"Authorization", "Cookie", "User-Agent"} {
		if upstream != k.Repo && name != "User-Agent" {
			continue
		}
		if v := c.Request().Header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	if upstream != k.Repo && config.SysConfig.Modelscope.InternationalToken != "" {
		req.Header.Set("Authorization", "Bearer "+config.SysConfig.Modelscope.InternationalToken)
	}
	resp, err := util.DoRequestWithRetry(req)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		return nil, echo.NewHTTPError(status, fmt.Sprintf("ModelScope metadata request to %s failed: %v", req.URL.Host, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, echo.NewHTTPError(resp.StatusCode, "ModelScope request rejected")
	}
	var envelope modelscopeEnvelope
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if !validModelscopeMetadata(raw) {
		return nil, echo.NewHTTPError(502, "invalid ModelScope metadata response")
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Code != 0 && envelope.Code != 200 {
		return nil, echo.NewHTTPError(502, "ModelScope application error")
	}
	if register {
		if err := repository.Register(config.SysConfig.Repos(), repository.Remote(k)); err != nil {
			return nil, err
		}
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

// A successful HTTP status alone does not establish a remote repository.
func validModelscopeMetadata(raw []byte) bool {
	var result struct {
		Code    *int
		Success *bool
		Data    json.RawMessage
	}
	if json.Unmarshal(raw, &result) != nil {
		return false
	}
	if result.Success != nil && !*result.Success {
		return false
	}
	if result.Code != nil && *result.Code != 0 && *result.Code != 200 {
		return false
	}
	if result.Code == nil && (result.Success == nil || !*result.Success) {
		return false
	}
	var data map[string]json.RawMessage
	return json.Unmarshal(result.Data, &data) == nil && data != nil
}
