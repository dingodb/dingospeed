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
	// Deferred 为 true 表示本次上传只把内容落盘，不重建清单、不产生新的快照标识、
	// 不改变版本标签指向。内容要等一次显式发布（PublishFiles）才对下载侧可见。
	Deferred bool
}

// LocalPublishParam 是一次批量发布的入参。发布清单由调用方在请求里完整声明，
// 服务端不记忆“哪些上传属于哪一批”——理由与“不另建进度状态”完全一致：
// 独立状态与实际落盘内容之间必然出现不一致，而清单三个字段本来就是上传时的必填项。
type LocalPublishParam struct {
	RepoType  string
	Org       string
	Repo      string
	Revision  string
	Overwrite bool
	Files     []LocalManifestFile
}

type LocalPublishResult struct {
	RepoType string `json:"repoType"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	Commit   string `json:"commit"`
	// Published 是本批次声明的条目数，FileCount 是合并后该版本的文件总数。
	Published int `json:"published"`
	FileCount int `json:"fileCount"`
	Added     int `json:"added"`
	Replaced  int `json:"replaced"`
	Unchanged int `json:"unchanged"`
	// Changed 为 false 表示合并后清单与当前清单完全相同，快照标识保持不变、未重写元数据。
	Changed bool   `json:"changed"`
	Status  string `json:"status"`
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
	RepoType string `json:"repoType"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	FilePath string `json:"filePath"`
	Size     int64  `json:"size"`
	Sha256   string `json:"sha256"`
	// ResumeOffset 是第一个空洞的位置，也就是顺序续传（老接口的 start）该从哪里接上。
	// 乱序并发的分块上传下它没有意义，用 MissingRanges。
	ResumeOffset int64 `json:"resumeOffset"`
	// BlockSize 是切分内容时必须使用的块大小，取自 blob header 里的权威值
	// （文件还不存在时回落到当前配置）。分块上传的调用方必须先取它再切分，
	// 不允许自己配置：agent 侧配错了就是静默的数据损坏。
	BlockSize int64 `json:"blockSize"`
	// MissingRanges 是尚未写入的字节区间列表，调用方据此只补缺块。
	MissingRanges []LocalByteRange `json:"missingRanges"`
	Effective     bool             `json:"effective"`
	BlobComplete  bool             `json:"blobComplete"`
	Status        string           `json:"status"`
}

type LocalByteRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
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
	// 加锁顺序固定为 repo → revision → blob，反向获取会死锁。
	uploadRepoLocks     = newKeyedMutex()
	uploadRevisionLocks = newKeyedMutex()
	// blob 锁是读写锁而不是互斥锁：分块上传的多个块要能同时往同一个 blob 里写
	// （取读锁，互不阻塞），而“整体替换这个 blob 文件”的两个动作——老接口的
	// os.Rename 与回收任务的 os.Remove——必须独占（取写锁）。
	// 少了这条区分，Linux 上 rename 会把 inode 换掉，正在写的分块 writer 会继续
	// 往孤儿 inode 上写，位图更新全部丢失且无人知晓。
	uploadBlobLocks = newKeyedRWMutex()
)

func uploadRepoLockKey(repoType, orgRepo string) string {
	return fmt.Sprintf("upload-repo:%s:%s", repoType, orgRepo)
}

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

type keyedRWMutexEntry struct {
	mu   sync.RWMutex
	refs int
}

// keyedRWMutex 与 keyedMutex 的引用计数策略完全一致，只是把互斥锁换成读写锁。
// refs 同时统计读者与写者：只有在没有任何持有者时条目才会被删除，
// 因此“删除后新来的获取者拿到一个全新条目”不会与仍未释放的持有者错配。
type keyedRWMutex struct {
	mu      sync.Mutex
	entries map[string]*keyedRWMutexEntry
}

func newKeyedRWMutex() *keyedRWMutex {
	return &keyedRWMutex{entries: make(map[string]*keyedRWMutexEntry)}
}

