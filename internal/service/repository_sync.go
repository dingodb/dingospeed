package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/internal/service/task"
	"dingospeed/pkg/config"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/transfersettings"
	"dingospeed/pkg/util"
)

// SyncRequest is a fixed uploaded-domain manifest supplied by the
// business gateway. No caller-controlled peer URLs or remote fallbacks exist.
type SyncRequest struct {
	BaseManifestDigest string                  `json:"baseManifestDigest,omitempty"`
	TaskID             string                  `json:"taskId"`
	Namespace          string                  `json:"namespace"`
	RepoType           string                  `json:"repoType"`
	Repo               string                  `json:"repo"`
	Revision           string                  `json:"revision"`
	BaseCommit         string                  `json:"baseCommit,omitempty"`
	ExpectedAbsent     bool                    `json:"expectedAbsent"`
	Files              []dao.LocalManifestFile `json:"files"`
	Remote             *dao.RemoteSyncManifest `json:"remote,omitempty"`
}

func (r SyncRequest) key() repository.RepoKey {
	return repository.RepoKey{Namespace: r.Namespace, RepoType: r.RepoType, Repo: r.Repo}
}
func (r SyncRequest) publication() dao.LocalPublishTreeParam {
	return dao.LocalPublishTreeParam{Namespace: r.Namespace, RepoType: r.RepoType, Repo: r.Repo, Revision: r.Revision, BaseCommit: r.BaseCommit, ExpectedAbsent: r.ExpectedAbsent, Files: r.Files}
}

type SyncStatus struct {
	ProtocolVersion int       `json:"protocolVersion"`
	TotalBytes      int64     `json:"totalBytes"`
	ExpiresAt       time.Time `json:"expiresAt,omitempty"`
	Generation      uint64    `json:"generation"`
	SnapshotVersion uint64    `json:"snapshotVersion"`
	RepoType        string    `json:"repoType"`
	Namespace       string    `json:"namespace"`
	Repo            string    `json:"repo"`
	Revision        string    `json:"revision"`
	BaseCommit      string    `json:"baseCommit,omitempty"`
	ExpectedAbsent  bool      `json:"expectedAbsent"`
	Dismissed       bool      `json:"dismissed"`
	AllowedActions  []string  `json:"allowedActions"`

	TaskID           string    `json:"taskId"`
	State            string    `json:"state"`
	ManifestDigest   string    `json:"manifestDigest"`
	CompletedFiles   int       `json:"completedFiles"`
	TotalFiles       int       `json:"totalFiles"`
	TransferredBytes int64     `json:"transferredBytes"`
	Error            string    `json:"error,omitempty"`
	Commit           string    `json:"commit,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt"`
	PublicationID    string    `json:"publicationId,omitempty"`
	RecoveryAttempts int       `json:"recoveryAttempts,omitempty"`
	RetryAt          time.Time `json:"retryAt,omitempty"`
}

type SyncSource struct {
	InstanceID    string `json:"instanceId"`
	ManagementURL string `json:"managementUrl"`
}
type SyncResolver func(context.Context, repository.RepoKey, dao.LocalManifestFile) ([]SyncSource, error)

type syncEntry struct {
	persist      sync.Mutex
	savedVersion uint64
	request      SyncRequest
	fingerprint  string
	executionID  string
	status       SyncStatus
	cancel       context.CancelFunc
}

// RepositorySyncService owns only node-local execution. Startup does not scan
// repositories/tasks: a requested unfinished receipt is reported as interrupted.
type RepositorySyncService struct {
	upload       *UploadService
	root         string
	mu           sync.Mutex
	admission    sync.Map
	entries      map[string]*syncEntry
	active       map[string]string
	resolve      SyncResolver
	client       *http.Client
	wakeRecovery chan struct{}
}

