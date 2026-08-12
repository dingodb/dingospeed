package dao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/util"

	"github.com/bytedance/sonic"
	"go.uber.org/zap"
)

type UploadDao struct {
	fileDao *FileDao
	lockDao *LockDao
}

type LocalUploadParam struct {
	RepoType  string
	Org       string
	Repo      string
	Revision  string
	FilePath  string
	Size      int64
	Sha256    string
	Overwrite bool
	Start     *int64
}

type LocalUploadResult struct {
	RepoType string `json:"repoType"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	Commit   string `json:"commit"`
	FilePath string `json:"filePath"`
	Size     int64  `json:"size"`
	Sha256   string `json:"sha256"`
	Status   string `json:"status"`
	// BlobReused 为 true 表示本次未重复写入内容，走的是 FR-2 6.2.5 的幂等快路径。
	BlobReused bool `json:"blobReused"`
}

type LocalUploadProgress struct {
	RepoType     string `json:"repoType"`
	Repo         string `json:"repo"`
	Revision     string `json:"revision"`
	FilePath     string `json:"filePath"`
	Size         int64  `json:"size"`
	Sha256       string `json:"sha256"`
	ResumeOffset int64  `json:"resumeOffset"`
	Effective    bool   `json:"effective"`
	BlobComplete bool   `json:"blobComplete"`
	Status       string `json:"status"`
}

// LocalManifestFile 是本地仓库某个快照下的一条文件记录。
// 这份清单是本地仓库唯一的文件级元数据来源：整仓文件树、单文件信息、
// 快照标识都从它派生，因此不再为每个 commit 逐文件落一份 paths-info。
type LocalManifestFile struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

const localManifestFileName = "dingo-local-manifest.json"

func LocalManifestPath(repoType, orgRepo, commit string) string {
	return filepath.Join(config.SysConfig.Repos(), "api", repoType, orgRepo, "revision", commit, localManifestFileName)
}

func NewUploadDao(fileDao *FileDao, lockDao *LockDao) *UploadDao {
	return &UploadDao{fileDao: fileDao, lockDao: lockDao}
}

// localUploadStageSuffix 是内容落到最终 blob 位置之前的暂存文件后缀。
// 暂存 + 原子改名保证：摘要校验没通过的内容永远不会出现在最终位置上，
// 因此“blob 存在且块位图完整”可以被安全地当作“内容与其 sha 名一致”。
const localUploadStageSuffix = ".uploading"

var localUploadInFlight = struct {
	sync.Mutex
	keys map[string]struct{}
}{keys: map[string]struct{}{}}

// 上传期间的互斥必须自己持有，不能放进带 TTL 的缓存里：
// 大文件上传远超缓存过期时间，锁对象一旦过期就会被重建，互斥会静默失效。
var (
	uploadRevisionLocks = newKeyedMutex()
	uploadBlobLocks     = newKeyedMutex()
)

type keyedMutexEntry struct {
	mu   sync.Mutex
	refs int
}

type keyedMutex struct {
	mu      sync.Mutex
	entries map[string]*keyedMutexEntry
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{entries: make(map[string]*keyedMutexEntry)}
}

func (k *keyedMutex) Lock(key string) {
	k.mu.Lock()
	entry, ok := k.entries[key]
	if !ok {
		entry = &keyedMutexEntry{}
		k.entries[key] = entry
	}
	entry.refs++
	k.mu.Unlock()
	entry.mu.Lock()
}

func (k *keyedMutex) Unlock(key string) {
	k.mu.Lock()
	entry, ok := k.entries[key]
	if !ok {
		k.mu.Unlock()
		return
	}
	entry.refs--
	if entry.refs <= 0 {
		delete(k.entries, key)
	}
	k.mu.Unlock()
	entry.mu.Unlock()
}

func IsLocalOrgRepo(orgRepo string) bool {
	namespace := "dingo-local"
	if config.SysConfig != nil && config.SysConfig.Upload.Namespace != "" {
		namespace = config.SysConfig.Upload.Namespace
	}
	return orgRepo == namespace || strings.HasPrefix(orgRepo, namespace+"/")
}

func (u *UploadDao) UploadWholeFile(param LocalUploadParam, body io.Reader) (*LocalUploadResult, error) {
	orgRepo := util.GetOrgRepo(param.Org, param.Repo)
	fileKey := fmt.Sprintf("%s:%s:%s:%s", param.RepoType, orgRepo, param.Revision, param.FilePath)
	if !tryEnterLocalUpload(fileKey) {
		return nil, localUploadError{status: http.StatusConflict, code: "UPLOAD_FILE_BUSY", msg: "same file is already being uploaded"}
	}
	defer leaveLocalUpload(fileKey)

	revisionLockKey := fmt.Sprintf("upload-revision:%s:%s:%s", param.RepoType, orgRepo, param.Revision)

	// 第一段临界区：只读当前清单做冲突/幂等判定，判定完立即释放，
	// 这样同一版本下的不同文件可以并行写内容（FR-7 6.7.2.2）。
	fastPath, err := u.precheck(revisionLockKey, param, orgRepo)
	if err != nil {
		return nil, err
	}
	if fastPath != nil {
		return fastPath, nil
	}

	// 内容落盘不持有版本锁，只持有 blob 级互斥。
	reused, err := u.materializeBlob(param, orgRepo, body)
	if err != nil {
		return nil, err
	}

	// 第二段临界区：重建清单、算快照标识、写元数据。必须重新读取清单，
	// 否则并发上传的另一个文件会被基于陈旧清单的重建覆盖掉（坑 8）。
	uploadRevisionLocks.Lock(revisionLockKey)
	defer uploadRevisionLocks.Unlock(revisionLockKey)

	_, currentManifest := u.currentManifest(param)
	if existing, ok := findManifestFile(currentManifest, param.FilePath); ok && !sameManifestContent(existing, param) {
		if param.Start != nil {
			return nil, fullOverwriteRequiredError()
		}
		if !param.Overwrite {
			return nil, overwriteRequiredError()
		}
	}
	commit, manifest, err := u.nextCommit(currentManifest, LocalManifestFile{
		Path:   param.FilePath,
		Sha256: param.Sha256,
		Size:   param.Size,
	})
	if err != nil {
		return nil, err
	}
	if err = u.writeEffectiveMetadata(param, commit, manifest); err != nil {
		return nil, err
	}
	return &LocalUploadResult{
		RepoType:   param.RepoType,
		Repo:       orgRepo,
		Revision:   param.Revision,
		Commit:     commit,
		FilePath:   param.FilePath,
		Size:       param.Size,
		Sha256:     param.Sha256,
		Status:     "effective",
		BlobReused: reused,
	}, nil
}

// precheck 在版本锁下做覆盖冲突判定与幂等快路径判定。
// 返回非 nil 的结果表示本次上传无需写入任何内容。
func (u *UploadDao) precheck(revisionLockKey string, param LocalUploadParam, orgRepo string) (*LocalUploadResult, error) {
	uploadRevisionLocks.Lock(revisionLockKey)
	defer uploadRevisionLocks.Unlock(revisionLockKey)

	currentCommit, currentManifest := u.currentManifest(param)
	existing, ok := findManifestFile(currentManifest, param.FilePath)
	if !ok {
		return nil, nil
	}
	if !sameManifestContent(existing, param) {
		if param.Start != nil {
			return nil, fullOverwriteRequiredError()
		}
		if !param.Overwrite {
			return nil, overwriteRequiredError()
		}
		return nil, nil
	}
	// 清单说这个文件已经生效了，还要确认内容确实完好地躺在磁盘上，
	// 否则会返回成功但下载不到（人工删除、外部清理都可能造成这种状态）。
	if !blobIsComplete(localBlobPath(param.RepoType, orgRepo, param.Sha256), param.Size) {
		return nil, nil
	}
	return &LocalUploadResult{
		RepoType:   param.RepoType,
		Repo:       orgRepo,
		Revision:   param.Revision,
		Commit:     currentCommit,
		FilePath:   param.FilePath,
		Size:       param.Size,
		Sha256:     param.Sha256,
		Status:     "already_exists",
		BlobReused: true,
	}, nil
}

// materializeBlob 把请求体写成一个通过完整性校验的 blob。
// 返回值表示是否复用了已存在的内容（未重复写入）。
func (u *UploadDao) materializeBlob(param LocalUploadParam, orgRepo string, body io.Reader) (bool, error) {
	repos := config.SysConfig.Repos()
	blobPath := localBlobPath(param.RepoType, orgRepo, param.Sha256)
	if err := ensureLocalUploadPathSafe(repos, blobPath); err != nil {
		return false, err
	}

	// blob 位置只由 sha 决定，与文件路径、版本标签无关，
	// 因此互斥必须建在 blob 上，否则同内容的两个上传会互相踩写。
	blobKey := fmt.Sprintf("upload-blob:%s:%s:%s", param.RepoType, orgRepo, param.Sha256)
	uploadBlobLocks.Lock(blobKey)
	defer uploadBlobLocks.Unlock(blobKey)

	if blobIsComplete(blobPath, param.Size) {
		return true, nil
	}
	if err := util.MakeDirs(blobPath); err != nil {
		return false, err
	}
	stagePath := blobPath + localUploadStageSuffix
	if param.Start == nil {
		// A request without start means a full-file upload. It must overwrite any
		// stale staged content for this sha instead of trying to append to it.
		if err := os.Remove(stagePath); err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}
	if err := writeStagedBlob(stagePath, param, body); err != nil {
		return false, err
	}
	if err := os.Rename(stagePath, blobPath); err != nil {
		return false, err
	}
	return false, nil
}

// writeStagedBlob 把请求体写入暂存文件并对落盘内容整体重算摘要。
// 摘要按 §3.3.1 的要求在内容完整之后重读一遍算出，不做流式累加。
func writeStagedBlob(stagePath string, param LocalUploadParam, body io.Reader) error {
	dingFile, err := downloader.NewDingCache(stagePath, config.SysConfig.Download.BlockSize)
	if err != nil {
		return err
	}
	if boundSize := dingFile.GetFileSize(); boundSize != 0 && boundSize != param.Size {
		dingFile.Close()
		if param.Start != nil {
			if rmErr := os.Remove(stagePath); rmErr != nil && !os.IsNotExist(rmErr) {
				zap.S().Warnf("remove incompatible staged upload file %s failed: %v", stagePath, rmErr)
			}
			return localUploadError{
				status: http.StatusConflict,
				code:   "UPLOAD_RESUME_BINDING_MISMATCH",
				msg:    fmt.Sprintf("staged upload is bound to size %d, declared size is %d", boundSize, param.Size),
			}
		}
		if rmErr := os.Remove(stagePath); rmErr != nil && !os.IsNotExist(rmErr) {
			return rmErr
		}
		dingFile, err = downloader.NewDingCache(stagePath, config.SysConfig.Download.BlockSize)
		if err != nil {
			return err
		}
	}
	if param.Size > 0 {
		if err = dingFile.Resize(param.Size); err != nil {
			dingFile.Close()
			return err
		}
	}
	start := int64(0)
	if param.Start != nil {
		start = *param.Start
		resumeOffset, offsetErr := resumableOffset(dingFile)
		if offsetErr != nil {
			dingFile.Close()
			return offsetErr
		}
		if start != resumeOffset {
			dingFile.Close()
			return resumeOffsetError(start, resumeOffset)
		}
	}
	if err = writeBodyToDingCache(dingFile, body, start, param.Size); err != nil {
		dingFile.Close()
		return err
	}
	if err = dingFile.Close(); err != nil {
		return err
	}
	actualHash, err := hashDingCachePayload(stagePath, param.Size)
	if err != nil {
		return err
	}
	if actualHash != param.Sha256 {
		return badLocalUploadRequest("sha256 mismatch: declared %s, actual %s", param.Sha256, actualHash)
	}
	return nil
}

// blobIsComplete 判断最终位置上的 blob 是否是一份完整可服务的内容。
// 因为写入走暂存 + 原子改名，出现在最终位置的内容必然已通过摘要校验，
// 所以这里只需检查大小与块位图，不必再读一遍全文。
func blobIsComplete(blobPath string, size int64) bool {
	complete, _, err := inspectCompleteBlob(blobPath, size)
	return err == nil && complete
}

func inspectCompleteBlob(blobPath string, expectedSize int64) (bool, int64, error) {
	if !util.FileExists(blobPath) {
		return false, 0, nil
	}
	dingFile, err := downloader.NewDingCache(blobPath, config.SysConfig.Download.BlockSize)
	if err != nil {
		return false, 0, err
	}
	defer dingFile.Close()
	size := dingFile.GetFileSize()
	if expectedSize >= 0 && size != expectedSize {
		return false, size, nil
	}
	offset, err := resumableOffset(dingFile)
	return offset == size, size, err
}

func localBlobPath(repoType, orgRepo, sha256 string) string {
	return filepath.Join(config.SysConfig.Repos(), "files", repoType, orgRepo, "blobs", sha256)
}

func sameManifestContent(existing LocalManifestFile, param LocalUploadParam) bool {
	return existing.Sha256 == param.Sha256 && existing.Size == param.Size
}

func overwriteRequiredError() localUploadError {
	return localUploadError{
		status: http.StatusConflict,
		code:   "UPLOAD_OVERWRITE_REQUIRED",
		msg:    "file already exists with different content; set overwrite=true to replace it",
	}
}

func fullOverwriteRequiredError() localUploadError {
	return localUploadError{
		status: http.StatusConflict,
		code:   "UPLOAD_FULL_OVERWRITE_REQUIRED",
		msg:    "file already exists with different content; retry as a full upload with overwrite=true",
	}
}

func writeBodyToDingCache(dingFile *downloader.DingCache, body io.Reader, start, declaredSize int64) error {
	blockSize := dingFile.GetBlockSize()
	buf := make([]byte, blockSize)
	total := start
	for blockIndex := start / blockSize; total < declaredSize; blockIndex++ {
		remaining := declaredSize - total
		want := blockSize
		if remaining < want {
			want = remaining
		}
		n, err := io.ReadFull(body, buf[:want])
		total += int64(n)
		if err != nil {
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				return badLocalUploadRequest("body shorter than declared size: declared %d, received %d", declaredSize, total)
			}
			return err
		}
		block := buf[:blockSize]
		if want < blockSize {
			clear(block[want:])
		}
		if err = dingFile.WriteBlock(blockIndex, block); err != nil {
			return err
		}
	}
	extra := make([]byte, 1)
	if n, err := body.Read(extra); n > 0 || (err != nil && err != io.EOF) {
		if n > 0 {
			return badLocalUploadRequest("body longer than declared size: declared %d", declaredSize)
		}
		return err
	}
	return nil
}

func resumableOffset(dingFile *downloader.DingCache) (int64, error) {
	size := dingFile.GetFileSize()
	blockSize := dingFile.GetBlockSize()
	if blockSize <= 0 {
		return 0, fmt.Errorf("invalid cache block size: %d", blockSize)
	}
	blockNum := (size + blockSize - 1) / blockSize
	for i := int64(0); i < blockNum; i++ {
		has, err := dingFile.HasBlock(i)
		if err != nil {
			return 0, err
		}
		if !has {
			return i * blockSize, nil
		}
	}
	return size, nil
}

func resumeOffsetError(declared, actual int64) localUploadError {
	return localUploadError{
		status: http.StatusConflict,
		code:   "UPLOAD_RESUME_OFFSET_MISMATCH",
		msg:    fmt.Sprintf("resume start %d does not match server resume offset %d", declared, actual),
	}
}

type localUploadError struct {
	status int
	code   string
	msg    string
}

func badLocalUploadRequest(format string, args ...interface{}) localUploadError {
	return localUploadError{status: http.StatusBadRequest, code: "UPLOAD_INVALID_CONTENT", msg: fmt.Sprintf(format, args...)}
}

func (e localUploadError) Error() string {
	return e.msg
}

func (e localUploadError) StatusCode() int {
	return e.status
}

func (e localUploadError) ErrorCode() string {
	return e.code
}

func hashDingCachePayload(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	headerSize := int64(36 + downloader.DEFAULT_BLOCK_MASK_MAX/8)
	if _, err = f.Seek(headerSize, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err = io.CopyN(h, f, size); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (u *UploadDao) QueryProgress(param LocalUploadParam) (*LocalUploadProgress, error) {
	orgRepo := util.GetOrgRepo(param.Org, param.Repo)
	blobPath := localBlobPath(param.RepoType, orgRepo, param.Sha256)
	if err := ensureLocalUploadPathSafe(config.SysConfig.Repos(), blobPath); err != nil {
		return nil, err
	}
	result := &LocalUploadProgress{
		RepoType: param.RepoType,
		Repo:     orgRepo,
		Revision: param.Revision,
		FilePath: param.FilePath,
		Sha256:   param.Sha256,
		Status:   "not_started",
	}

	currentCommit, currentManifest := u.currentManifest(param)
	if currentCommit != "" {
		if existing, ok := findManifestFile(currentManifest, param.FilePath); ok && existing.Sha256 == param.Sha256 {
			if complete, size, err := inspectCompleteBlob(blobPath, existing.Size); err != nil {
				return nil, err
			} else if complete {
				result.Size = size
				result.ResumeOffset = size
				result.Effective = true
				result.BlobComplete = true
				result.Status = "effective"
				return result, nil
			}
		}
	}

	stagePath := blobPath + localUploadStageSuffix
	if util.FileExists(stagePath) {
		dingFile, err := downloader.NewDingCache(stagePath, config.SysConfig.Download.BlockSize)
		if err != nil {
			return nil, err
		}
		offset, offsetErr := resumableOffset(dingFile)
		size := dingFile.GetFileSize()
		closeErr := dingFile.Close()
		if offsetErr != nil {
			return nil, offsetErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		result.Size = size
		result.ResumeOffset = offset
		result.Status = "uploading"
		return result, nil
	}

	if complete, size, err := inspectCompleteBlob(blobPath, -1); err != nil {
		return nil, err
	} else if complete {
		result.Size = size
		result.ResumeOffset = size
		result.BlobComplete = true
		result.Status = "blob_complete"
	}
	return result, nil
}

func (u *UploadDao) CleanupExpiredStagedUploads(retention time.Duration) (int, error) {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	root := filepath.Join(config.SysConfig.Repos(), "files")
	cutoff := time.Now().Add(-retention)
	removed := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), localUploadStageSuffix) {
			return nil
		}
		repoType, orgRepo, sha, ok := stagedBlobIdentity(root, path)
		if !ok {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return nil
			}
			return statErr
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		blobKey := fmt.Sprintf("upload-blob:%s:%s:%s", repoType, orgRepo, sha)
		uploadBlobLocks.Lock(blobKey)
		defer uploadBlobLocks.Unlock(blobKey)
		info, statErr = os.Stat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return nil
			}
			return statErr
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return rmErr
		}
		removed++
		return nil
	})
	if os.IsNotExist(err) {
		return removed, nil
	}
	return removed, err
}

func (u *UploadDao) RunStagedUploadCleanup(ctx context.Context) {
	interval := config.SysConfig.GetUploadStagingCleanupInterval()
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed, err := u.CleanupExpiredStagedUploads(config.SysConfig.GetUploadStagingRetention())
			if err != nil {
				zap.S().Warnf("cleanup expired staged uploads failed: %v", err)
				continue
			}
			if removed > 0 {
				zap.S().Infof("cleanup expired staged uploads removed %d file(s)", removed)
			}
		}
	}
}

func stagedBlobIdentity(root, stagePath string) (repoType, orgRepo, sha string, ok bool) {
	rel, err := filepath.Rel(root, stagePath)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", "", "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 5 || parts[len(parts)-2] != "blobs" {
		return "", "", "", false
	}
	repoType = parts[0]
	if repoType != "models" && repoType != "datasets" {
		return "", "", "", false
	}
	sha = strings.TrimSuffix(parts[len(parts)-1], localUploadStageSuffix)
	if sha == "" {
		return "", "", "", false
	}
	orgRepo = strings.Join(parts[1:len(parts)-2], "/")
	if orgRepo == "" {
		return "", "", "", false
	}
	return repoType, orgRepo, sha, true
}

func (u *UploadDao) currentManifest(param LocalUploadParam) (string, []LocalManifestFile) {
	currentCommit, _ := u.fileDao.GetCommitHfOffline(param.RepoType, util.GetOrgRepo(param.Org, param.Repo), param.Revision)
	if currentCommit != "" {
		return currentCommit, u.readManifest(param.RepoType, util.GetOrgRepo(param.Org, param.Repo), currentCommit)
	}
	return "", nil
}

func (u *UploadDao) nextCommit(current []LocalManifestFile, uploaded LocalManifestFile) (string, []LocalManifestFile, error) {
	manifest := append([]LocalManifestFile(nil), current...)
	replaced := false
	for i := range manifest {
		if manifest[i].Path == uploaded.Path {
			manifest[i] = uploaded
			replaced = true
			break
		}
	}
	if !replaced {
		manifest = append(manifest, uploaded)
	}
	sort.Slice(manifest, func(i, j int) bool {
		return manifest[i].Path < manifest[j].Path
	})
	data, err := sonic.Marshal(manifest)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), manifest, nil
}

func (u *UploadDao) readManifest(repoType, orgRepo, commit string) []LocalManifestFile {
	manifest, err := u.fileDao.ReadLocalManifest(repoType, orgRepo, commit)
	if err != nil {
		return nil
	}
	return manifest
}

// writeEffectiveMetadata 让本次上传对下载侧生效。
//
// 每个文件的元数据都从清单派生（见 FileDao.ReadLocalManifest），resolve 链接由
// 下载链路在首次请求时自行建立（FileDao.ConstructBlobsAndFileFile），所以这里
// 每次上传只写固定的 3 个文件，与该版本已有多少文件无关。
// 逐条落 paths-info、逐条建 resolve 链接会让逐个上传 N 个文件的代价变成 O(N²)
// 个文件系统对象（N=1000 时约一百万个），且其中绝大多数永远不会被读到。
func (u *UploadDao) writeEffectiveMetadata(param LocalUploadParam, commit string, manifest []LocalManifestFile) error {
	orgRepo := util.GetOrgRepo(param.Org, param.Repo)
	manifestPath := LocalManifestPath(param.RepoType, orgRepo, commit)
	if err := ensureLocalUploadPathSafe(config.SysConfig.Repos(), manifestPath); err != nil {
		return err
	}
	if err := util.MakeDirs(manifestPath); err != nil {
		return err
	}
	if err := util.WriteDataToFileAtomic(manifestPath, manifest); err != nil {
		return err
	}
	if err := u.writeMeta(param.RepoType, orgRepo, commit, commit, manifest); err != nil {
		return err
	}
	return u.writeMeta(param.RepoType, orgRepo, param.Revision, commit, manifest)
}

func (u *UploadDao) writeMeta(repoType, orgRepo, revision, commit string, manifest []LocalManifestFile) error {
	siblings := make([]map[string]string, 0, len(manifest))
	var usedStorage int64
	for _, item := range manifest {
		siblings = append(siblings, map[string]string{"rfilename": item.Path})
		usedStorage += item.Size
	}
	body, err := sonic.Marshal(map[string]interface{}{
		"id":          orgRepo,
		"sha":         commit,
		"siblings":    siblings,
		"usedStorage": usedStorage,
	})
	if err != nil {
		return err
	}
	headers := map[string]string{
		"content-type":   "application/json",
		"content-length": fmt.Sprintf("%d", len(body)),
		"x-repo-commit":  commit,
	}
	apiDir := filepath.Join(config.SysConfig.Repos(), "api", repoType, orgRepo, "revision", revision)
	metaGetPath := filepath.Join(apiDir, "meta_get.json")
	if err = ensureLocalUploadPathSafe(config.SysConfig.Repos(), metaGetPath); err != nil {
		return err
	}
	if err = util.MakeDirs(metaGetPath); err != nil {
		return err
	}
	if err = u.fileDao.WriteCacheRequest(metaGetPath, http.StatusOK, headers, body); err != nil {
		return err
	}
	metaHeadPath := filepath.Join(apiDir, "meta_head.json")
	if err = ensureLocalUploadPathSafe(config.SysConfig.Repos(), metaHeadPath); err != nil {
		return err
	}
	if err = util.MakeDirs(metaHeadPath); err != nil {
		return err
	}
	return u.fileDao.WriteCacheRequest(metaHeadPath, http.StatusOK, headers, body)
}

func findManifestFile(manifest []LocalManifestFile, filePath string) (LocalManifestFile, bool) {
	for _, item := range manifest {
		if item.Path == filePath {
			return item, true
		}
	}
	return LocalManifestFile{}, false
}

func tryEnterLocalUpload(key string) bool {
	localUploadInFlight.Lock()
	defer localUploadInFlight.Unlock()
	if _, ok := localUploadInFlight.keys[key]; ok {
		return false
	}
	localUploadInFlight.keys[key] = struct{}{}
	return true
}

func leaveLocalUpload(key string) {
	localUploadInFlight.Lock()
	defer localUploadInFlight.Unlock()
	delete(localUploadInFlight.keys, key)
}

func ensureLocalUploadPathSafe(root, target string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return localUploadError{status: http.StatusBadRequest, code: "UPLOAD_PATH_ESCAPE", msg: "target path escapes repository root"}
	}
	parent := filepath.Dir(targetAbs)
	for {
		if parent == rootAbs || parent == "." {
			return nil
		}
		info, statErr := os.Lstat(parent)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				parent = filepath.Dir(parent)
				continue
			}
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return localUploadError{status: http.StatusBadRequest, code: "UPLOAD_PATH_SYMLINK", msg: "target path contains a symlink parent"}
		}
		parent = filepath.Dir(parent)
	}
}
