package handler

import (
	"dingospeed/internal/dao"
	"dingospeed/internal/service"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"fmt"
	"github.com/labstack/echo/v4"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var repositoryMS = service.NewModelscopeService()

// RepositoryLocator validates the independent locator once. Values from
// net/url.Query are already decoded and must never be decoded again.
func RepositoryLocator(fileRequired, revisionRequired bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			q, err := parseUniqueQuery(c)
			if err != nil {
				return err
			}
			k := repository.RepoKey{Namespace: c.Param("namespace"), RepoType: c.Param("repoType"), Repo: q["repo"]}
			if err = k.Validate(); err != nil {
				return echo.NewHTTPError(400, err.Error())
			}
			revision := q["revision"]
			if revisionRequired || revision != "" {
				if err = repository.Segment(revision); err != nil {
					return echo.NewHTTPError(400, err.Error())
				}
			}
			file := q["path"]
			if fileRequired || file != "" {
				if err = repository.Relative(file); err != nil {
					return echo.NewHTTPError(400, err.Error())
				}
			}
			c.Set("repositoryKey", k)
			return next(c)
		}
	}
}

func parseUniqueQuery(c echo.Context) (map[string]string, error) {
	var values url.Values
	var err error
	// ParseQuery additionally reports malformed escapes and unescaped semicolons.
	values, err = url.ParseQuery(c.Request().URL.RawQuery)
	if err != nil {
		return nil, echo.NewHTTPError(400, "invalid query encoding")
	}
	q := map[string]string{}
	for key, vs := range values {
		if len(vs) != 1 {
			return nil, echo.NewHTTPError(400, "duplicate query parameter: "+key)
		}
		q[key] = vs[0]
	}
	for _, name := range []string{"namespace", "org", "orgRepo", "repoType"} {
		if _, ok := q[name]; ok {
			return nil, echo.NewHTTPError(400, "locator field must not override route: "+name)
		}
	}
	return q, nil
}

func requestRepoKey(c echo.Context) repository.RepoKey {
	return c.Get("repositoryKey").(repository.RepoKey)
}

// HFProtocolKey adapts only a standard upstream repo ID. Registered hosted
// namespaces require the same trusted download gateway as the own API.
func HFProtocolKey(c echo.Context, repoType, org, repo string) (repository.RepoKey, error) {
	k := repository.RepoKey{Namespace: repository.HuggingFace, RepoType: repoType, Repo: repo}
	if org != "" {
		k.Repo = org + "/" + repo
	}
	forceHF := c.Get("forcedProvider") == repository.HuggingFace
	hosted := !forceHF && (c.Get("forcedLocal") == true || org == repository.Local)
	if hosted {
		k.Namespace = org
		k.Repo = repo
	}
	if err := k.Validate(); err != nil {
		return k, echo.NewHTTPError(400, err.Error())
	}
	if hosted {
		d, err := repository.Read(config.SysConfig.Repos(), k)
		if org == repository.Local && os.IsNotExist(err) {
			// Pre-namespace uploads have manifests but no platform descriptor.
			return k, nil
		}
		if err != nil {
			return k, repositoryHTTPError(err)
		}
		if d.Source != "hosted" {
			return k, echo.NewHTTPError(404, "hosted repository not found")
		}
	}

	return k, nil
}

// The reserved HF source supports cold reads. Other namespaces need a hosted
// descriptor; corrupt descriptors and unavailable storage are never cache misses.
func readableRepository(k repository.RepoKey) error {
	_, err := repository.Read(config.SysConfig.Repos(), k)
	if k.Namespace == repository.HuggingFace && os.IsNotExist(err) {
		return nil
	}
	return err
}