func NewRepositorySyncService(upload *UploadService) *RepositorySyncService {
	s := &RepositorySyncService{upload: upload, root: filepath.Join(config.SysConfig.Repos(), ".repository-sync"), entries: map[string]*syncEntry{}, active: map[string]string{}, client: &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	s.resolve = s.schedulerSources
	s.wakeRecovery = make(chan struct{}, 1)
	return s
}

func syncError(status int, code, message string) error {
	return uploadError{status: status, code: code, msg: message}
}
func syncActive(state string) bool {
	return state == "queued" || state == "downloading" || state == "publishing" || state == "pausing" || state == "canceling" || state == "resuming"
}
func (s *RepositorySyncService) taskPath(id, name string) string {
	return filepath.Join(s.root, id, name+".json")
}
func syncID(id string) bool { return validRepoSegment(id) && repository.Segment(id) == nil }
func syncTarget(r SyncRequest) string {
	return r.RepoType + "\x00" + r.Namespace + "\x00" + r.Repo + "\x00" + r.Revision
}
func digestJSON(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Separate task/target admission locks keep filesystem work for A off B's path.
func (s *RepositorySyncService) lockAdmission(key string) func() {
	v, _ := s.admission.LoadOrStore(key, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}
func executionFingerprint(r SyncRequest) string { r.TaskID = ""; return digestJSON(r) }
func (s *RepositorySyncService) snapshot(e *syncEntry) SyncStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return decorateSyncStatus(e.status)
}

func (s *RepositorySyncService) Start(r SyncRequest) (SyncStatus, error)   { return s.start(r, true) }
func (s *RepositorySyncService) Prepare(r SyncRequest) (SyncStatus, error) { return s.start(r, false) }
func (s *RepositorySyncService) start(r SyncRequest, startNow bool) (SyncStatus, error) {
	if !config.SysConfig.Registration().Enabled {
		return SyncStatus{}, syncError(409, "SYNC_STANDALONE", "node registration is disabled")
	}
	if !syncID(r.TaskID) {
		return SyncStatus{}, syncError(400, "SYNC_INVALID_ARGUMENT", "invalid taskId")
	}
	if err := validateSyncRequest(r); err != nil {
		return SyncStatus{}, syncError(400, "SYNC_INVALID_ARGUMENT", err.Error())
	}
	if r.Remote != nil {
		copyManifest := *r.Remote
		copyManifest.Files = append([]dao.RemoteSyncFile{}, r.Remote.Files...)
		sort.Slice(copyManifest.Files, func(i, j int) bool { return copyManifest.Files[i].Path < copyManifest.Files[j].Path })
		r.Remote = &copyManifest
	}
	r.Files = append([]dao.LocalManifestFile{}, r.Files...)
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Path < r.Files[j].Path })
	fingerprint := digestJSON(r)
	executionID := executionFingerprint(r)
	unlockTask := s.lockAdmission("task:" + r.TaskID)
	defer unlockTask()
	unlockTarget := s.lockAdmission("target:" + syncTarget(r))
	defer unlockTarget()
	if entry, err := s.load(r.TaskID); err == nil {
		if entry.fingerprint != fingerprint {
			return SyncStatus{}, syncError(409, "SYNC_REQUEST_CHANGED", "taskId already names another request")
		}
		return s.durableSnapshot(entry)
	} else if !errors.Is(err, os.ErrNotExist) {
		return SyncStatus{}, err
	}
	s.mu.Lock()
	id := s.active[syncTarget(r)]
	if id != "" {
		entry := s.entries[id]
		if entry.executionID == executionID {
			s.mu.Unlock()
			return s.durableSnapshot(entry)
		}
		s.mu.Unlock()
		return SyncStatus{}, syncError(409, "SYNC_REVISION_BUSY", "revision is being synchronized by task "+id)
	}
	s.mu.Unlock()
	// Validate the preview's condition before spending any transfer work. Final
	// PublishTree checks it again under its publication lock.
	commit, _, err := s.readSyncBaseline(r)
	if r.ExpectedAbsent {
		if err == nil {
			return SyncStatus{}, syncError(409, "REVISION_CHANGED", "revision was created after selection")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return SyncStatus{}, err
		}
	} else if err != nil || commit != r.BaseCommit {
		return SyncStatus{}, syncError(409, "REVISION_CHANGED", "revision changed after selection")
	}
	if !startNow && r.Remote == nil {
		for _, f := range r.Files {
			available, e := s.upload.SyncBlobAvailable(r.key(), f)
			if e != nil {
				return SyncStatus{}, e
			}
			if !available && f.Size > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				sources, e := s.resolve(ctx, r.key(), f)
				cancel()
				if e != nil {
					return SyncStatus{}, e
				}
				if len(sources) == 0 {
					return SyncStatus{}, syncError(409, "SYNC_NO_SOURCE", "no available node copy: "+f.Path)
				}
			}
		}
	}
	if !startNow && r.Remote != nil && !config.SysConfig.Online() {
		for _, f := range r.Remote.Files {
			available, err := s.upload.uploadDao.RemoteSyncSourceAvailable(r.key(), f)
			if err != nil {
				return SyncStatus{}, err
			}
			if !available {
				return SyncStatus{}, syncError(409, "SYNC_ORIGIN_DISABLED", "no complete node copy and origin access is disabled: "+f.Path)
			}
		}
	}
	e := &syncEntry{request: r, fingerprint: fingerprint, executionID: executionID, status: SyncStatus{TaskID: r.TaskID, State: "queued", ManifestDigest: syncManifestDigest(r), PublicationID: fingerprint, TotalFiles: syncFileCount(r), UpdatedAt: time.Now().UTC()}}
	e.status = initializeSyncStatus(e.status, r)
	if !startNow {
		e.status.State = "prepared"
		e.status.ExpiresAt = time.Now().Add(30 * time.Minute).UTC()
	}
	e.status.Generation = 1
	e.status.SnapshotVersion = 1
	if err := util.WriteDataToFileAtomic(s.taskPath(r.TaskID, "manifest"), r); err != nil {
		return SyncStatus{}, err
	}
	if err := util.WriteDataToFileAtomic(s.taskPath(r.TaskID, "status"), e.status); err != nil {
		return SyncStatus{}, err
	}
	e.savedVersion = e.status.SnapshotVersion
	if !startNow {
		s.mu.Lock()
		s.entries[r.TaskID] = e
		s.mu.Unlock()
		return decorateSyncStatus(e.status), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	s.mu.Lock()
	s.entries[r.TaskID] = e
	s.active[syncTarget(r)] = r.TaskID
	status := decorateSyncStatus(e.status)
	s.mu.Unlock()
	go s.execute(ctx, e)
	return status, nil
}

