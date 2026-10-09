package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"dingospeed/internal/dao"
	"dingospeed/internal/service"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"github.com/labstack/echo/v4"
)

type RepositorySyncHandler struct {
	service *service.RepositorySyncService
	upload  *service.UploadService
}

func NewRepositorySyncHandler(upload *service.UploadService) *RepositorySyncHandler {
	return &RepositorySyncHandler{service: service.NewRepositorySyncService(upload), upload: upload}
}

// RepositorySyncGuard limits node synchronization to registered cluster mode.
// It uses the existing management-port boundary without a separate sync key.
func RepositorySyncGuard(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !config.SysConfig.Registration().Enabled {
			return c.JSON(409, map[string]string{"code": "SYNC_STANDALONE"})
		}
		return next(c)
	}
}
func (h *RepositorySyncHandler) Start(c echo.Context) error {
	var req service.SyncRequest
	dec := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return c.JSON(400, map[string]string{"code": "SYNC_INVALID_ARGUMENT"})
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return c.JSON(400, map[string]string{"code": "SYNC_INVALID_ARGUMENT"})
	}
	if req.Namespace != "" && req.Namespace != c.Param("namespace") || req.RepoType != "" && req.RepoType != c.Param("repoType") {
		return c.JSON(400, map[string]string{"code": "SYNC_IDENTITY_MISMATCH"})
	}
	req.Namespace = c.Param("namespace")
	req.RepoType = c.Param("repoType")
	var status service.SyncStatus
	var err error
	if strings.HasSuffix(c.Path(), "/prepare") {
		status, err = h.service.Prepare(req)
	} else {
		status, err = h.service.Start(req)
	}
	if err != nil {
		return writeUploadError(c, "sync start failed", err)
	}
	return c.JSON(http.StatusAccepted, status)
}
func (h *RepositorySyncHandler) Status(c echo.Context) error {
	status, err := h.service.Status(c.Param("taskId"))
	if errors.Is(err, os.ErrNotExist) {
		return c.NoContent(404)
	}
	if err != nil {
		return writeUploadError(c, "sync status failed", err)
	}
	return c.JSON(200, status)
}
func (h *RepositorySyncHandler) Cancel(c echo.Context) error {
	status, err := h.service.Cancel(c.Param("taskId"))
	if errors.Is(err, os.ErrNotExist) {
		return c.NoContent(404)
	}
	if err != nil {
		return writeUploadError(c, "sync cancel failed", err)
	}
	return c.JSON(200, status)
}
func (h *RepositorySyncHandler) Content(c echo.Context) error {
	size, err := strconv.ParseInt(c.QueryParam("size"), 10, 64)
	if err != nil || size < 0 {
		return c.NoContent(400)
	}
	k := repository.RepoKey{Namespace: c.Param("namespace"), RepoType: c.Param("repoType"), Repo: c.QueryParam("repo")}
	f := dao.LocalManifestFile{Path: c.QueryParam("path"), Sha256: c.QueryParam("sha256"), Size: size}
	c.Response().Header().Set("Content-Type", "application/octet-stream")
	start, end, rangeErr := dao.ParseSyncRange(c.Request().Header.Get("Range"), size)
	if rangeErr != nil {
		c.Response().Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		return c.NoContent(416)
	}
	c.Response().Header().Set("Accept-Ranges", "bytes")
	c.Response().Header().Set("Content-Length", strconv.FormatInt(end-start, 10))
	if c.Request().Header.Get("Range") != "" {
		c.Response().Status = http.StatusPartialContent
		c.Response().Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))
	}
	err = h.upload.CopySyncContentRange(c.Request().Context(), k, f, start, end, c.Response())
	if err != nil {
		if c.Response().Committed {
			return err
		}
		c.Response().Header().Del("Content-Length")
		c.Response().Header().Del("Content-Range")
		return c.JSON(404, map[string]string{"code": "SYNC_CONTENT_UNAVAILABLE"})
	}
	return nil
}