func (h *FileHandler) RepositoryFile(c echo.Context) error {
	if id := c.Request().Header.Get(common.LocalTransferHeader); id != "" {
		finish, ok := common.ClaimLocalTransfer(id)
		if !ok {
			return echo.NewHTTPError(409, "local transfer no longer active")
		}
		defer finish()
		if trace := common.LocalCacheTrace(id); trace != nil {
			c.SetRequest(c.Request().WithContext(common.WithCacheTrace(c.Request().Context(), trace)))
		}
	}

	k := requestRepoKey(c)
	if k.Namespace == repository.ModelScope {
		return repositoryMS.RepositoryFile(c, k, c.QueryParam("revision"), c.QueryParam("path"))
	}
	if err := readableRepository(k); err != nil {
		return repositoryHTTPError(err)
	}
	if c.Request().Method == http.MethodHead {
		return h.fileService.FileHeadCommon(c, k.RepoType, k.ID(), c.QueryParam("revision"), c.QueryParam("path"))
	}
	return h.fileGetCommon(c, k.RepoType, k.ID(), c.QueryParam("revision"), c.QueryParam("path"))
}
func (h *MetaHandler) RepositorySnapshot(c echo.Context) error {
	k := requestRepoKey(c)
	d, err := repository.Read(config.SysConfig.Repos(), k)
	if err != nil {
		return repositoryHTTPError(err)
	}
	if d.Source != "hosted" {
		return echo.NewHTTPError(400, "snapshot requires a hosted repository")
	}
	snap, err := h.metaService.GetLocalSnapshot(k.RepoType, k.ID(), c.QueryParam("revision"))
	if err != nil {
		return repositoryHTTPError(err)
	}
	verify := c.QueryParam("verify")
	if verify == "true" || verify == "sha256" {
		files := make([]dao.LocalManifestFile, len(snap.Files))
		for i, f := range snap.Files {
			files[i] = dao.LocalManifestFile{Path: f.Path, Size: f.Size, Sha256: f.Sha256}
		}
		if err := dao.VerifyPublishedFiles(k.RepoType, k.ID(), snap.Commit, files); err != nil {
			return repositoryHTTPError(err)
		}
		if verify == "sha256" {
			if err := dao.VerifyPublishedSHA256(k.RepoType, k.ID(), files); err != nil {
				return repositoryHTTPError(err)
			}
		}
	}
	return c.JSON(200, map[string]interface{}{"namespace": k.Namespace, "repoType": k.RepoType, "repo": k.Repo, "revision": c.QueryParam("revision"), "commit": snap.Commit, "files": snap.Files, "complete": true, "fileCount": len(snap.Files), "verified": verify == "true" || verify == "sha256", "contentVerified": verify == "sha256"})
}
func (h *MetaHandler) RepositoryArchive(c echo.Context) error {
	k := requestRepoKey(c)
	d, err := repository.Read(config.SysConfig.Repos(), k)
	if err != nil {
		return repositoryHTTPError(err)
	}
	if d.Source != "hosted" {
		return echo.NewHTTPError(400, "archive requires a hosted repository")
	}
	revision := c.QueryParam("revision")
	if _, err = h.metaService.GetLocalSnapshot(k.RepoType, k.ID(), revision); err != nil {
		return repositoryHTTPError(err)
	}
	setLocalArchiveHeaders(c, filepath.Base(k.Repo)+"-"+revision+".zip")
	if c.Request().Method == http.MethodHead {
		return c.NoContent(200)
	}
	return h.metaService.StreamLocalArchive(k.RepoType, k.ID(), revision, filepath.Base(k.Repo), c.Response())
}
func (h *MetaHandler) RepositoryTree(c echo.Context) error {
	k := requestRepoKey(c)
	if k.Namespace == repository.ModelScope {
		items, err := repositoryMS.RepositoryTree(c, k, c.QueryParam("revision"), c.QueryParam("path"), strings.EqualFold(c.QueryParam("recursive"), "true"))
		if err != nil {
			return err
		}
		return c.JSON(200, items)
	}
	if err := readableRepository(k); err != nil {
		return repositoryHTTPError(err)
	}
	items, err := h.metaService.GetRepoTree(k.RepoType, k.ID(), c.QueryParam("revision"), c.QueryParam("path"), strings.EqualFold(c.QueryParam("recursive"), "true"), c.Request().Header.Get("Authorization"))
	if err != nil {
		return repositoryHTTPError(err)
	}
	if k.Namespace == repository.HuggingFace {
		if err := repository.Register(config.SysConfig.Repos(), repository.Remote(k)); err != nil {
			return repositoryHTTPError(err)
		}
	}
	return c.JSON(200, items)
}
func (h *MetaHandler) RepositoryMetadata(c echo.Context) error {
	if id := c.Request().Header.Get(common.LocalTransferHeader); id != "" {
		finish, ok := common.ClaimLocalTransfer(id)
		if !ok {
			return echo.NewHTTPError(409, "local transfer no longer active")
		}
		defer finish()
		if trace := common.LocalCacheTrace(id); trace != nil {
			c.SetRequest(c.Request().WithContext(common.WithCacheTrace(c.Request().Context(), trace)))
		}
	}

	k := requestRepoKey(c)
	if k.Namespace == repository.ModelScope {
		meta, err := repositoryMS.RepositoryMetadata(c, k, c.QueryParam("revision"))
		if err != nil {
			return err
		}
		c.Response().Header().Set("X-Repo-Commit", fmt.Sprint(meta["sha"]))
		if c.Request().Method == http.MethodHead {
			return c.NoContent(200)
		}
		return c.JSON(200, meta)
	}
	if k.Namespace != repository.HuggingFace {
		if _, err := repository.Read(config.SysConfig.Repos(), k); err != nil {
			return repositoryHTTPError(err)
		}
	}
	meta, err := h.metaService.GetMetadata(k.RepoType, k.ID(), c.QueryParam("revision"), strings.ToLower(c.Request().Method), c.Request().Header.Get("Authorization"))
	if err != nil {
		return repositoryHTTPError(err)
	}
	for key, value := range meta.Headers {
		c.Response().Header().Set(key, value)
	}
	if c.Request().Method == http.MethodHead {
		return c.NoContent(meta.StatusCode)
	}
	return c.Blob(meta.StatusCode, "application/json", meta.OriginContent)
}
func (h *MetaHandler) RepositoryDirectories(c echo.Context) error {
	if _, err := parseUniqueQuery(c); err != nil {
		return err
	}
	namespace, typ := c.Param("namespace"), c.Param("repoType")
	if err := (repository.RepoKey{Namespace: namespace, RepoType: typ, Repo: "placeholder"}).Validate(); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	all, err := repository.Discover(c.Request().Context(), config.SysConfig.Repos(), typ, namespace)
	if err != nil {
		return repositoryHTTPError(err)
	}
	result := []repository.RepoKey{}
	for _, d := range all {
		if d.Namespace == namespace && d.RepoType == typ {
			result = append(result, d)
		}
	}
	return c.JSON(200, result)
}
func (h *MetaHandler) RepositoryRevisions(c echo.Context) error {
	k := requestRepoKey(c)
	if k.Namespace == repository.ModelScope {
		items, err := repositoryMS.RepositoryRevisions(c, k)
		if err != nil {
			return err
		}
		return c.JSON(200, items)
	}
	if _, err := repository.Read(config.SysConfig.Repos(), k); err != nil {
		return repositoryHTTPError(err)
	}
	entries, err := os.ReadDir(filepath.Join(k.APIRoot(config.SysConfig.Repos()), "revision"))
	if os.IsNotExist(err) {
		entries = nil
	} else if err != nil {
		return repositoryHTTPError(err)
	}
	result := []map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		commit, err := h.metaService.RevisionCommit(k.RepoType, k.ID(), entry.Name())
		if err != nil {
			return repositoryHTTPError(err)
		}
		if entry.Name() != commit {
			result = append(result, map[string]string{"name": entry.Name(), "commit": commit})
		}
	}
	return c.JSON(200, result)
}
func repositoryHTTPError(err error) error {
	if e, ok := err.(*echo.HTTPError); ok {
		return e
	}
	if os.IsNotExist(err) {
		return echo.NewHTTPError(404, "repository entry not found")
	}
	if e, ok := err.(interface{ StatusCode() int }); ok {
		return echo.NewHTTPError(e.StatusCode(), err.Error())
	}
	return echo.NewHTTPError(500, fmt.Sprintf("repository access failed: %v", err))
}

