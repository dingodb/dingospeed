package handler

import (
	"errors"
	"net/http"
	"os"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/hfprojection"
	"dingospeed/pkg/repository"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

func HFLocalCatalog(c echo.Context) error {
	if _, err := parseUniqueQuery(c); err != nil {
		return err
	}
	if c.Param("namespace") != repository.HuggingFace && c.Param("namespace") != repository.ModelScope {
		return echo.NewHTTPError(400, "local projection requires a remote repository")
	}
	k := repository.RepoKey{Namespace: c.Param("namespace"), RepoType: c.Param("repoType"), Repo: "placeholder"}
	if err := k.Validate(); err != nil {
		return echo.NewHTTPError(400, err.Error())
	}
	var out hfprojection.Catalog
	var err error
	if k.Namespace == repository.ModelScope {
		out, err = (hfprojection.Reader{Root: config.SysConfig.Repos(), Namespace: k.Namespace, Context: c.Request().Context()}).Catalog(k.RepoType)
	} else {
		out, err = hfprojection.DefaultIndex.RefreshContext(c.Request().Context(), config.SysConfig.Repos(), k.RepoType)
	}
	if err != nil {
		return hfProjectionError(err)
	}
	return c.JSON(200, out)
}

func HFLocalManifest(c echo.Context) error {
	k := requestRepoKey(c)
	if k.Namespace != repository.HuggingFace && k.Namespace != repository.ModelScope {
		return echo.NewHTTPError(400, "local projection requires a remote repository")
	}
	var out hfprojection.Manifest
	var err error
	if k.Namespace == repository.ModelScope {
		out, err = (hfprojection.Reader{Root: config.SysConfig.Repos(), Namespace: k.Namespace, Context: c.Request().Context()}).Manifest(k.RepoType, k.Repo, c.QueryParam("revision"))
	} else {
		out, err = hfprojection.DefaultIndex.ManifestContext(c.Request().Context(), config.SysConfig.Repos(), k.RepoType, k.Repo, c.QueryParam("revision"))
	}
	if err != nil {
		return hfProjectionError(err)
	}
	return c.JSON(200, out)
}

func HFLocalFile(c echo.Context) error {
	k := requestRepoKey(c)
	if k.Namespace != repository.HuggingFace && k.Namespace != repository.ModelScope {
		return echo.NewHTTPError(400, "local projection requires a remote repository")
	}
	f, content, err := (hfprojection.Reader{Root: config.SysConfig.Repos(), Namespace: k.Namespace}).OpenFile(k.RepoType, k.Repo, c.QueryParam("revision"), c.QueryParam("path"))
	if errors.Is(err, hfprojection.ErrIncomplete) {
		return echo.NewHTTPError(409, err.Error())
	}
	if err != nil {
		return hfProjectionError(err)
	}
	defer f.Close()
	c.Response().Header().Set("Cache-Control", "no-store")
	http.ServeContent(c.Response(), c.Request(), c.QueryParam("path"), time.Time{}, content)
	return nil
}

func hfProjectionError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return echo.NewHTTPError(404, "local cache entry not found")
	}
	zap.S().Warnw("Could not read HF cache projection", "error", err)
	return echo.NewHTTPError(500, "local cache is unreadable or metadata is invalid")
}