func (h *RepositorySyncHandler) List(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	out, err := h.service.List(limit, c.QueryParam("before"))
	if err != nil {
		return writeUploadError(c, "sync list failed", err)
	}
	return c.JSON(200, out)
}
func (h *RepositorySyncHandler) Action(c echo.Context) error {
	var status service.SyncStatus
	var err error
	switch c.Param("action") {
	case "execute":
		status, err = h.service.Execute(c.Param("taskId"))
	case "pause":
		status, err = h.service.Pause(c.Param("taskId"))
	case "resume":
		status, err = h.service.Resume(c.Param("taskId"))
	case "retry":
		status, err = h.service.Retry(c.Param("taskId"))
	case "cancel":
		status, err = h.service.Cancel(c.Param("taskId"))
	case "dismiss":
		status, err = h.service.Dismiss(c.Param("taskId"))
	default:
		return c.NoContent(404)
	}
	if errors.Is(err, os.ErrNotExist) {
		return c.NoContent(404)
	}
	if err != nil {
		return writeUploadError(c, "sync action failed", err)
	}
	return c.JSON(200, status)
}

func (h *RepositorySyncHandler) RemotePrepare(c echo.Context) error {
	var req service.RemoteSyncRequest
	dec := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 8<<20))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return c.NoContent(400)
	}
	if (req.Namespace != "" && req.Namespace != c.Param("namespace")) || (req.RepoType != "" && req.RepoType != c.Param("repoType")) {
		return c.NoContent(400)
	}
	req.Namespace = c.Param("namespace")
	req.RepoType = c.Param("repoType")
	status, err := h.service.Prepare(req.Request())
	if err != nil {
		return writeUploadError(c, "remote prepare failed", err)
	}
	return c.JSON(200, status)
}
func (h *RepositorySyncHandler) RemoteManifest(c echo.Context) error {
	k := repository.RepoKey{Namespace: c.Param("namespace"), RepoType: c.Param("repoType"), Repo: c.QueryParam("repo")}
	m, err := h.service.RemoteManifest(c.Request().Context(), k, c.QueryParam("revision"), c.QueryParam("source"), c.Request().Header.Get("X-Dingo-Origin-Authorization"))
	if errors.Is(err, os.ErrNotExist) {
		return c.NoContent(404)
	}
	if err != nil {
		return writeUploadError(c, "remote manifest unavailable", err)
	}
	return c.JSON(200, map[string]any{"namespace": k.Namespace, "repoType": k.RepoType, "repo": k.Repo, "revision": c.QueryParam("revision"), "commit": m.Commit, "files": m.Files, "manifestDigest": service.RemoteManifestDigest(m), "manifestComplete": true, "allowOrigin": config.SysConfig.Online()})
}
func RepositoryRemoteContent(c echo.Context) error {
	size, err := strconv.ParseInt(c.QueryParam("size"), 10, 64)
	if err != nil || size < 0 {
		return c.NoContent(400)
	}
	k := repository.RepoKey{Namespace: c.Param("namespace"), RepoType: c.Param("repoType"), Repo: c.QueryParam("repo")}
	oid := c.QueryParam("oid")
	kind := "sha256"
	if len(oid) == 40 {
		kind = "provider-sha1"
		if k.Namespace == repository.HuggingFace {
			kind = "git-blob-sha1"
		}
	}
	f := dao.RemoteSyncFile{Path: c.QueryParam("path"), OID: oid, Size: size, IdentityKind: kind}
	commit := c.QueryParam("commit")
	if err = dao.ValidateRemoteManifest(k, &dao.RemoteSyncManifest{Commit: commit, Files: []dao.RemoteSyncFile{f}}); err != nil {
		return c.NoContent(400)
	}
	start, end, err := dao.ParseSyncRange(c.Request().Header.Get("Range"), size)
	if err != nil {
		return c.NoContent(416)
	}
	c.Response().Header().Set("Content-Type", "application/octet-stream")
	c.Response().Header().Set("Content-Length", fmt.Sprint(end-start))
	c.Response().Header().Set("Accept-Ranges", "bytes")
	if c.Request().Header.Get("Range") != "" {
		c.Response().Status = 206
		c.Response().Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))
	}
	if err = dao.CopyRemoteSyncContent(c.Request().Context(), k, commit, f, start, end, c.Response()); err != nil {
		if c.Response().Committed {
			return err
		}
		c.Response().Header().Del("Content-Length")
		c.Response().Header().Del("Content-Range")
		return c.NoContent(404)
	}
	return nil
}

func (h *RepositorySyncHandler) RemoteSourceReport(c echo.Context) error {
	var req service.RemoteSourceReportRequest
	dec := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 1<<20))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return c.NoContent(400)
	}
	result, err := h.service.ReportRemoteSource(repository.RepoKey{Namespace: c.Param("namespace"), RepoType: c.Param("repoType"), Repo: req.Repo}, req)
	if err != nil {
		return writeUploadError(c, "remote source report failed", err)
	}
	return c.JSON(200, result)
}
