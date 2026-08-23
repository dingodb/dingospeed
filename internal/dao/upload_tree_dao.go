package dao

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"dingospeed/pkg/util"

	"github.com/bytedance/sonic"
	"go.uber.org/zap"
)

// LocalPublishTreeParam 是一次“整棵树发布”的入参。
//
// 与 LocalPublishParam 的区别只有一处，但语义天差地别：Files 是**目标全量清单**，
// 不是增量批次。发布路径做的是并集合并，因此永远无法让清单变短；仓库编辑要把
// 新增与删除合成一次提交，只能由调用方声明“提交之后应该是什么样”。
//
// BaseCommit 是必填的乐观锁：调用方基于某个快照编辑，提交时该快照必须仍是
// revision 的当前指向，否则说明别人已经改过，静默覆盖会丢掉对方的提交。
type LocalPublishTreeParam struct {
	RepoType   string
	Org        string
	Repo       string
	Revision   string
	BaseCommit string
	Files      []LocalManifestFile
}

type LocalPublishTreeResult struct {
	RepoType string `json:"repoType"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	Commit   string `json:"commit"`
	// PreviousCommit 是本次提交之前 revision 的指向。
	//
	// 调用方判断“改动是否生效”必须看它与 Commit 是否不同，不能看 Commit 是否变化：
	// 快照标识是清单内容的确定性摘要，删掉一个文件再把同一份内容加回来，算出的
	// 标识与之前那次逐字符相同。
	PreviousCommit string `json:"previousCommit"`
	FileCount      int    `json:"fileCount"`
	Added          int    `json:"added"`
	Replaced       int    `json:"replaced"`
	Removed        int    `json:"removed"`
	Unchanged      int    `json:"unchanged"`
	Changed        bool   `json:"changed"`
	Status         string `json:"status"`
}

// supersededMarkerFileName 标记一个已经没有任何 revision 指向的旧快照。
//
// 它不是历史：旧快照不进任何列表、不可切换查看，保留它只为两件事——
// 客户端拿到 sha 之后会按这个 sha 逐文件下载（大模型可持续数十分钟），
// 立刻抹掉会让在途下载整片 404；以及给用户一步“撤销上次修改”的窗口。
// 到期由清理任务回收。
const supersededMarkerFileName = "dingo-superseded.json"

type supersededMarker struct {
	// Revision 是把这个快照顶下去的那个版本标签，用来做“每个 revision 只留一个”。
	Revision     string `json:"revision"`
	ByCommit     string `json:"byCommit"`
	SupersededAt int64  `json:"supersededAt"`
}

func supersededMarkerPath(repoType, orgRepo, commit string) string {
	return filepath.Join(repoApiRoot(repoType, orgRepo), "revision", commit, supersededMarkerFileName)
}

// PublishTree 用一份完整的目标清单取代 revision 当前的清单，只产生一个新快照。
//
// 与 PublishFiles 共用 manifestCommit 与 writeEffectiveMetadata：两条路径算出的
// 标识必须来自同一份序列化，否则同样的内容会得到两个标识，客户端会白白重下一遍。
func (u *UploadDao) PublishTree(param LocalPublishTreeParam) (*LocalPublishTreeResult, error) {
	orgRepo := util.GetOrgRepo(param.Org, param.Repo)

	// 锁序与 PublishFiles 完全一致：publish try-enter → 仓库锁 → 版本锁。
	// 少一把或换个顺序都会与即时生效上传、回收任务交叉成死锁或脏读。
	publishKey := fmt.Sprintf("upload-publish:%s:%s:%s", param.RepoType, orgRepo, param.Revision)
	if !tryEnterLocalUpload(publishKey) {
		return nil, localUploadError{
			status: http.StatusConflict,
			code:   "PUBLISH_IN_PROGRESS",
			msg:    "another publish is already in progress for this revision",
		}
	}
	defer leaveLocalUpload(publishKey)

	repoLockKey := uploadRepoLockKey(param.RepoType, orgRepo)
	uploadRepoLocks.Lock(repoLockKey)
	defer uploadRepoLocks.Unlock(repoLockKey)

	revisionLockKey := fmt.Sprintf("upload-revision:%s:%s:%s", param.RepoType, orgRepo, param.Revision)
	uploadRevisionLocks.Lock(revisionLockKey)
	defer uploadRevisionLocks.Unlock(revisionLockKey)

	currentCommit, currentManifest := u.currentManifestOf(param.RepoType, orgRepo, param.Revision)
	if currentCommit == "" {
		return nil, localUploadError{
			status: http.StatusNotFound,
			code:   "REVISION_NOT_FOUND",
			msg:    fmt.Sprintf("revision %s does not exist; use publish to create it", param.Revision),
		}
	}
	if currentCommit != param.BaseCommit {
		return nil, localUploadError{
			status: http.StatusConflict,
			code:   "REVISION_CHANGED",
			msg: fmt.Sprintf("revision %s now points at %s, not the edited %s; reload and apply the changes again",
				param.Revision, currentCommit, param.BaseCommit),
		}
	}

	// 目标清单里的每一条都要确认内容完整躺在盘上，包括原本就在清单里的那些：
	// 编辑期间外部删掉一个 blob，照单全收就会写出一份“有记录没文件”的快照。
	if err := verifyManifestContent(param.RepoType, orgRepo, param.Files); err != nil {
		return nil, err
	}

	manifest := append([]LocalManifestFile(nil), param.Files...)
	sort.Slice(manifest, func(i, j int) bool {
		return manifest[i].Path < manifest[j].Path
	})
	commit, err := manifestCommit(manifest)
	if err != nil {
		return nil, err
	}

	added, replaced, unchanged, removed := diffManifest(currentManifest, manifest)
	result := &LocalPublishTreeResult{
		RepoType:       param.RepoType,
		Repo:           orgRepo,
		Revision:       param.Revision,
		Commit:         commit,
		PreviousCommit: currentCommit,
		FileCount:      len(manifest),
		Added:          added,
		Replaced:       replaced,
		Removed:        removed,
		Unchanged:      unchanged,
	}

	// 目标与当前逐字节相同：不重写任何元数据，也不产生待回收快照。
	if commit == currentCommit {
		result.Changed = false
		result.Status = "unchanged"
		return result, nil
	}

	// 新快照完整写完之后，writeEffectiveMetadata 最后才改标签的指向；
	// 中途失败时 revision 仍指向旧快照，不会出现指向半份内容的窗口。
	if err = u.writeEffectiveMetadata(param.RepoType, orgRepo, param.Revision, commit, manifest); err != nil {
		return nil, err
	}
	u.retireSupersededCommit(param.RepoType, orgRepo, param.Revision, currentCommit, commit)

	result.Changed = true
	result.Status = "published"
	return result, nil
}

// diffManifest 统计目标清单相对当前清单的增删改，只用于回执展示。
func diffManifest(current, target []LocalManifestFile) (added, replaced, unchanged, removed int) {
	currentByPath := make(map[string]LocalManifestFile, len(current))
	for _, item := range current {
		currentByPath[item.Path] = item
	}
	targetPaths := make(map[string]struct{}, len(target))
	for _, item := range target {
		targetPaths[item.Path] = struct{}{}
		existing, ok := currentByPath[item.Path]
		switch {
		case !ok:
			added++
		case existing.Sha256 == item.Sha256 && existing.Size == item.Size:
			unchanged++
		default:
			replaced++
		}
	}
	for _, item := range current {
		if _, ok := targetPaths[item.Path]; !ok {
			removed++
		}
	}
	return added, replaced, unchanged, removed
}

// retireSupersededCommit 给刚被顶下去的旧快照打墓碑。
//
// 打之前必须重新确认没有别的标签指着它：快照是内容寻址的，main 与 v1 只要文件集合
// 相同就是同一个目录，改完 main 就回收，会把 v1 指向的内容一起抹掉。
//
// 这一步的失败不该让整次提交失败——标签已经成功改指，内容也已经完整落盘，
// 丢的只是回收的起点，下一次编辑或人工清理仍能兜住。
func (u *UploadDao) retireSupersededCommit(repoType, orgRepo, revision, oldCommit, newCommit string) {
	// 新指向的快照可能正是之前被顶下去的某一个：撤销是一种，删掉一个文件又把同样的
	// 内容加回来也是一种（标识是内容的摘要，算出来逐字符相同），清空一个 revision
	// 更是每次都会回到同一个空快照。它现在重新被标签指着，墓碑必须先摘掉，
	// 否则下面的清理和回收任务会把一份正在生效的快照删掉。
	clearSupersededMarker(repoType, orgRepo, newCommit)
	if oldCommit == "" || oldCommit == newCommit {
		return
	}
	if commitHasTags(repoType, orgRepo, oldCommit) {
		return
	}
	marker := supersededMarker{Revision: revision, ByCommit: newCommit, SupersededAt: time.Now().Unix()}
	if err := util.WriteDataToFileAtomic(supersededMarkerPath(repoType, orgRepo, oldCommit), marker); err != nil {
		zap.S().Warnf("[UPLOAD-TREE] mark superseded snapshot failed: %s/%s commit=%s err=%v", repoType, orgRepo, oldCommit, err)
		return
	}
	// 每个 revision 只留最近一个待回收快照：频繁编辑本来就会一次顶掉一个，
	// 全留着就是在偷偷攒历史，而历史正是本设计明确不提供的东西。
	u.dropOlderSupersededSnapshots(repoType, orgRepo, revision, oldCommit)
}

func (u *UploadDao) dropOlderSupersededSnapshots(repoType, orgRepo, revision, keepCommit string) {
	for commit, marker := range listSupersededMarkers(repoType, orgRepo) {
		if commit == keepCommit || marker.Revision != revision {
			continue
		}
		// 与 CleanupSupersededSnapshots 一样，删之前再确认一遍没有标签指着它：
		// 打完墓碑之后它可能又被重新指了回来。
		if commitHasTags(repoType, orgRepo, commit) {
			clearSupersededMarker(repoType, orgRepo, commit)
			continue
		}
		if err := u.dropSnapshotFiles(repoType, orgRepo, commit); err != nil {
			zap.S().Warnf("[UPLOAD-TREE] drop superseded snapshot failed: %s/%s commit=%s err=%v", repoType, orgRepo, commit, err)
		}
	}
}

// commitHasTags 判断是否还有版本标签指向这个快照。
//
// 判定规则与 collectRepoTags 保持一致：writeEffectiveMetadata 会给快照自己也写一份
// 元数据（目录名就是 sha），那份不算标签，否则任何快照都会被判成“仍被引用”。
func commitHasTags(repoType, orgRepo, commit string) bool {
	revisionRoot := filepath.Join(repoApiRoot(repoType, orgRepo), "revision")
	entries, err := os.ReadDir(revisionRoot)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == commit {
			continue
		}
		sha := readMetaSha(filepath.Join(revisionRoot, entry.Name(), "meta_get.json"))
		if sha == "" || sha == entry.Name() {
			continue
		}
		if sha == commit {
			return true
		}
	}
	return false
}

// clearSupersededMarker 摘掉一个快照的墓碑，用在它重新被标签指向的时候。
func clearSupersededMarker(repoType, orgRepo, commit string) {
	if commit == "" {
		return
	}
	if err := os.Remove(supersededMarkerPath(repoType, orgRepo, commit)); err != nil && !os.IsNotExist(err) {
		zap.S().Warnf("[UPLOAD-TREE] clear superseded marker failed: %s/%s commit=%s err=%v", repoType, orgRepo, commit, err)
	}
}

func listSupersededMarkers(repoType, orgRepo string) map[string]supersededMarker {
	result := make(map[string]supersededMarker)
	revisionRoot := filepath.Join(repoApiRoot(repoType, orgRepo), "revision")
	entries, err := os.ReadDir(revisionRoot)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		b, readErr := util.ReadFileToBytes(supersededMarkerPath(repoType, orgRepo, entry.Name()))
		if readErr != nil {
			continue
		}
		var marker supersededMarker
		if sonic.Unmarshal(b, &marker) != nil {
			continue
		}
		result[entry.Name()] = marker
	}
	return result
}

// dropSnapshotFiles 抹掉一个快照的清单、元数据与 resolve 链接。
//
// 与缓存管理的 dropSnapshot 共用一份实现：链接必须一起删，留着不只是垃圾，
// 软链创建失败时它会降级成硬链，硬链还在的话彻底删除 blob 也不会释放磁盘。
func (u *UploadDao) dropSnapshotFiles(repoType, orgRepo, commit string) error {
	return dropSnapshotFiles(u.fileDao, repoType, orgRepo, commit)
}

func dropSnapshotFiles(fileDao *FileDao, repoType, orgRepo, commit string) error {
	if err := os.RemoveAll(filepath.Join(repoApiRoot(repoType, orgRepo), "revision", commit)); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(repoFilesRoot(repoType, orgRepo), "resolve", commit)); err != nil {
		return err
	}
	// 清单本来“同一个 commit 不会再变”，FileDao 因此把它长期缓存；
	// 快照消失之后这份缓存必须清掉，否则下载侧还会读到它。
	fileDao.InvalidateLocalManifest(repoType, orgRepo, commit)
	return nil
}

// CleanupSupersededSnapshots 回收过了保留期的待回收快照。
//
// 必须排在 CleanupUnreferencedBlobs 之前：快照没删掉之前它的清单仍然算引用
// （referencedShas 扫的是全部快照），blob 永远轮不到回收。
func (u *UploadDao) CleanupSupersededSnapshots(retention time.Duration) (int, error) {
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	cutoff := time.Now().Add(-retention).Unix()
	dropped := 0
	for _, key := range listRepoKeys() {
		if !IsLocalOrgRepo(key.OrgRepo) {
			continue
		}
		count, err := u.cleanupRepoSupersededSnapshots(key.RepoType, key.OrgRepo, cutoff)
		dropped += count
		if err != nil {
			return dropped, err
		}
	}
	return dropped, nil
}

func (u *UploadDao) cleanupRepoSupersededSnapshots(repoType, orgRepo string, cutoff int64) (int, error) {
	// 仓库锁挡住并发的发布：否则回收可能挤在“标签改指新快照”之前，
	// 把一个马上要被重新指向的快照删掉。
	repoLockKey := uploadRepoLockKey(repoType, orgRepo)
	uploadRepoLocks.Lock(repoLockKey)
	defer uploadRepoLocks.Unlock(repoLockKey)

	dropped := 0
	for commit, marker := range listSupersededMarkers(repoType, orgRepo) {
		if marker.SupersededAt > cutoff {
			continue
		}
		// 保留期内可能有人把某个标签重新指了回来（撤销），那它就不再是待回收的。
		if commitHasTags(repoType, orgRepo, commit) {
			continue
		}
		if err := u.dropSnapshotFiles(repoType, orgRepo, commit); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}
