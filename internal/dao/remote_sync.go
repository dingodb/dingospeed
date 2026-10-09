package dao

import (
	"context"
	"crypto/sha256"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/hfprojection"
	"dingospeed/pkg/proto/manager"
	"dingospeed/pkg/repository"
	"dingospeed/pkg/util"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type RemoteSyncFile struct {
	Path                 string `json:"path"`
	OID                  string `json:"oid"`
	Size                 int64  `json:"size"`
	IdentityKind         string `json:"identityKind"`
	UpstreamFileRevision string `json:"upstreamFileRevision,omitempty"`
}
type RemoteSyncManifest struct {
	Commit string           `json:"commit"`
	Files  []RemoteSyncFile `json:"files"`
}

func ValidateRemoteManifest(k repository.RepoKey, m *RemoteSyncManifest) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if k.Namespace != repository.HuggingFace && k.Namespace != repository.ModelScope {
		return fmt.Errorf("not a remote repository")
	}
	if m == nil || len(m.Files) > config.SysConfig.GetUploadPublishMaxFiles() || repository.Segment(m.Commit) != nil || len(m.Commit) < 7 || strings.Trim(m.Commit, "0123456789abcdef") != "" {
		return fmt.Errorf("invalid fixed remote commit")
	}
	seen := map[string]bool{}
	var total int64
	for _, f := range m.Files {
		if repository.Relative(f.Path) != nil || seen[f.Path] || f.Size < 0 {
			return fmt.Errorf("invalid or duplicate remote file")
		}
		seen[f.Path] = true
		if f.Size > math.MaxInt64-total {
			return fmt.Errorf("manifest size overflow")
		}
		total += f.Size
		if (k.Namespace == repository.HuggingFace && f.IdentityKind == "provider-sha1") || (k.Namespace == repository.ModelScope && f.IdentityKind == "git-blob-sha1") {
			return fmt.Errorf("provider identity mismatch")
		}
		n := 64
		if f.IdentityKind == "git-blob-sha1" || f.IdentityKind == "provider-sha1" {
			n = 40
		} else if f.IdentityKind != "sha256" {
			return fmt.Errorf("unsupported content identity")
		}
		if len(f.OID) != n || strings.Trim(f.OID, "0123456789abcdef") != "" {
			return fmt.Errorf("invalid native content identity")
		}
		if f.UpstreamFileRevision != "" && repository.Segment(f.UpstreamFileRevision) != nil {
			return fmt.Errorf("invalid file revision")
		}
	}
	return nil
}
func nativeKind(k repository.RepoKey, oid string) string {
	if len(oid) == 64 {
		return "sha256"
	}
	if k.Namespace == repository.HuggingFace {
		return "git-blob-sha1"
	}
	return "provider-sha1"
}
func remoteMetaPath(k repository.RepoKey, revision string) string {
	return filepath.Join(k.Revision(config.SysConfig.Repos(), revision), "meta_get.json")
}
func ReadRemoteSyncManifest(k repository.RepoKey, revision string) (RemoteSyncManifest, error) {
	out := RemoteSyncManifest{Files: []RemoteSyncFile{}}
	if k.Validate() != nil || repository.Segment(revision) != nil {
		return out, fmt.Errorf("invalid remote locator")
	}
	path := remoteMetaPath(k, revision)
	if err := repository.SafePath(config.SysConfig.Repos(), path); err != nil {
		return out, err
	}
	body, err := readCacheContent(path)
	if err != nil {
		return out, err
	}
	var meta struct {
		Sha      string `json:"sha"`
		Siblings []struct {
			Path         string `json:"rfilename"`
			Size         int64  `json:"size"`
			OID          string `json:"blobId"`
			FileRevision string `json:"fileRevision"`
		} `json:"siblings"`
	}
	if err = json.Unmarshal(body, &meta); err != nil {
		return out, err
	}
	out.Commit = meta.Sha
	if repository.Segment(out.Commit) != nil {
		return out, fmt.Errorf("invalid cached commit")
	}
	for _, f := range meta.Siblings {
		if f.OID != "" {
			out.Files = append(out.Files, RemoteSyncFile{Path: f.Path, OID: f.OID, Size: f.Size, IdentityKind: nativeKind(k, f.OID), UpstreamFileRevision: f.FileRevision})
			continue
		}
		path := filepath.Join(k.PathsInfo(config.SysConfig.Repos(), out.Commit, f.Path), "paths-info_post.json")
		if repository.Relative(f.Path) != nil {
			return out, fmt.Errorf("unsafe remote path")
		}
		if err := repository.SafePath(config.SysConfig.Repos(), path); err != nil {
			return out, err
		}
		raw, e := readCacheContent(path)
		var infos []common.PathsInfo
		if e != nil || json.Unmarshal(raw, &infos) != nil || len(infos) != 1 {
			return out, fmt.Errorf("fixed file metadata unavailable: %s", f.Path)
		}
		info := infos[0]
		oid := info.Oid
		if info.Lfs.Oid != "" {
			oid = info.Lfs.Oid
		}
		out.Files = append(out.Files, RemoteSyncFile{Path: f.Path, OID: oid, Size: info.Size, IdentityKind: nativeKind(k, oid), UpstreamFileRevision: f.FileRevision})
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out, ValidateRemoteManifest(k, &out)
}
func (u *UploadDao) ResolveHFManifest(ctx context.Context, k repository.RepoKey, revision, authorization string) (RemoteSyncManifest, error) {
	out := RemoteSyncManifest{Files: []RemoteSyncFile{}}
	resp, err := u.fileDao.RemoteRequestMetaContext(ctx, consts.RequestTypeGet, k.RepoType, k.ID(), revision, authorization)
	if err != nil {
		return out, err
	}
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("upstream metadata HTTP %d", resp.StatusCode)
	}
	var meta CommitHfSha
	if err = json.Unmarshal(resp.Body, &meta); err != nil {
		return out, err
	}
	out.Commit = meta.Sha
	paths := []string{}
	for _, f := range meta.Siblings {
		if repository.Relative(f.Rfilename) != nil {
			return out, fmt.Errorf("unsafe file")
		}
		paths = append(paths, f.Rfilename)
	}
	upstream, err := UpstreamRepo(k.RepoType, k.ID())
	if err != nil {
		return out, err
	}
	uri := "/api/" + k.RepoType + "/" + repository.EscapeURLPath(upstream) + "/paths-info/" + url.PathEscape(out.Commit)
	for start := 0; start < len(paths); start += 128 {
		resp, err := u.fileDao.requestFilePathInfoContext(ctx, uri, authorization, paths[start:min(start+128, len(paths))])
		if err != nil {
			return out, err
		}
		var infos []common.PathsInfo
		if err = json.Unmarshal(resp.Body, &infos); err != nil {
			return out, err
		}
		for _, info := range infos {
			oid := info.Oid
			if info.Lfs.Oid != "" {
				oid = info.Lfs.Oid
			}
			out.Files = append(out.Files, RemoteSyncFile{Path: info.Path, OID: oid, Size: info.Size, IdentityKind: nativeKind(k, oid)})
		}
	}
	expected := map[string]bool{}
	for _, path := range paths {
		expected[path] = true
	}
	for _, f := range out.Files {
		if !expected[f.Path] {
			return out, fmt.Errorf("upstream returned unexpected file")
		}
	}
	if len(out.Files) != len(paths) {
		return out, fmt.Errorf("upstream returned incomplete manifest")
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out, ValidateRemoteManifest(k, &out)
}
func (u *UploadDao) RemoteBlobAvailable(k repository.RepoKey, f RemoteSyncFile) (bool, error) {
	path := BlobPath(k.RepoType, k.ID(), f.OID)
	if err := repository.SafePath(config.SysConfig.Repos(), path); err != nil {
		return false, err
	}
	return syncCacheComplete(path, f.Size)
}
func (u *UploadDao) DownloadRemoteSyncFile(ctx context.Context, k repository.RepoKey, commit string, f RemoteSyncFile, uri string, source *downloader.RemoteSource, onBytes func(int64)) error {
	if okay, err := u.RemoteBlobAvailable(k, f); err != nil {
		return err
	} else if okay {
		return nil
	}
	if err := util.MakeDirs(BlobPath(k.RepoType, k.ID(), f.OID)); err != nil {
		return err
	}
	if f.Size == 0 {
		cache, err := downloader.NewDingCache(BlobPath(k.RepoType, k.ID(), f.OID), config.SysConfig.Download.BlockSize)
		if err != nil {
			return err
		}
		return cache.Close()
	}
	d := u.fileDao.downloaderDao
	if d == nil {
		return fmt.Errorf("remote downloader is not configured")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &downloader.TaskParam{Context: ctx, Cancel: cancel, RepoKey: k, DataType: k.RepoType, OrgRepo: k.ID(), Revision: commit, FileName: f.Path, FileSize: f.Size, Etag: f.OID, Uri: uri, Source: source, BlobsFile: BlobPath(k.RepoType, k.ID(), f.OID), Unordered: true, ResponseChan: make(chan []byte, 4), CacheResult: make(chan error, 1), NativeSync: true, OnNetworkBytes: onBytes}
	if err := d.FileDownload(0, f.Size, false, p); err != nil {
		return err
	}
	for range p.ResponseChan {
	}
	return <-p.CacheResult
}
func (u *UploadDao) PublishRemoteSync(k repository.RepoKey, revision, base string, absent bool, baseDigest string, m RemoteSyncManifest, publication string) error {
	if err := ValidateRemoteManifest(k, &m); err != nil {
		return err
	}
	// A commit is immutable: creating its cache metadata may never change a
	// previously visible commit. Existing metadata can only be reused.
	immutablePath := remoteMetaPath(k, m.Commit)
	fileReferenceLocks.Lock(immutablePath)
	defer fileReferenceLocks.Unlock(immutablePath)
	existing, readErr := ReadRemoteSyncManifest(k, m.Commit)
	if readErr == nil && RemoteManifestDigest(existing) != RemoteManifestDigest(m) {
		return remoteRevisionConflict()
	}
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	siblings := []map[string]any{}
	for _, f := range m.Files {
		good, err := u.RemoteBlobAvailable(k, f)
		if err != nil {
			return err
		}
		if !good {
			return fmt.Errorf("remote cache incomplete: %s", f.Path)
		}
		info := &common.PathsInfo{Type: "file", Path: f.Path, Size: f.Size, Oid: f.OID}
		if f.IdentityKind == "sha256" {
			info.Lfs = common.Lfs{Oid: f.OID, Size: f.Size}
		}
		if err = u.fileDao.cacheSyncImmutableFileInfo(k, m.Commit, info); err != nil {
			return err
		}
		if err = u.fileDao.ConstructBlobsAndFileFile(BlobPath(k.RepoType, k.ID(), f.OID), ResolvePath(k.RepoType, k.ID(), m.Commit, f.Path)); err != nil {
			return err
		}
		siblings = append(siblings, map[string]any{"rfilename": f.Path, "size": f.Size, "blobId": f.OID, "fileRevision": f.UpstreamFileRevision})
	}
	body, _ := json.Marshal(map[string]any{"sha": m.Commit, "siblings": siblings, "id": k.ID()})
	headers := map[string]string{"x-repo-commit": m.Commit, "x-sync-publication-id": publication}
	if revision != m.Commit && errors.Is(readErr, os.ErrNotExist) {
		if err := u.fileDao.WriteCacheRequest(immutablePath, 200, map[string]string{"x-repo-commit": m.Commit}, body); err != nil {
			return err
		}
	}
	path := remoteMetaPath(k, revision)
	lock := u.fileDao.lockDao.getMetaFileLock(path)
	lock.Lock()
	defer lock.Unlock()
	old, err := readCacheContent(path)
	if absent {
		if err == nil {
			return remoteRevisionConflict()
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		var current struct {
			Sha string `json:"sha"`
		}
		if err != nil || json.Unmarshal(old, &current) != nil || current.Sha != base {
			return remoteRevisionConflict()
		}
	}
	if !absent {
		current, e := ReadRemoteSyncManifest(k, revision)
		if e != nil || RemoteManifestDigest(current) != baseDigest {
			return remoteRevisionConflict()
		}
	}
	if err = repository.Register(config.SysConfig.Repos(), repository.Remote(k)); err != nil {
		return err
	}
	if err = util.MakeDirs(path); err != nil {
		return err
	}
	cached := common.CacheContent{Version: consts.VersionSnapshot, StatusCode: 200, Headers: headers, Content: fmt.Sprintf("%x", body)}
	if err = util.WriteDataToFileAtomic(path, cached); err != nil {
		return RemotePublicationUncertain{err}
	}
	InvalidateRemoteSyncIndex(k)
	if err = util.WriteDataToFileAtomic(remoteReceiptPath(k, publication), map[string]string{"publication": publication, "revision": revision, "commit": m.Commit, "manifestDigest": RemoteManifestDigest(m)}); err != nil {
		return RemotePublicationUncertain{err}
	}

	return nil
}
func remoteRevisionConflict() error {
	return localUploadError{status: 409, code: "REVISION_CHANGED", msg: "remote target revision changed"}
}
func (u *UploadDao) RemotePublicationMatches(k repository.RepoKey, revision, publication string, expected RemoteSyncManifest) (bool, error) {
	b, err := os.ReadFile(remoteReceiptPath(k, publication))
	if err == nil {
		var receipt map[string]string
		if json.Unmarshal(b, &receipt) != nil || receipt["publication"] != publication || receipt["revision"] != revision || receipt["commit"] != expected.Commit || receipt["manifestDigest"] != RemoteManifestDigest(expected) {
			return false, fmt.Errorf("invalid native publication receipt")
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	c, err := u.fileDao.ReadCacheRequest(remoteMetaPath(k, revision))
	if err != nil {
		return false, err
	}
	return c.Headers["x-sync-publication-id"] == publication && c.Headers["x-repo-commit"] == expected.Commit, nil
}
func InvalidateRemoteSyncIndex(k repository.RepoKey) {
	if k.Namespace == repository.HuggingFace {
		hfprojection.DefaultIndex.InvalidateRepository(config.SysConfig.Repos(), k.RepoType, k.Repo)
	}
}

func CopyRemoteSyncContent(ctx context.Context, k repository.RepoKey, commit string, f RemoteSyncFile, start, end int64, w io.Writer) error {
	files, err := cachedRemoteContentManifest(k, commit)
	if err != nil {
		return err
	}
	entry, found := files[f.Path]
	if !found {
		return fmt.Errorf("fixed content not referenced")
	}
	if entry.OID == "" {
		infoPath := filepath.Join(k.PathsInfo(config.SysConfig.Repos(), commit, f.Path), "paths-info_post.json")
		if err := repository.SafePath(config.SysConfig.Repos(), infoPath); err != nil {
			return err
		}
		raw, err := readCacheContent(infoPath)
		if err != nil {
			return err
		}
		var infos []common.PathsInfo
		if json.Unmarshal(raw, &infos) != nil || len(infos) != 1 {
			return fmt.Errorf("file metadata unavailable")
		}
		entry.OID = infos[0].Oid
		if infos[0].Lfs.Oid != "" {
			entry.OID = infos[0].Lfs.Oid
		}
		entry.Size = infos[0].Size
	}
	if entry.OID != f.OID || entry.Size != f.Size {
		return fmt.Errorf("fixed content not referenced")
	}
	path := BlobPath(k.RepoType, k.ID(), f.OID)
	if err = repository.SafePath(config.SysConfig.Repos(), path); err != nil {
		return err
	}
	good, err := syncCacheComplete(path, f.Size)
	if err != nil {
		return err
	}
	if !good {
		return fmt.Errorf("remote content incomplete")
	}
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()
	h := &downloader.DingCacheHeader{}
	if err = h.Read(input); err != nil {
		return err
	}
	_, err = io.CopyN(w, syncContextReader{ctx, io.NewSectionReader(input, h.GetHeaderSize()+start, end-start)}, end-start)
	return err
}

func RemoteManifestDigest(m RemoteSyncManifest) string {
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func remoteReceiptPath(k repository.RepoKey, publication string) string {
	return filepath.Join(k.APIRoot(config.SysConfig.Repos()), "sync-native-receipts", publication+".json")
}
func (f *FileDao) cacheSyncImmutableFileInfo(k repository.RepoKey, commit string, info *common.PathsInfo) error {
	path := filepath.Join(k.PathsInfo(config.SysConfig.Repos(), commit, info.Path), "paths-info_post.json")
	fileReferenceLocks.Lock(path)
	defer fileReferenceLocks.Unlock(path)
	if err := repository.SafePath(config.SysConfig.Repos(), path); err != nil {
		return err
	}
	if b, err := readCacheContent(path); err == nil {
		var old []common.PathsInfo
		if json.Unmarshal(b, &old) != nil || len(old) != 1 {
			return fmt.Errorf("invalid existing native metadata")
		}
		oid := old[0].Oid
		if old[0].Lfs.Oid != "" {
			oid = old[0].Lfs.Oid
		}
		if oid != info.Oid || old[0].Size != info.Size || old[0].Path != info.Path {
			return remoteRevisionConflict()
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, _ := json.Marshal([]*common.PathsInfo{info})
	return f.WriteCacheRequest(path, 200, map[string]string{"x-repo-commit": commit}, body)
}

type RemotePublicationUncertain struct{ Err error }

func (e RemotePublicationUncertain) Error() string { return e.Err.Error() }
func (e RemotePublicationUncertain) Unwrap() error { return e.Err }

// Immutable manifests are shared by range readers. The targeted metadata stamp
// invalidates this bounded cache when a provider refresh writes a new envelope.
var remoteContentManifests = struct {
	sync.Mutex
	entries map[string]remoteContentManifest
}{entries: map[string]remoteContentManifest{}}

type remoteContentManifest struct {
	size     int64
	modified time.Time
	manifest map[string]RemoteSyncFile
}

func cachedRemoteContentManifest(k repository.RepoKey, commit string) (map[string]RemoteSyncFile, error) {
	path := remoteMetaPath(k, commit)
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	remoteContentManifests.Lock()
	entry, ok := remoteContentManifests.entries[path]
	remoteContentManifests.Unlock()
	if ok && entry.size == stat.Size() && entry.modified.Equal(stat.ModTime()) {
		return entry.manifest, nil
	}
	raw, err := readCacheContent(path)
	if err != nil {
		return nil, err
	}
	var meta struct {
		Sha      string `json:"sha"`
		Siblings []struct {
			Path string `json:"rfilename"`
			OID  string `json:"blobId"`
			Size int64  `json:"size"`
		} `json:"siblings"`
	}
	if json.Unmarshal(raw, &meta) != nil || meta.Sha != commit {
		return nil, fmt.Errorf("fixed manifest unavailable")
	}
	files := make(map[string]RemoteSyncFile, len(meta.Siblings))
	for _, f := range meta.Siblings {
		if repository.Relative(f.Path) != nil {
			return nil, fmt.Errorf("unsafe cached path")
		}
		files[f.Path] = RemoteSyncFile{Path: f.Path, OID: f.OID, Size: f.Size}
	}
	remoteContentManifests.Lock()
	defer remoteContentManifests.Unlock()
	if len(remoteContentManifests.entries) >= 128 {
		remoteContentManifests.entries = map[string]remoteContentManifest{}
	}
	remoteContentManifests.entries[path] = remoteContentManifest{stat.Size(), stat.ModTime(), files}
	return files, nil
}

// Preview source lookup uses only scheduler metadata and local cache headers.
// It never starts a range transfer or mutates a revision.
func (u *UploadDao) RemoteSyncSourceAvailable(k repository.RepoKey, f RemoteSyncFile) (bool, error) {
	if okay, err := u.RemoteBlobAvailable(k, f); err != nil || okay {
		return okay, err
	}
	if f.Size == 0 || config.SysConfig.Online() {
		return true, nil
	}
	if u.fileDao.downloaderDao == nil {
		return false, nil
	}
	source, err := u.fileDao.downloaderDao.getRequestDomainScheduler(k.RepoType, k.ID(), f.Path, f.OID, 0, f.Size, f.Size)
	if err != nil {
		return false, err
	}
	return source.SchedulerType == consts.SchedulerYes && source.MaxOffset >= f.Size, nil
}

// ReportRemoteSyncSources explicitly refreshes only complete files from one
// already-fixed manifest. It does not change repository metadata or read bodies.
func (u *UploadDao) ReportRemoteSyncSources(k repository.RepoKey, m RemoteSyncManifest) (int, error) {
	if err := ValidateRemoteManifest(k, &m); err != nil {
		return 0, err
	}
	d := u.fileDao.downloaderDao
	if d == nil || d.schedulerDao == nil || d.schedulerDao.Client == nil {
		return 0, fmt.Errorf("scheduler source reporting unavailable")
	}
	entries := []*manager.FileProcessEntry{}
	for _, f := range m.Files {
		complete, err := u.RemoteBlobAvailable(k, f)
		if err != nil || !complete {
			continue
		}
		entries = append(entries, &manager.FileProcessEntry{DataType: k.RepoType, Org: k.Namespace, Repo: k.Repo, Name: f.Path, Etag: f.OID, FileSize: f.Size, InstanceId: config.SysConfig.Registration().NodeID, StartPos: f.Size, EndPos: f.Size, Status: consts.StatusDownloaded})
	}
	if len(entries) == 0 {
		return 0, nil
	}
	if err := d.schedulerDao.SyncFileProcess(&manager.SyncFileProcessReq{FileProcessEntries: entries}); err != nil {
		return 0, err
	}
	return len(entries), nil
}
