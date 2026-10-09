package service

import (
	"context"
	"dingospeed/internal/dao"
	"dingospeed/internal/downloader"
	"dingospeed/internal/service/task"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/transfersettings"
	"dingospeed/pkg/util"
	"errors"
	"fmt"
	"github.com/labstack/echo/v4"
	"net/http"
	"net/url"
	"os"
	"sort"
)

type RemoteSyncRequest struct {
	TaskID             string               `json:"taskId"`
	Namespace          string               `json:"namespace"`
	RepoType           string               `json:"repoType"`
	Repo               string               `json:"repo"`
	Revision           string               `json:"revision"`
	BaseManifestDigest string               `json:"baseManifestDigest,omitempty"`
	BaseCommit         string               `json:"baseCommit,omitempty"`
	ExpectedAbsent     bool                 `json:"expectedAbsent"`
	SourceCommit       string               `json:"sourceCommit"`
	Files              []dao.RemoteSyncFile `json:"files"`
}

func (r RemoteSyncRequest) Request() SyncRequest {
	return SyncRequest{TaskID: r.TaskID, Namespace: r.Namespace, RepoType: r.RepoType, Repo: r.Repo, Revision: r.Revision, BaseCommit: r.BaseCommit, ExpectedAbsent: r.ExpectedAbsent, BaseManifestDigest: r.BaseManifestDigest, Remote: &dao.RemoteSyncManifest{Commit: r.SourceCommit, Files: r.Files}}
}
func validateSyncRequest(r SyncRequest) error {
	if r.Remote == nil {
		return validatePublishTreeParam(r.publication())
	}
	if repository.Segment(r.Revision) != nil || len(r.Files) != 0 {
		return fmt.Errorf("invalid remote sync identity")
	}
	if r.ExpectedAbsent {
		if r.BaseCommit != "" {
			return fmt.Errorf("baseCommit and expectedAbsent are mutually exclusive")
		}
	} else if repository.Segment(r.BaseCommit) != nil || len(r.BaseManifestDigest) != 64 {
		return fmt.Errorf("base commit is required")
	}
	return dao.ValidateRemoteManifest(r.key(), r.Remote)
}
func syncFileCount(r SyncRequest) int {
	if r.Remote != nil {
		return len(r.Remote.Files)
	}
	return len(r.Files)
}
func syncManifestDigest(r SyncRequest) string {
	if r.Remote != nil {
		return digestJSON(r.Remote)
	}
	return digestJSON(r.Files)
}
func syncTotalBytes(r SyncRequest) int64 {
	var n int64
	if r.Remote != nil {
		for _, f := range r.Remote.Files {
			n += f.Size
		}
	} else {
		for _, f := range r.Files {
			n += f.Size
		}
	}
	return n
}
func (s *RepositorySyncService) readSyncBaseline(r SyncRequest) (string, []dao.LocalManifestFile, error) {
	if r.Remote != nil {
		m, err := dao.ReadRemoteSyncManifest(r.key(), r.Revision)
		if err == nil && !r.ExpectedAbsent && RemoteManifestDigest(m) != r.BaseManifestDigest {
			return "", nil, fmt.Errorf("remote target manifest changed")
		}
		return m.Commit, nil, err
	}
	return dao.ReadInventorySnapshot(r.key(), r.Revision)
}
func (s *RepositorySyncService) RemoteManifest(ctx context.Context, k repository.RepoKey, revision, source, authorization string) (dao.RemoteSyncManifest, error) {
	if k.Validate() != nil || repository.Segment(revision) != nil {
		return dao.RemoteSyncManifest{}, fmt.Errorf("invalid source")
	}
	if source == "" || source == "local" {
		return dao.ReadRemoteSyncManifest(k, revision)
	}
	if source != "upstream" {
		return dao.RemoteSyncManifest{}, fmt.Errorf("invalid manifest source")
	}
	if !config.SysConfig.Online() {
		return dao.RemoteSyncManifest{}, fmt.Errorf("origin access is disabled")
	}
	if k.Namespace == repository.HuggingFace {
		return s.upload.uploadDao.ResolveHFManifest(ctx, k, revision, authorization)
	}
	if k.Namespace != repository.ModelScope {
		return dao.RemoteSyncManifest{}, fmt.Errorf("not a remote repository")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/", nil)
	req.Header.Set("Authorization", authorization)
	c := echo.New().NewContext(req, nil)
	envelope, err := NewModelscopeService().repositoryJSONReadOnly(c, k, "/repo/files", url.Values{"Revision": {revision}, "Recursive": {"true"}})
	if err != nil {
		return dao.RemoteSyncManifest{}, err
	}
	out := dao.RemoteSyncManifest{Commit: envelope.Data.LatestCommitter.Id, Files: []dao.RemoteSyncFile{}}
	if out.Commit == "" {
		out.Commit = envelope.Data.LatestCommitter.ShortId
	}
	for _, f := range envelope.Data.Files {
		if f.Type == "tree" || f.Type == "directory" {
			continue
		}
		path := f.Path
		if path == "" {
			path = f.Name
		}
		oid, kind := f.Sha256, "sha256"
		if oid == "" {
			oid, kind = f.Sha1, "provider-sha1"
		}
		out.Files = append(out.Files, dao.RemoteSyncFile{Path: path, OID: oid, Size: f.Size, IdentityKind: kind, UpstreamFileRevision: f.Revision})
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out, dao.ValidateRemoteManifest(k, &out)
}
func (s *RepositorySyncService) remoteTransfer(ctx context.Context, e *syncEntry, f dao.RemoteSyncFile) error {
	k := e.request.key()
	commit := e.request.Remote.Commit
	var uri string
	source := &downloader.RemoteSource{}
	if k.Namespace == repository.HuggingFace {
		prefix := ""
		if k.RepoType != "models" {
			prefix = "/" + k.RepoType
		}
		source.Domain = config.SysConfig.GetHFURLBase()
		uri = prefix + "/" + repository.EscapeURLPath(k.Repo) + "/resolve/" + url.PathEscape(commit) + "/" + repository.EscapeURLPath(f.Path)
		source.Fetch = func(ctx context.Context, domain, uri string, headers map[string]string, consume func(*http.Response) error) error {
			if !config.SysConfig.Online() {
				return fmt.Errorf("origin access disabled")
			}
			return util.GetStreamContext(ctx, domain, uri, headers, consume)
		}
	} else {
		base, upstream, err := modelscopeOrigin(k.Repo)
		if err != nil {
			return err
		}
		source.Domain = base
		revision := f.UpstreamFileRevision
		if revision == "" {
			revision = commit
		}
		uri = "/api/v1/" + k.RepoType + "/" + repository.EscapeURLPath(upstream) + "/repo?" + url.Values{"Revision": {revision}, "FilePath": {f.Path}}.Encode()
		fetch := modelscopeFetch(f.Size)
		source.Fetch = func(ctx context.Context, domain, uri string, headers map[string]string, consume func(*http.Response) error) error {
			if !config.SysConfig.Online() {
				return fmt.Errorf("origin access disabled")
			}
			return fetch(ctx, domain, uri, headers, consume)
		}
		if upstream != k.Repo && config.SysConfig.Modelscope.InternationalToken != "" {
			source.Headers = map[string]string{"authorization": "Bearer " + config.SysConfig.Modelscope.InternationalToken}
		}
	}
	return s.upload.uploadDao.DownloadRemoteSyncFile(transfersettings.WithBackground(ctx), k, commit, f, uri, source, func(n int64) {
		e.persist.Lock()
		defer e.persist.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.entries[e.request.TaskID] == e {
			e.status.TransferredBytes += n
			e.status.SnapshotVersion++
		}
	})
}
func (s *RepositorySyncService) executeRemote(ctx context.Context, e *syncEntry) {
	defer func() {
		s.mu.Lock()
		if s.entries[e.request.TaskID] == e {
			if s.active[syncTarget(e.request)] == e.request.TaskID {
				delete(s.active, syncTarget(e.request))
			}
			if e.cancel != nil {
				e.cancel()
			}
			e.cancel = nil
		}
		s.mu.Unlock()
		s.signalRecovery()
	}()
	err := s.transition(e, "downloading", nil)
	if err == nil {
		limit := transfersettings.Current().Upload
		if limit <= 0 || limit > 4 {
			limit = 4
		}
		err = task.ExecuteManifest(ctx, limit, len(e.request.Remote.Files), func(ctx context.Context, i int) (func(context.Context) error, error) {
			f := e.request.Remote.Files[i]
			return func(ctx context.Context) error {
				if err := s.remoteTransfer(ctx, e, f); err != nil {
					return fmt.Errorf("%s: %w", f.Path, err)
				}
				e.persist.Lock()
				s.mu.Lock()
				e.status.CompletedFiles++
				e.status.SnapshotVersion++
				s.mu.Unlock()
				e.persist.Unlock()
				return nil
			}, nil
		})
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		state := "failed"
		if errors.Is(err, context.Canceled) {
			state = "interrupted"
			if s.snapshot(e).State == "pausing" {
				state = "paused"
			}
			if s.snapshot(e).State == "canceling" {
				state = "cancelled"
			}
		}
		_ = s.transition(e, state, err)
		return
	}
	if !config.SysConfig.Registration().Enabled {
		_ = s.transition(e, "failed", fmt.Errorf("node registration disabled"))
		return
	}
	if err = s.transition(e, "publishing", nil); err != nil {
		state := "failed"
		if errors.Is(err, context.Canceled) {
			state = "paused"
			if s.snapshot(e).State == "canceling" {
				state = "cancelled"
			}
		}
		_ = s.transition(e, state, err)
		return
	}
	r := e.request
	err = s.upload.uploadDao.PublishRemoteSync(r.key(), r.Revision, r.BaseCommit, r.ExpectedAbsent, r.BaseManifestDigest, *r.Remote, e.fingerprint)
	if err != nil {
		state := "failed"
		var uncertain dao.RemotePublicationUncertain
		if errors.As(err, &uncertain) {
			state = "result_unconfirmed"
		}
		if code, ok := err.(interface{ ErrorCode() string }); ok && code.ErrorCode() == "REVISION_CHANGED" {
			state = "conflict"
		}
		_ = s.transition(e, state, err)
		return
	}
	m, err := dao.ReadRemoteSyncManifest(r.key(), r.Remote.Commit)
	if err != nil || digestJSON(m) != digestJSON(r.Remote) {
		_ = s.transition(e, "result_unconfirmed", fmt.Errorf("remote publication readback unavailable"))
		return
	}
	s.mu.Lock()
	e.status.Commit = m.Commit
	s.mu.Unlock()
	if err = s.transition(e, "completed", nil); err != nil {
		s.mu.Lock()
		e.status.State = "result_unconfirmed"
		e.status.Error = "published but receipt persistence failed"
		e.status.SnapshotVersion++
		s.mu.Unlock()
	}
}
func (s *RepositorySyncService) recoverRemotePublication(e *syncEntry) {
	r := e.request
	okay, err := s.upload.uploadDao.RemotePublicationMatches(r.key(), r.Revision, e.fingerprint, *r.Remote)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.deferRecovery(e, err)
		return
	}
	if okay {
		dao.InvalidateRemoteSyncIndex(r.key())
		m, err := dao.ReadRemoteSyncManifest(r.key(), r.Remote.Commit)
		if err != nil || digestJSON(m) != digestJSON(r.Remote) {
			s.deferRecovery(e, fmt.Errorf("fixed remote result unavailable"))
			return
		}
		s.mu.Lock()
		e.status.Commit = m.Commit
		e.status.CompletedFiles = len(m.Files)
		s.mu.Unlock()
		s.setRecoveryResult(e, "completed", nil)
		return
	}
	commit, _, err := s.readSyncBaseline(r)
	if (r.ExpectedAbsent && errors.Is(err, os.ErrNotExist)) || (!r.ExpectedAbsent && err == nil && commit == r.BaseCommit) {
		s.setRecoveryResult(e, "interrupted", fmt.Errorf("remote publication not observed; resume with fixed baseline"))
		return
	}
	s.setRecoveryResult(e, "needs_attention", fmt.Errorf("remote publication receipt unavailable; inspect target before any new task"))
}

func RemoteManifestDigest(m dao.RemoteSyncManifest) string { return digestJSON(m) }

type RemoteSourceReportRequest struct {
	Repo           string `json:"repo"`
	Revision       string `json:"revision"`
	Commit         string `json:"commit"`
	ManifestDigest string `json:"manifestDigest"`
}
type RemoteSourceReport struct {
	ReportedFiles int      `json:"reportedFiles"`
	TotalFiles    int      `json:"totalFiles"`
	Warnings      []string `json:"warnings,omitempty"`
}

func (s *RepositorySyncService) ReportRemoteSource(k repository.RepoKey, r RemoteSourceReportRequest) (RemoteSourceReport, error) {
	result := RemoteSourceReport{}
	if !config.SysConfig.Registration().Enabled {
		return result, syncError(409, "SYNC_STANDALONE", "node registration is disabled")
	}
	if r.Repo != k.Repo || repository.Segment(r.Revision) != nil || repository.Segment(r.Commit) != nil {
		return result, syncError(400, "SYNC_INVALID_ARGUMENT", "invalid remote source locator")
	}
	m, err := dao.ReadRemoteSyncManifest(k, r.Commit)
	if err != nil {
		return result, err
	}
	if m.Commit != r.Commit || RemoteManifestDigest(m) != r.ManifestDigest {
		return result, syncError(409, "REVISION_CHANGED", "fixed source manifest does not match")
	}
	result.TotalFiles = len(m.Files)
	result.ReportedFiles, err = s.upload.uploadDao.ReportRemoteSyncSources(k, m)
	if err != nil {
		return result, syncError(502, "SYNC_SOURCE_REPORT_FAILED", "node inventory report did not complete")
	}
	if result.ReportedFiles < result.TotalFiles {
		result.Warnings = []string{fmt.Sprintf("%d files have no complete local cache", result.TotalFiles-result.ReportedFiles)}
	}
	return result, nil
}
