package service

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"dingospeed/internal/downloader"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

func (m *ModelscopeService) HandleFileDownload(c echo.Context, owner, repo, repoType string) error {
	q, err := url.ParseQuery(c.Request().URL.RawQuery)
	if err != nil || len(q["Revision"]) > 1 || len(q["FilePath"]) != 1 {
		return echo.NewHTTPError(400, "invalid file locator")
	}
	return m.serveFile(c, repository.RepoKey{Namespace: repository.ModelScope, RepoType: repoType, Repo: owner + "/" + repo}, q.Get("Revision"), q.Get("FilePath"))
}

func (m *ModelscopeService) serveFile(c echo.Context, k repository.RepoKey, revision, path string) error {
	if revision == "" {
		revision = "master"
	}
	if err := k.Validate(); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	if err := repository.Segment(revision); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	if err := repository.Relative(path); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	// A file-list request both resolves the mutable revision and checks the caller's access.
	envelope, err := m.repositoryJSON(c, k, "/repo/files", url.Values{"Revision": {revision}, "Recursive": {"true"}})
	if err != nil {
		return err
	}
	var info *common.PathsInfo
	commit := ""
	for _, entry := range envelope.Data.Files {
		name := entry.Path
		if name == "" {
			name = entry.Name
		}
		if name != path || entry.Type == "tree" || entry.Type == "directory" {
			continue
		}
		if info != nil {
			return echo.NewHTTPError(502, "ambiguous upstream file")
		}
		oid := entry.Sha256
		if oid == "" {
			oid = entry.Sha1
		}
		if repository.Segment(oid) != nil || entry.Size < 0 {
			return echo.NewHTTPError(502, "missing upstream file identity")
		}
		commit = entry.Revision
		// Some endpoints return the commit on the revision record instead of each file.
		if commit == "" {
			refs, e := m.RepositoryRevisions(c, k)
			if e != nil {
				return e
			}
			for _, ref := range refs {
				if (ref.Name == revision || ref.Commit == revision) && ref.Commit != ref.Name {
					commit = ref.Commit
					break
				}
			}
		}
		if repository.Segment(commit) != nil {
			return echo.NewHTTPError(502, "upstream did not provide a file revision")
		}
		info = &common.PathsInfo{Type: "file", Path: path, Oid: oid, Size: entry.Size}
	}
	if info == nil {
		return echo.NewHTTPError(404, "file not found")
	}
	// Record a repository only when the upstream supplies repository-level
	// identity. Missing repository metadata never raises single-file admission.
	if envelope.Data.LatestCommitter.Id != "" || envelope.Data.LatestCommitter.ShortId != "" {
		if _, err = m.cacheRepositoryMetadata(k, revision, envelope); err != nil {
			// An unrelated sibling must not raise admission for this valid file.
			// Explicit repository preheating still reports metadata errors.
			zap.S().Warnw("Could not record ModelScope repository snapshot", "repo", k.Repo, "error", err)
		}
	}
	// Use the same paths-info envelope as HF, without inventing a full repository snapshot.
	if err = m.fileDao.CacheProviderFileInfo(k, commit, revision, info); err != nil {
		return err
	}
	uri := "/api/v1/" + k.RepoType + "/" + repository.EscapeURLPath(k.Repo) + "/repo?" + url.Values{"Revision": {commit}, "FilePath": {path}}.Encode()
	headers := map[string]string{}
	for _, name := range []string{"Authorization", "Cookie", "User-Agent"} {
		if v := c.Request().Header.Get(name); v != "" {
			headers[strings.ToLower(name)] = v
		}
	}
	source := &downloader.RemoteSource{Domain: strings.TrimRight(config.SysConfig.Modelscope.OfficialBaseURL, "/"), Headers: headers, Fetch: modelscopeFetch(info.Size)}
	return m.fileDao.ServePreparedFile(c, k, commit, path, strings.ToLower(c.Request().Method), info, uri, source)
}

// ModelScope may return 200 with Content-Range for a partial response. Normalize
// that transport detail, retaining the shared streaming/retry/block machinery.
func modelscopeFetch(size int64) func(context.Context, string, string, map[string]string, func(*http.Response) error) error {
	return func(ctx context.Context, domain, uri string, headers map[string]string, consume func(*http.Response) error) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, domain+uri, nil)
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := util.DoRequestWithRetry(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 && resp.StatusCode != 206 {
			return myerr.NewAppendCode(resp.StatusCode, "ModelScope download rejected")
		}
		start, end, err := util.FileRange(req.Header.Get("Range"), size)
		if err != nil {
			return err
		}
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			var a, b, total int64
			if n, e := fmt.Sscanf(cr, "bytes %d-%d/%d", &a, &b, &total); e != nil || n != 3 || a != start || b != end-1 || total != size {
				return fmt.Errorf("ModelScope returned a mismatched range")
			}
			resp.StatusCode = http.StatusPartialContent
		} else if start != 0 || end != size {
			return fmt.Errorf("ModelScope ignored the byte range")
		}
		if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
			return fmt.Errorf("unexpected encoded ModelScope range")
		}
		resp.Header.Del("Content-Encoding")
		return consume(resp)
	}
}