func (s *RepositorySyncService) load(id string) (*syncEntry, error) {
	s.mu.Lock()
	e := s.entries[id]
	s.mu.Unlock()
	if e != nil {
		return e, nil
	}
	b, err := os.ReadFile(s.taskPath(id, "manifest"))
	if err != nil {
		return nil, err
	}
	e = &syncEntry{}
	if err = json.Unmarshal(b, &e.request); err != nil {
		return nil, err
	}
	if e.request.TaskID != id {
		return nil, fmt.Errorf("task receipt identity mismatch")
	}
	if err = validateSyncRequest(e.request); err != nil {
		return nil, err
	}
	e.request.Files = append([]dao.LocalManifestFile{}, e.request.Files...)
	sort.Slice(e.request.Files, func(i, j int) bool { return e.request.Files[i].Path < e.request.Files[j].Path })
	b, err = os.ReadFile(s.taskPath(id, "status"))
	if errors.Is(err, os.ErrNotExist) {
		// A crash between the two atomic writes must still bind this ID to the
		// original request; never overwrite the persisted manifest on retry.
		e.status = SyncStatus{TaskID: id, State: "queued", ManifestDigest: syncManifestDigest(e.request), TotalFiles: syncFileCount(e.request)}
	} else if err != nil {
		return nil, err
	} else if err = json.Unmarshal(b, &e.status); err != nil {
		return nil, err
	}
	e.fingerprint = digestJSON(e.request)
	e.executionID = executionFingerprint(e.request)
	if e.status.TaskID != "" && e.status.TaskID != id {
		return nil, fmt.Errorf("task status identity mismatch")
	}
	if e.status.PublicationID != "" && e.status.PublicationID != e.fingerprint {
		return nil, fmt.Errorf("task publication identity mismatch")
	}
	manifestDigest := syncManifestDigest(e.request)
	if e.status.ManifestDigest != "" && e.status.ManifestDigest != manifestDigest {
		return nil, fmt.Errorf("task status manifest mismatch")
	}
	e.status.TaskID = id
	e.status.ManifestDigest = manifestDigest
	e.status.TotalFiles = syncFileCount(e.request)
	migrateSummary := e.status.ProtocolVersion != 1 || e.status.Namespace == ""
	e.status = initializeSyncStatus(e.status, e.request)
	if migrateSummary {
		e.status.SnapshotVersion++
		if err = util.WriteDataToFileAtomic(s.taskPath(id, "status"), e.status); err != nil {
			return nil, err
		}
	}
	if e.status.State == "publishing" {
		e.status.State = "result_unconfirmed"
		e.status.Error = "publication was interrupted; checking its durable receipt"
		e.status.SnapshotVersion++
		if err = util.WriteDataToFileAtomic(s.taskPath(id, "status"), e.status); err != nil {
			return nil, err
		}
	} else if e.status.State == "canceling" {
		e.status.State = "cancelled"
		e.status.SnapshotVersion++
		if err = util.WriteDataToFileAtomic(s.taskPath(id, "status"), e.status); err != nil {
			return nil, err
		}
	} else if syncActive(e.status.State) {
		e.status.State = "interrupted"
		e.status.SnapshotVersion++
		e.status.Error = "execution stopped; resume this task with its fixed manifest and target condition"
		e.status.UpdatedAt = time.Now().UTC()
		if err = util.WriteDataToFileAtomic(s.taskPath(id, "status"), e.status); err != nil {
			return nil, err
		}
	}
	e.savedVersion = e.status.SnapshotVersion
	pendingRecovery := e.status.State == "result_unconfirmed"
	s.mu.Lock()
	s.entries[id] = e
	s.mu.Unlock()
	if pendingRecovery {
		s.signalRecovery()
	}
	return e, nil
}