func (k *keyedRWMutex) acquire(key string) *keyedRWMutexEntry {
	k.mu.Lock()
	defer k.mu.Unlock()
	entry, ok := k.entries[key]
	if !ok {
		entry = &keyedRWMutexEntry{}
		k.entries[key] = entry
	}
	entry.refs++
	return entry
}

func (k *keyedRWMutex) release(key string) *keyedRWMutexEntry {
	k.mu.Lock()
	defer k.mu.Unlock()
	entry, ok := k.entries[key]
	if !ok {
		return nil
	}
	entry.refs--
	if entry.refs <= 0 {
		delete(k.entries, key)
	}
	return entry
}

func (k *keyedRWMutex) Lock(key string) {
	k.acquire(key).mu.Lock()
}

func (k *keyedRWMutex) Unlock(key string) {
	if entry := k.release(key); entry != nil {
		entry.mu.Unlock()
	}
}

func (k *keyedRWMutex) RLock(key string) {
	k.acquire(key).mu.RLock()
}

func (k *keyedRWMutex) RUnlock(key string) {
	if entry := k.release(key); entry != nil {
		entry.mu.RUnlock()
	}
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

	// 暂缓生效的上传完全不碰版本清单：它只把内容写进按摘要寻址的 blob 空间，
	// 而 blob 位置与仓库内路径无关，所以“这个路径已存在别的内容”在这一步
	// 结构上还不构成冲突。覆盖判定统一留到发布时按整批处理（BR-5）。
	if param.Deferred {
		reused, err := u.materializeBlob(param, orgRepo, body)
		if err != nil {
			return nil, err
		}
		return &LocalUploadResult{
			RepoType:   param.RepoType,
			Repo:       orgRepo,
			Revision:   param.Revision,
			FilePath:   param.FilePath,
			Size:       param.Size,
			Sha256:     param.Sha256,
			Status:     "staged",
			BlobReused: reused,
		}, nil
	}

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
	// 与批量发布一样先取仓库锁，把内容与清单的绑定挡在回收任务之外。
	repoLockKey := uploadRepoLockKey(param.RepoType, orgRepo)
	uploadRepoLocks.Lock(repoLockKey)
	defer uploadRepoLocks.Unlock(repoLockKey)
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
	if err = u.writeEffectiveMetadata(param.RepoType, orgRepo, param.Revision, commit, manifest); err != nil {
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

// PublishFiles 把一批已经完整落盘的文件一次性纳入版本清单，只产生一个快照标识。
//
// 与逐个上传的等价性靠“共用同一套清单合并与标识计算”保证（见 nextCommitBatch）：
// 只要合并后的清单相同，快照标识就必然逐字符相同，两条路径进系统的内容不会被
// 客户端当成两个不同的版本。
func (u *UploadDao) PublishFiles(param LocalPublishParam) (*LocalPublishResult, error) {
	orgRepo := util.GetOrgRepo(param.Org, param.Repo)

	// 发布之间必须明确拒绝而不是排队（BR-4.2.1），所以这里用 try-enter 而不是版本锁。
	publishKey := fmt.Sprintf("upload-publish:%s:%s:%s", param.RepoType, orgRepo, param.Revision)
	if !tryEnterLocalUpload(publishKey) {
		return nil, localUploadError{
			status: http.StatusConflict,
			code:   "PUBLISH_IN_PROGRESS",
			msg:    "another publish is already in progress for this revision",
		}
	}
	defer leaveLocalUpload(publishKey)

	// 仓库锁把“确认内容在盘上”和“把它写进清单”圈成一个整体，回收任务也持有它。
	// 少了这把锁，回收就可能挤进这两步中间，让清单指向一个刚被删掉的 blob。
	repoLockKey := uploadRepoLockKey(param.RepoType, orgRepo)
	uploadRepoLocks.Lock(repoLockKey)
	defer uploadRepoLocks.Unlock(repoLockKey)

	// 版本锁与即时生效上传共用同一把：两者都要“读清单 → 合并 → 写回”，
	// 不共用就会各自基于陈旧清单重建，后写的覆盖先写的。
	revisionLockKey := fmt.Sprintf("upload-revision:%s:%s:%s", param.RepoType, orgRepo, param.Revision)
	uploadRevisionLocks.Lock(revisionLockKey)
	defer uploadRevisionLocks.Unlock(revisionLockKey)

	if err := u.verifyPublishContent(param, orgRepo); err != nil {
		return nil, err
	}

	currentCommit, currentManifest := u.currentManifestOf(param.RepoType, orgRepo, param.Revision)
	added, replaced, unchanged, err := classifyPublishItems(currentManifest, param)
	if err != nil {
		return nil, err
	}

	commit, manifest, err := u.nextCommitBatch(currentManifest, param.Files)
	if err != nil {
		return nil, err
	}

	result := &LocalPublishResult{
		RepoType:  param.RepoType,
		Repo:      orgRepo,
		Revision:  param.Revision,
		Commit:    commit,
		Published: len(param.Files),
		FileCount: len(manifest),
		Added:     added,
		Replaced:  replaced,
		Unchanged: unchanged,
	}

	// 合并后与当前清单完全一致：标识保持不变，不重写任何元数据（BR-2.3）。
	if commit == currentCommit {
		result.Changed = false
		result.Status = "unchanged"
		return result, nil
	}

	if err = u.writeEffectiveMetadata(param.RepoType, orgRepo, param.Revision, commit, manifest); err != nil {
		return nil, err
	}
	result.Changed = true
	result.Status = "published"
	return result, nil
}

// verifyPublishContent 逐条确认发布清单声明的内容确实完整躺在磁盘上。
// 发布清单是调用方声明的，不能信：少传了一个文件、传了一半就中断、内容被外部删掉，
// 都会让清单进入“有记录但没文件”的破损状态，而那正是生效语义明令不允许出现的。
func (u *UploadDao) verifyPublishContent(param LocalPublishParam, orgRepo string) error {
	repos := config.SysConfig.Repos()
	var missing, mismatched []string
	for _, item := range param.Files {
		blobPath := localBlobPath(param.RepoType, orgRepo, item.Sha256)
		if err := ensureLocalUploadPathSafe(repos, blobPath); err != nil {
			return err
		}
		complete, size, err := inspectCompleteBlob(blobPath, -1)
		if err != nil {
			return err
		}
		if !complete {
			missing = append(missing, item.Path)
			continue
		}
		if size != item.Size {
			mismatched = append(mismatched, fmt.Sprintf("%s (declared %d, staged %d)", item.Path, item.Size, size))
		}
	}
	if len(missing) > 0 {
		return localUploadError{
			status: http.StatusConflict,
			code:   "PUBLISH_CONTENT_NOT_READY",
			msg:    fmt.Sprintf("content is not fully uploaded for: %s", strings.Join(missing, ", ")),
		}
	}
	if len(mismatched) > 0 {
		return localUploadError{
			status: http.StatusConflict,
			code:   "PUBLISH_CONTENT_MISMATCH",
			msg:    fmt.Sprintf("declared size does not match uploaded content for: %s", strings.Join(mismatched, ", ")),
		}
	}
	return nil
}

// classifyPublishItems 统计本批次相对当前清单的新增/覆盖/无变化，并在需要显式覆盖时拒绝。
// 覆盖声明作用于整次发布：只要有一条冲突就整次拒绝，不做部分发布——
// 部分发布会重新引入批量本来要消除的中间态，调用方也无从判断哪些生效了。
func classifyPublishItems(current []LocalManifestFile, param LocalPublishParam) (added, replaced, unchanged int, err error) {
	var conflicts []string
	for _, item := range param.Files {
		existing, ok := findManifestFile(current, item.Path)
		switch {
		case !ok:
			added++
		case existing.Sha256 == item.Sha256 && existing.Size == item.Size:
			unchanged++
		case !param.Overwrite:
			conflicts = append(conflicts, item.Path)
		default:
			replaced++
		}
	}
	if len(conflicts) > 0 {
		return 0, 0, 0, localUploadError{
			status: http.StatusConflict,
			code:   "PUBLISH_OVERWRITE_REQUIRED",
			msg: fmt.Sprintf("these files already exist with different content; set overwrite=true to replace them: %s",
				strings.Join(conflicts, ", ")),
		}
	}
	return added, replaced, unchanged, nil
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
	// 写锁：os.Rename 是整体替换文件的动作，必须与正在写同一个 blob 的分块上传互斥。
	blobKey := uploadBlobLockKey(param.RepoType, orgRepo, param.Sha256)
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

// localBlobPath 与下载侧共用同一个 BlobPath：上传写的就是下载读的那个文件，
// 两侧算出来的路径字符串必须逐字符相同，否则 DingCacheManager 会为同一个文件
// 发出两个句柄，各自维护一份互相看不见的块位图。
func localBlobPath(repoType, orgRepo, sha256 string) string {
	return BlobPath(repoType, orgRepo, sha256)
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
		RepoType:      param.RepoType,
		Repo:          orgRepo,
		Revision:      param.Revision,
		FilePath:      param.FilePath,
		Sha256:        param.Sha256,
		BlockSize:     config.SysConfig.Download.BlockSize,
		MissingRanges: []LocalByteRange{},
		Status:        "not_started",
	}

	currentCommit, currentManifest := u.currentManifest(param)
	if currentCommit != "" {
		if existing, ok := findManifestFile(currentManifest, param.FilePath); ok && existing.Sha256 == param.Sha256 {
			blob, err := inspectBlobProgress(blobPath)
			if err != nil {
				return nil, err
			}
			if blob.exists && blob.size == existing.Size && len(blob.missing) == 0 {
				result.Size = blob.size
				result.BlockSize = blob.blockSize
				result.ResumeOffset = blob.size
				result.Effective = true
				result.BlobComplete = true
				result.Status = "effective"
				return result, nil
			}
		}
	}

	// 老接口的续传暂存文件优先：它存在就说明这个 sha 正走整文件上传那条路，
	// 该路径的续传语义是顺序的，只能用 ResumeOffset 表达。
	stagePath := blobPath + localUploadStageSuffix
	if util.FileExists(stagePath) {
		dingFile, err := downloader.NewDingCache(stagePath, config.SysConfig.Download.BlockSize)
		if err != nil {
			return nil, err
		}
		offset, offsetErr := resumableOffset(dingFile)
		size := dingFile.GetFileSize()
		blockSize := dingFile.GetBlockSize()
		closeErr := dingFile.Close()
		if offsetErr != nil {
			return nil, offsetErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		result.Size = size
		result.BlockSize = blockSize
		result.ResumeOffset = offset
		result.Status = "uploading"
		return result, nil
	}

	// 最终位置上的 blob：分块上传的内容从第一个字节起就写在这里，因此它既可能是
	// 一份完整内容，也可能是一份还缺着块的半成品。缺块必须以区间列表形式回给调用方，
	// “第一个空洞”这个顺序续传的概念在乱序并发下没有意义。
	blob, err := inspectBlobProgress(blobPath)
	if err != nil {
		return nil, err
	}
	if !blob.exists {
		return result, nil
	}
	result.Size = blob.size
	result.BlockSize = blob.blockSize
	result.MissingRanges = blob.missing
	if len(blob.missing) == 0 {
		result.ResumeOffset = blob.size
		result.BlobComplete = true
		result.Status = "blob_complete"
	} else {
		result.ResumeOffset = blob.missing[0].Offset
		result.Status = "uploading"
	}
	return result, nil
}

type blobProgress struct {
	exists    bool
	size      int64
	blockSize int64
	missing   []LocalByteRange
}

func inspectBlobProgress(blobPath string) (blobProgress, error) {
	progress := blobProgress{missing: []LocalByteRange{}}
	if !util.FileExists(blobPath) {
		return progress, nil
	}
	dingFile, err := downloader.NewDingCache(blobPath, config.SysConfig.Download.BlockSize)
	if err != nil {
		return progress, err
	}
	progress.exists = true
	progress.size = dingFile.GetFileSize()
	progress.blockSize = dingFile.GetBlockSize()
	missing, missErr := missingBlockRanges(dingFile)
	closeErr := dingFile.Close()
	if missErr != nil {
		return progress, missErr
	}
	if closeErr != nil {
		return progress, closeErr
	}
	progress.missing = missing
	return progress, nil
}

// missingBlockRanges 把位图里未置位的块合并成字节区间。相邻缺块合并成一段，
// 让调用方可以按自己的 chunk 大小重新切分，而不是被迫按块逐个重传。
func missingBlockRanges(dingFile *downloader.DingCache) ([]LocalByteRange, error) {
	size := dingFile.GetFileSize()
	blockSize := dingFile.GetBlockSize()
	if blockSize <= 0 {
		return nil, fmt.Errorf("invalid cache block size: %d", blockSize)
	}
	blockNum := (size + blockSize - 1) / blockSize
	ranges := []LocalByteRange{}
	runStart := int64(-1)
	appendRun := func(endBlock int64) {
		offset := runStart * blockSize
		end := min(endBlock*blockSize, size)
		ranges = append(ranges, LocalByteRange{Offset: offset, Length: end - offset})
		runStart = -1
	}
	for i := int64(0); i < blockNum; i++ {
		has, err := dingFile.HasBlock(i)
		if err != nil {
			return nil, err
		}
		if !has {
			if runStart < 0 {
				runStart = i
			}
			continue
		}
		if runStart >= 0 {
			appendRun(i)
		}
	}
	if runStart >= 0 {
		appendRun(blockNum)
	}
	return ranges, nil
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
		blobKey := uploadBlobLockKey(repoType, orgRepo, sha)
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

// CleanupUnreferencedBlobs 回收“已经完整落盘、但不被任何清单引用”的内容。
//
// 这是批量发布引入的一类新残留：暂缓生效的上传成功后内容就完整躺在盘上了，
// 但那次发布可能永远不会来（脚本挂了、批次被放弃、换了别的清单）。它既不是
// 未完成的暂存文件（暂存清理够不着），又在本地命名空间里（防淘汰保护着它），
// 现有机制下会无限累积。
//
// 反过来，误删的代价极高：本地命名空间是自研内容的**唯一发布源**，删了就没有了。
// 所以判定“未被引用”时要扫遍该仓库下**所有**快照的清单，而不只是版本标签当前
// 指向的那一个——记录过旧标识的客户端仍然能解析到旧快照。
func (u *UploadDao) CleanupUnreferencedBlobs(retention time.Duration) (int, error) {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	repos := config.SysConfig.Repos()
	root := filepath.Join(repos, "files")
	cutoff := time.Now().Add(-retention)
	removed := 0

	repoBlobs, err := collectLocalBlobs(root)
	if err != nil {
		return 0, err
	}
	for repoKey, blobs := range repoBlobs {
		repoType, orgRepo := repoKey.repoType, repoKey.orgRepo
		count, cleanErr := u.cleanupRepoBlobs(repos, repoType, orgRepo, blobs, cutoff)
		removed += count
		if cleanErr != nil {
			return removed, cleanErr
		}
	}
	return removed, nil
}

type localRepoKey struct {
	repoType string
	orgRepo  string
}

// collectLocalBlobs 只收集本地命名空间下的 blob。公开模型的缓存与自研内容共用
// 同一棵目录树，漏掉这个判断就会把公开缓存也当成“未被引用”删掉，
// 那是在改动磁盘清理对公开模型的既有行为。
func collectLocalBlobs(root string) (map[localRepoKey][]string, error) {
	result := make(map[localRepoKey][]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), localUploadStageSuffix) {
			return nil
		}
		repoType, orgRepo, sha, ok := stagedBlobIdentity(root, path)
		if !ok || !IsLocalOrgRepo(orgRepo) {
			return nil
		}
		key := localRepoKey{repoType: repoType, orgRepo: orgRepo}
		result[key] = append(result[key], sha)
		return nil
	})
	if os.IsNotExist(err) {
		return result, nil
	}
	return result, err
}

func (u *UploadDao) cleanupRepoBlobs(repos, repoType, orgRepo string, blobs []string, cutoff time.Time) (int, error) {
	// 仓库锁挡住并发的发布：引用集合在这把锁下算出来之后就不会再被追加引用。
	repoLockKey := uploadRepoLockKey(repoType, orgRepo)
	uploadRepoLocks.Lock(repoLockKey)
	defer uploadRepoLocks.Unlock(repoLockKey)

	referenced, err := u.referencedShas(repos, repoType, orgRepo)
	if err != nil {
		return 0, err
	}
	tombstones := readRecycleEntries(repoType, orgRepo)
	removed := 0
	for _, sha := range blobs {
		if _, ok := referenced[sha]; ok {
			// 引用又回来了（重新上传并发布过），那条墓碑已经是错的，就地作废。
			if _, has := tombstones[sha]; has {
				removeRecycleEntry(repoType, orgRepo, sha)
			}
			continue
		}
		blobPath := localBlobPath(repoType, orgRepo, sha)
		info, statErr := os.Stat(blobPath)
		switch {
		case statErr != nil && os.IsNotExist(statErr):
			removeRecycleEntry(repoType, orgRepo, sha)
			continue
		case statErr != nil:
			return removed, statErr
		}
		if !expiredForReclaim(info, tombstones[sha], cutoff) {
			// 保留期内：可能正等着一次还没发出来的发布，或者还躺在回收站里，留着。
			continue
		}
		// 同一个 sha 可能还有一个正在续传的暂存文件，那份由暂存清理按自己的
		// 保留期处理，这里只管最终位置上的那一份——**不检查它是否完整**。
		// 这正是分块上传需要的行为：中途放弃的分块上传就是以不完整的
		// blobs/<sha> 形式留在盘上的，只按“未被任何清单引用 + 超过保留期”回收。
		// 续传间隔小于保留期（默认 168h）就不会被误删。
		if rmErr := reclaimBlobFile(repoType, orgRepo, sha); rmErr != nil {
			// 传输占用是暂时的，下一个周期再来即可；为它中断整趟清理不划算。
			zap.S().Warnf("[UPLOAD-CLEANUP] reclaim skipped: %s/%s sha=%s err=%v", repoType, orgRepo, sha, rmErr)
			continue
		}
		removeRecycleEntry(repoType, orgRepo, sha)
		removed++
		zap.S().Infof("[UPLOAD-CLEANUP] reclaimed unreferenced upload content: %s/%s sha=%s", repoType, orgRepo, sha)
	}
	return removed, nil
}

// expiredForReclaim 判断一份未被引用的内容是否已经过了保留期。
//
// 有墓碑时以墓碑的 unlinkedAt 为准，没有才回落到 blob 的 mtime。
// 这个区分是必须的：一个三个月前上传的文件今天被删进回收站，它的 mtime 早就过期了，
// 按 mtime 判定的话下一个清理周期（最多一小时）就会把它彻底删掉，回收站形同虚设。
// 反过来，“暂缓生效的上传等不到发布”这类老残留没有墓碑，仍按 mtime 判定，行为不变。
func expiredForReclaim(info os.FileInfo, entry RecycleEntry, cutoff time.Time) bool {
	if entry.UnlinkedAt > 0 {
		return !time.Unix(entry.UnlinkedAt, 0).After(cutoff)
	}
	return !info.ModTime().After(cutoff)
}

// CleanupRecycledBlobs 回收远端缓存里被一级删除、且已过保留期的内容。
//
// 判据严格限定为“存在回收站墓碑”，不是“扫出来没有引用就删”。两条理由：
//  1. 公开缓存与自研内容共用同一棵目录树，无差别回收等于改动磁盘清理对公开模型的
//     既有行为——这正是 collectLocalBlobs 里那条 IsLocalOrgRepo 过滤要防的事；
//  2. diskClean 的 LRU 会直接 os.Remove resolve 链接。一个远端 blob 完全可能
//     “链接被 LRU 删了、内容还在”，此时它在引用扫描下就是无引用的，
//     而这跟用户的任何删除动作都无关。
//
// 本地命名空间的墓碑不在这里处理：CleanupUnreferencedBlobs 那一趟已经覆盖了它们。
func (u *UploadDao) CleanupRecycledBlobs(retention time.Duration) (int, error) {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	cutoff := time.Now().Add(-retention)
	removed := 0
	for _, key := range listRepoKeys() {
		if IsLocalOrgRepo(key.OrgRepo) {
			continue
		}
		removed += u.cleanupRecycledRepo(key.RepoType, key.OrgRepo, cutoff)
	}
	return removed, nil
}

func (u *UploadDao) cleanupRecycledRepo(repoType, orgRepo string, cutoff time.Time) int {
	tombstones := readRecycleEntries(repoType, orgRepo)
	if len(tombstones) == 0 {
		return 0
	}
	repoLockKey := uploadRepoLockKey(repoType, orgRepo)
	uploadRepoLocks.Lock(repoLockKey)
	defer uploadRepoLocks.Unlock(repoLockKey)

	idx := buildRepoIndex(repoType, orgRepo)
	removed := 0
	for sha, entry := range tombstones {
		if len(idx.BySha[sha]) > 0 {
			removeRecycleEntry(repoType, orgRepo, sha)
			continue
		}
		if _, ok := idx.Blobs[sha]; !ok {
			removeRecycleEntry(repoType, orgRepo, sha)
			continue
		}
		if time.Unix(entry.UnlinkedAt, 0).After(cutoff) {
			continue
		}
		if err := reclaimBlobFile(repoType, orgRepo, sha); err != nil {
			zap.S().Warnf("[UPLOAD-CLEANUP] reclaim recycled blob skipped: %s/%s sha=%s err=%v", repoType, orgRepo, sha, err)
			continue
		}
		removeRecycleEntry(repoType, orgRepo, sha)
		removed++
		zap.S().Infof("[UPLOAD-CLEANUP] reclaimed recycled cache content: %s/%s sha=%s", repoType, orgRepo, sha)
	}
	return removed
}

// referencedShas 收集该仓库下所有快照清单引用到的内容摘要。
// 扫的是全部 commit 清单，不是版本标签当前指向的那一份。
func (u *UploadDao) referencedShas(repos, repoType, orgRepo string) (map[string]struct{}, error) {
	referenced := make(map[string]struct{})
	revisionRoot := filepath.Join(repos, "api", repoType, filepath.FromSlash(orgRepo), "revision")
	entries, err := os.ReadDir(revisionRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return referenced, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		for _, item := range u.readManifest(repoType, orgRepo, entry.Name()) {
			referenced[item.Sha256] = struct{}{}
		}
	}
	return referenced, nil
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
			} else if removed > 0 {
				zap.S().Infof("cleanup expired staged uploads removed %d file(s)", removed)
			}
			retention := config.SysConfig.GetUploadOrphanRetention()
			reclaimed, err := u.CleanupUnreferencedBlobs(retention)
			if err != nil {
				zap.S().Warnf("reclaim unreferenced upload content failed: %v", err)
			} else if reclaimed > 0 {
				zap.S().Infof("reclaimed %d unreferenced upload content file(s)", reclaimed)
			}
			// 上一趟只覆盖本地命名空间。远端缓存里被一级删除的内容由墓碑驱动，
			// 与上传内容共用同一个保留期（orphanRetentionHours，默认 168h）。
			recycled, err := u.CleanupRecycledBlobs(retention)
			if err != nil {
				zap.S().Warnf("reclaim recycled cache content failed: %v", err)
				continue
			}
			if recycled > 0 {
				zap.S().Infof("reclaimed %d recycled cache content file(s)", recycled)
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
	return u.currentManifestOf(param.RepoType, util.GetOrgRepo(param.Org, param.Repo), param.Revision)
}

func (u *UploadDao) currentManifestOf(repoType, orgRepo, revision string) (string, []LocalManifestFile) {
	currentCommit, _ := u.fileDao.GetCommitHfOffline(repoType, orgRepo, revision)
	if currentCommit != "" {
		return currentCommit, u.readManifest(repoType, orgRepo, currentCommit)
	}
	return "", nil
}

func (u *UploadDao) nextCommit(current []LocalManifestFile, uploaded LocalManifestFile) (string, []LocalManifestFile, error) {
	return u.nextCommitBatch(current, []LocalManifestFile{uploaded})
}

// nextCommitBatch 把一批文件合并进当前清单并算出新的快照标识。
//
// 单文件上传与批量发布**必须**共用这一个实现。清单的合并、排序与序列化只要出现
// 第二份写法，同一批内容走两条路径就会算出不同的标识，客户端会把它们当成两个版本
// 白白重下一遍；而这种偏差在单独测批量时完全测不出来，因为批量路径自己是自洽的。
func (u *UploadDao) nextCommitBatch(current, uploaded []LocalManifestFile) (string, []LocalManifestFile, error) {
	manifest := append([]LocalManifestFile(nil), current...)
	for _, item := range uploaded {
		replaced := false
		for i := range manifest {
			if manifest[i].Path == item.Path {
				manifest[i] = item
				replaced = true
				break
			}
		}
		if !replaced {
			manifest = append(manifest, item)
		}
	}
	sort.Slice(manifest, func(i, j int) bool {
		return manifest[i].Path < manifest[j].Path
	})
	commit, err := manifestCommit(manifest)
	if err != nil {
		return "", nil, err
	}
	return commit, manifest, nil
}

// manifestCommit 是“清单内容 → 快照标识”的唯一实现。
//
// 发布与缓存管理的删除都要算它：删除会把一份清单变成另一份清单，新清单必须落在
// 与发布路径完全一致的标识下。两处各写一遍序列化，同样的内容就会算出两个标识，
// 客户端会把它们当成两个版本白白重下一遍。
func manifestCommit(manifest []LocalManifestFile) (string, error) {
	data, err := sonic.Marshal(manifest)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
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
func (u *UploadDao) writeEffectiveMetadata(repoType, orgRepo, revision, commit string, manifest []LocalManifestFile) error {
	manifestPath := LocalManifestPath(repoType, orgRepo, commit)
	if err := ensureLocalUploadPathSafe(config.SysConfig.Repos(), manifestPath); err != nil {
		return err
	}
	if err := util.MakeDirs(manifestPath); err != nil {
		return err
	}
	if err := util.WriteDataToFileAtomic(manifestPath, manifest); err != nil {
		return err
	}
	if err := u.writeMeta(repoType, orgRepo, commit, commit, manifest); err != nil {
		return err
	}
	return u.writeMeta(repoType, orgRepo, revision, commit, manifest)
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