func (h *MetaHandler) RepositoryFiles(c echo.Context) error {
	k := requestRepoKey(c)
	if k.Namespace == repository.ModelScope {
		items, err := repositoryMS.RepositoryTree(c, k, c.QueryParam("revision"), c.QueryParam("path"), false)
		if err != nil {
			return err
		}
		files := []map[string]interface{}{}
		for _, item := range items {
			q := url.Values{"repo": {k.Repo}, "revision": {c.QueryParam("revision")}, "path": {item.Path}}
			link := "/api/repositories/" + url.PathEscape(k.RepoType) + "/" + url.PathEscape(k.Namespace) + "/file?" + q.Encode()
			files = append(files, map[string]interface{}{"name": filepath.Base(item.Path), "size": item.Size, "isDir": item.Type == "directory", "link": link})
		}
		return c.JSON(200, files)
	}
	if _, err := repository.Read(config.SysConfig.Repos(), k); err != nil {
		return repositoryHTTPError(err)
	}
	commit, err := h.metaService.RevisionCommit(k.RepoType, k.ID(), c.QueryParam("revision"))
	if err != nil {
		return repositoryHTTPError(err)
	}
	files, err := h.metaService.RepositoryFiles(k.RepoType, k.ID(), commit, c.QueryParam("path"))
	if err != nil {
		return repositoryHTTPError(err)
	}
	return c.JSON(200, files)
}
func (h *FileHandler) RepositoryOffset(c echo.Context) error {
	k := requestRepoKey(c)
	if _, err := repository.Read(config.SysConfig.Repos(), k); err != nil {
		return repositoryHTTPError(err)
	}
	etag := c.QueryParam("etag")
	if err := repository.Segment(etag); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	size, err := strconv.ParseInt(c.QueryParam("size"), 10, 64)
	if err != nil || size < 0 {
		return echo.NewHTTPError(400, "invalid size")
	}
	return c.JSON(200, h.fileService.GetFileOffset(k.RepoType, k.Namespace, k.Repo, etag, size))
}

func (h *MetaHandler) ProtocolRefs(c echo.Context) error {
	k, err := HFProtocolKey(c, c.Param("repoType"), c.Param("org"), c.Param("repo"))
	if err != nil {
		return err
	}
	if k.Namespace == repository.HuggingFace {
		return h.metaService.ForwardToNewSite(c)
	}
	entries, err := os.ReadDir(filepath.Join(k.APIRoot(config.SysConfig.Repos()), "revision"))
	if err != nil {
		return repositoryHTTPError(err)
	}
	branches := []map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		commit, err := h.metaService.RevisionCommit(k.RepoType, k.ID(), entry.Name())
		if err != nil {
			return repositoryHTTPError(err)
		}
		if entry.Name() != commit {
			branches = append(branches, map[string]string{"name": entry.Name(), "ref": "refs/heads/" + entry.Name(), "targetCommit": commit})
		}
	}
	return c.JSON(200, map[string]interface{}{"branches": branches, "tags": []interface{}{}, "converts": []interface{}{}})
}