func (s *RepositorySyncService) Status(id string) (SyncStatus, error) {
	if !syncID(id) {
		return SyncStatus{}, syncError(400, "SYNC_INVALID_ARGUMENT", "invalid taskId")
	}
	unlock := s.lockAdmission("task:" + id)
	defer unlock()
	e, err := s.load(id)
	if err != nil {
		return SyncStatus{}, err
	}
	return s.durableSnapshot(e)
}
func (s *RepositorySyncService) Cancel(id string) (SyncStatus, error) {
	return s.stopSync(id, "canceling")
}
func (s *RepositorySyncService) Pause(id string) (SyncStatus, error) {
	return s.stopSync(id, "pausing")
}

func (s *RepositorySyncService) transition(e *syncEntry, state string, cause error) error {
	e.persist.Lock()
	defer e.persist.Unlock()
	s.mu.Lock()
	if current := s.entries[e.request.TaskID]; current != nil && current != e {
		s.mu.Unlock()
		return context.Canceled
	}
	// A stop intent wins over a worker beginning its transfer or publication.
	if (state == "downloading" || state == "publishing") && (e.status.State == "pausing" || e.status.State == "canceling") {
		s.mu.Unlock()
		return context.Canceled
	}
	before := e.status
	e.status.State = state
	e.status.UpdatedAt = time.Now().UTC()
	e.status.SnapshotVersion++
	if state == "paused" || state == "cancelled" {
		cause = nil
	}
	if cause != nil {
		e.status.Error = cause.Error()
	} else {
		e.status.Error = ""
	}
	status := e.status
	s.mu.Unlock()
	if err := util.WriteDataToFileAtomic(s.taskPath(e.request.TaskID, "status"), status); err != nil {
		s.mu.Lock()
		e.status = before
		s.mu.Unlock()
		return err
	}
	e.savedVersion = status.SnapshotVersion
	return nil
}
func (s *RepositorySyncService) execute(ctx context.Context, e *syncEntry) {
	if e.request.Remote != nil {
		s.executeRemote(ctx, e)
		return
	}
	releaseContent := dao.PinSyncManifest(e.request.key(), e.request.Files)
	defer releaseContent()
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
		err = task.ExecuteManifest(ctx, limit, syncFileCount(e.request), func(ctx context.Context, i int) (func(context.Context) error, error) {
			f := e.request.Files[i]
			return func(ctx context.Context) error {
				if err := s.obtain(ctx, e, f); err != nil {
					return fmt.Errorf("%s: %w", f.Path, err)
				}
				e.persist.Lock()
				s.mu.Lock()
				if s.entries[e.request.TaskID] == e {
					e.status.CompletedFiles++
					e.status.SnapshotVersion++
				}
				s.mu.Unlock()
				e.persist.Unlock()
				return nil
			}, nil
		})
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && !config.SysConfig.Registration().Enabled {
		err = fmt.Errorf("node registration disabled before publication")
	}
	if err != nil {
		state := "failed"
		if errors.Is(err, context.Canceled) {
			state = "interrupted"
			if s.snapshot(e).State == "pausing" {
				state = "paused"
			} else if s.snapshot(e).State == "canceling" {
				state = "cancelled"
			}
		}
		_ = s.transition(e, state, err)
		return
	}
	// transition and stop share the per-task persistence lock: publication
	// admission is durable before the irreversible call and rejects late stops.
	err = s.transition(e, "publishing", nil)
	if err != nil {
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
	result, err := s.upload.PublishTree(s.publication(e))
	if err != nil {
		// IO errors may have happened after the durable publication intent.
		// Do not claim rollback or replay blindly.
		state := "result_unconfirmed"
		if code, ok := err.(interface{ ErrorCode() string }); ok && code.ErrorCode() == "REVISION_CHANGED" {
			state = "conflict"
		}
		if sc, ok := err.(interface{ StatusCode() int }); ok && sc.StatusCode() == 404 {
			state = "conflict"
		}
		_ = s.transition(e, state, err)
		if state == "result_unconfirmed" {
			s.recoverPublication(e)
		}
		return
	}
	commit, files, err := dao.ReadInventorySnapshot(e.request.key(), result.Commit)
	if err != nil || commit != result.Commit || digestJSON(files) != e.status.ManifestDigest {
		_ = s.transition(e, "result_unconfirmed", fmt.Errorf("publication returned but target readback did not match"))
		s.recoverPublication(e)
		return
	}
	s.mu.Lock()
	e.status.Commit = result.Commit
	s.mu.Unlock()
	if err = s.transition(e, "completed", nil); err != nil {
		s.mu.Lock()
		e.status.State = "result_unconfirmed"
		e.status.Error = "published but receipt persistence failed"
		e.status.SnapshotVersion++
		s.mu.Unlock()
	}
}

func (s *RepositorySyncService) obtain(ctx context.Context, e *syncEntry, f dao.LocalManifestFile) error {
	k := e.request.key()
	available, err := s.upload.SyncBlobAvailable(k, f)
	if err != nil {
		return err
	}
	if available {
		return nil
	}
	var sources []SyncSource
	if f.Size > 0 {
		sources, err = s.resolve(ctx, k, f)
		if err != nil {
			return err
		}
	}
	urls := make([]string, 0, len(sources))
	for _, source := range sources {
		base, err := syncBaseURL(source.ManagementURL)
		if err != nil {
			continue
		}
		q := url.Values{"repo": {k.Repo}, "path": {f.Path}, "sha256": {f.Sha256}, "size": {strconv.FormatInt(f.Size, 10)}}
		urls = append(urls, base+"/api/repository-sync/"+url.PathEscape(k.RepoType)+"/"+url.PathEscape(k.Namespace)+"/content?"+q.Encode())
	}
	if len(urls) == 0 && f.Size > 0 {
		return fmt.Errorf("no available node copy; uploaded repositories cannot fetch an origin")
	}
	return s.upload.DownloadSyncBlob(ctx, k, f, urls, func(n int64) {
		e.persist.Lock()
		defer e.persist.Unlock()
		s.mu.Lock()
		if s.entries[e.request.TaskID] == e {
			e.status.TransferredBytes += n
			e.status.SnapshotVersion++
		}
		s.mu.Unlock()
	})
}

func syncBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("invalid registered service endpoint")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func (s *RepositorySyncService) schedulerSources(ctx context.Context, k repository.RepoKey, f dao.LocalManifestFile) ([]SyncSource, error) {
	reg := config.SysConfig.Registration()
	if !reg.Enabled {
		return nil, fmt.Errorf("node registration is disabled")
	}
	base, err := syncBaseURL(reg.HTTPURL)
	if err != nil {
		return nil, err
	}
	q := url.Values{"namespace": {k.Namespace}, "repoType": {k.RepoType}, "repo": {k.Repo}, "path": {f.Path}, "sha256": {f.Sha256}, "size": {strconv.FormatInt(f.Size, 10)}, "excludeInstanceId": {reg.NodeID}}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/upload-inventory/sources?"+q.Encode(), nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("source lookup returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Items []SyncSource `json:"items"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err = dec.Decode(&out); err != nil {
		return nil, err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("invalid source response")
	}
	return out.Items, nil
}

func (u *UploadService) CopySyncContent(k repository.RepoKey, f dao.LocalManifestFile, w io.Writer) error {
	if err := validateRepoLocator(k.RepoType, k.Namespace, k.Repo, "main"); err != nil {
		return err
	}
	if err := validateManifestList([]dao.LocalManifestFile{f}); err != nil {
		return err
	}
	return u.uploadDao.CopySyncContent(k, f, w)
}
