package dao

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/util"

	"github.com/bytedance/sonic"
)

func treeParam(revision, baseCommit string, files ...LocalManifestFile) LocalPublishTreeParam {
	return LocalPublishTreeParam{
		RepoType:   "models",
		Org:        "dingo-local",
		Repo:       "demo",
		Revision:   revision,
		BaseCommit: baseCommit,
		Files:      files,
	}
}

func mustPublishTree(t *testing.T, u *UploadDao, param LocalPublishTreeParam) *LocalPublishTreeResult {
	t.Helper()
	result, err := u.PublishTree(param)
	if err != nil {
		t.Fatalf("publish tree failed: %v", err)
	}
	return result
}

func commitDirExists(repos, repoType, orgRepo, commit string) bool {
	info, err := os.Stat(filepath.Join(repos, "api", repoType, filepath.FromSlash(orgRepo), "revision", commit))
	return err == nil && info.IsDir()
}

// backdateSupersededMarker 把墓碑的时间戳往前挪，用来确定性地触发保留期到期，
// 而不是让测试去睡等真实时间。
func backdateSupersededMarker(t *testing.T, repoType, orgRepo, commit string, age time.Duration) {
	t.Helper()
	path := supersededMarkerPath(repoType, orgRepo, commit)
	b, err := util.ReadFileToBytes(path)
	if err != nil {
		t.Fatalf("read superseded marker for %s failed: %v", commit, err)
	}
	var marker supersededMarker
	if err = sonic.Unmarshal(b, &marker); err != nil {
		t.Fatalf("decode superseded marker for %s failed: %v", commit, err)
	}
	marker.SupersededAt = time.Now().Add(-age).Unix()
	if err = util.WriteDataToFileAtomic(path, marker); err != nil {
		t.Fatalf("rewrite superseded marker for %s failed: %v", commit, err)
	}
}

// seedRevision 发布两个文件，返回该 revision 的当前快照标识与两条清单。
func seedRevision(t *testing.T, u *UploadDao, revision string) (string, LocalManifestFile, LocalManifestFile) {
	t.Helper()
	keep, drop := []byte("keep-me"), []byte("drop-me")
	mustStage(t, u, deferredParam("config.json", keep), keep)
	mustStage(t, u, deferredParam("weights/tmp/old.bin", drop), drop)
	keepItem := manifestItem("config.json", keep)
	dropItem := manifestItem("weights/tmp/old.bin", drop)
	result := mustPublish(t, u, publishParam(revision, keepItem, dropItem))
	return result.Commit, keepItem, dropItem
}

func TestPublishTreeAppliesAdditionsAndDeletionsInOneCommit(t *testing.T) {
	u, repos := newTestUploadDao(t)
	base, keepItem, _ := seedRevision(t, u, "main")

	added := []byte("brand-new-weights")
	mustStage(t, u, deferredParam("weights/new.bin", added), added)
	addedItem := manifestItem("weights/new.bin", added)

	before := len(commitDirs(t, repos, "models", "dingo-local/demo"))
	result := mustPublishTree(t, u, treeParam("main", base, keepItem, addedItem))

	if !result.Changed || result.Status != "published" {
		t.Fatalf("expected a published change, got status=%q changed=%t", result.Status, result.Changed)
	}
	if result.PreviousCommit != base {
		t.Fatalf("previousCommit = %s, want %s", result.PreviousCommit, base)
	}
	if result.Commit == base {
		t.Fatal("commit must differ from the base commit")
	}
	if result.Added != 1 || result.Removed != 1 || result.Unchanged != 1 || result.Replaced != 0 {
		t.Fatalf("diff stats = added:%d removed:%d unchanged:%d replaced:%d, want 1/1/1/0",
			result.Added, result.Removed, result.Unchanged, result.Replaced)
	}
	if result.FileCount != 2 {
		t.Fatalf("fileCount = %d, want 2", result.FileCount)
	}
	// 一次编辑只产生一个新快照，删除不额外生成中间快照。
	if after := len(commitDirs(t, repos, "models", "dingo-local/demo")); after != before+1 {
		t.Fatalf("snapshot count went %d -> %d, want exactly one new snapshot", before, after)
	}
	if got := revisionCommit(t, u, "models", "dingo-local/demo", "main"); got != result.Commit {
		t.Fatalf("revision points at %s, want %s", got, result.Commit)
	}
	manifest := u.readManifest("models", "dingo-local/demo", result.Commit)
	if len(manifest) != 2 {
		t.Fatalf("new manifest has %d entries, want 2", len(manifest))
	}
	for _, item := range manifest {
		if item.Path == "weights/tmp/old.bin" {
			t.Fatal("deleted path is still present in the new manifest")
		}
	}
}

func TestPublishTreeRejectsStaleBaseCommit(t *testing.T) {
	u, _ := newTestUploadDao(t)
	base, keepItem, _ := seedRevision(t, u, "main")

	// 另一个人先提交了一次，revision 已经不再指向 base。
	mustPublishTree(t, u, treeParam("main", base, keepItem))

	_, err := u.PublishTree(treeParam("main", base, keepItem))
	if err == nil {
		t.Fatal("expected a conflict when the base commit is stale")
	}
	coded, ok := err.(interface {
		StatusCode() int
		ErrorCode() string
	})
	if !ok {
		t.Fatalf("error does not carry a status code: %v", err)
	}
	if coded.StatusCode() != 409 || coded.ErrorCode() != "REVISION_CHANGED" {
		t.Fatalf("got %d/%s, want 409/REVISION_CHANGED", coded.StatusCode(), coded.ErrorCode())
	}
}

// 快照是内容寻址的：两个版本标签只要文件集合相同就共用同一个目录。
// 编辑其中一个而回收旧快照，会把另一个标签指向的内容一起抹掉。
func TestPublishTreeKeepsSnapshotStillTaggedByAnotherRevision(t *testing.T) {
	u, repos := newTestUploadDao(t)
	base, keepItem, dropItem := seedRevision(t, u, "main")

	shared := mustPublish(t, u, publishParam("v1", keepItem, dropItem))
	if shared.Commit != base {
		t.Fatalf("v1 commit = %s, want the same snapshot as main (%s)", shared.Commit, base)
	}

	mustPublishTree(t, u, treeParam("main", base, keepItem))

	if !commitDirExists(repos, "models", "dingo-local/demo", base) {
		t.Fatal("snapshot still tagged by v1 was removed")
	}
	if util.FileExists(supersededMarkerPath("models", "dingo-local/demo", base)) {
		t.Fatal("snapshot still tagged by v1 must not be marked superseded")
	}
	if got := revisionCommit(t, u, "models", "dingo-local/demo", "v1"); got != base {
		t.Fatalf("v1 now points at %s, want %s", got, base)
	}
	if len(u.readManifest("models", "dingo-local/demo", base)) != 2 {
		t.Fatal("v1 no longer resolves to its full manifest")
	}
}

func TestPublishTreeMarksSupersededSnapshotAndCleanupDropsIt(t *testing.T) {
	u, repos := newTestUploadDao(t)
	base, keepItem, _ := seedRevision(t, u, "main")

	mustPublishTree(t, u, treeParam("main", base, keepItem))

	// 保留期内旧快照仍然完整，在途下载按旧 sha 请求不会 404。
	if !commitDirExists(repos, "models", "dingo-local/demo", base) {
		t.Fatal("superseded snapshot was dropped immediately")
	}
	if !util.FileExists(supersededMarkerPath("models", "dingo-local/demo", base)) {
		t.Fatal("superseded snapshot was not marked")
	}
	if len(u.readManifest("models", "dingo-local/demo", base)) != 2 {
		t.Fatal("superseded snapshot no longer serves its manifest")
	}

	dropped, err := u.CleanupSupersededSnapshots(24 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("cleanup dropped %d snapshot(s) before the retention elapsed", dropped)
	}

	backdateSupersededMarker(t, "models", "dingo-local/demo", base, 48*time.Hour)
	dropped, err = u.CleanupSupersededSnapshots(24 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("cleanup dropped %d snapshot(s), want 1", dropped)
	}
	if commitDirExists(repos, "models", "dingo-local/demo", base) {
		t.Fatal("expired snapshot survived cleanup")
	}

	// 快照消失之后，只被它引用的内容才轮得到 blob 回收。
	referenced, err := u.referencedShas(config.SysConfig.Repos(), "models", "dingo-local/demo")
	if err != nil {
		t.Fatalf("collect referenced shas failed: %v", err)
	}
	if _, ok := referenced[manifestItem("weights/tmp/old.bin", []byte("drop-me")).Sha256]; ok {
		t.Fatal("deleted content is still referenced after its snapshot was dropped")
	}
}

func TestPublishTreeKeepsOnlyOneSupersededSnapshotPerRevision(t *testing.T) {
	u, repos := newTestUploadDao(t)
	base, keepItem, _ := seedRevision(t, u, "main")

	first := mustPublishTree(t, u, treeParam("main", base, keepItem))
	// 第二次编辑必须落到一份全新的内容上。加回 dropItem 会让清单与 base 逐字符相同，
	// 算出的标识也就是 base，那测的就不是“留几个待回收快照”而是撤销了。
	extra := []byte("third file")
	mustStage(t, u, deferredParam("extra.bin", extra), extra)
	second := mustPublishTree(t, u, treeParam("main", first.Commit, keepItem, manifestItem("extra.bin", extra)))

	if commitDirExists(repos, "models", "dingo-local/demo", base) {
		t.Fatal("the older superseded snapshot should have been dropped by the next edit")
	}
	if !commitDirExists(repos, "models", "dingo-local/demo", first.Commit) {
		t.Fatal("the most recent superseded snapshot must be kept for the retention window")
	}
	if got := revisionCommit(t, u, "models", "dingo-local/demo", "main"); got != second.Commit {
		t.Fatalf("revision points at %s, want %s", got, second.Commit)
	}
}

// 撤销一次编辑要能把快照原样找回来。
//
// 标识是内容的摘要，把删掉的文件加回来算出的就是被顶下去的那个快照本身。
// 它重新被标签指着，就不再是待回收的了——墓碑得摘掉，快照更不能删。
func TestPublishTreeKeepsASnapshotThatIsPointedAtAgain(t *testing.T) {
	u, repos := newTestUploadDao(t)
	base, keepItem, dropItem := seedRevision(t, u, "main")

	first := mustPublishTree(t, u, treeParam("main", base, keepItem))
	undo := mustPublishTree(t, u, treeParam("main", first.Commit, keepItem, dropItem))

	if undo.Commit != base {
		t.Fatalf("undo landed on %s, want the original snapshot %s", undo.Commit, base)
	}
	if !commitDirExists(repos, "models", "dingo-local/demo", base) {
		t.Fatal("the snapshot the revision points at was dropped")
	}
	if _, marked := listSupersededMarkers("models", "dingo-local/demo")[base]; marked {
		t.Fatal("a snapshot that is tagged again must not stay marked for reclamation")
	}
	manifest, err := u.fileDao.ReadLocalManifest("models", "dingo-local/demo", base)
	if err != nil {
		t.Fatalf("read the restored manifest failed: %v", err)
	}
	if len(manifest) != 2 {
		t.Fatalf("restored manifest lists %d files, want 2", len(manifest))
	}
}

// 快照标识是清单内容的摘要，重放同一次编辑必然算出同一个标识。
// 这时候不能重写元数据，也不能凭空多出一个待回收快照。
func TestPublishTreeReplayIsIdempotent(t *testing.T) {
	u, repos := newTestUploadDao(t)
	base, keepItem, _ := seedRevision(t, u, "main")

	first := mustPublishTree(t, u, treeParam("main", base, keepItem))
	before := len(commitDirs(t, repos, "models", "dingo-local/demo"))

	replay := mustPublishTree(t, u, treeParam("main", first.Commit, keepItem))
	if replay.Changed || replay.Status != "unchanged" {
		t.Fatalf("replay reported status=%q changed=%t, want unchanged/false", replay.Status, replay.Changed)
	}
	if replay.Commit != first.Commit || replay.PreviousCommit != first.Commit {
		t.Fatalf("replay moved the pointer: %s -> %s", replay.PreviousCommit, replay.Commit)
	}
	if after := len(commitDirs(t, repos, "models", "dingo-local/demo")); after != before {
		t.Fatalf("replay changed the snapshot count %d -> %d", before, after)
	}
}

// 删掉一个文件再把同一份内容加回来，标识会回到删除之前那个值。
// 调用方因此必须看 previousCommit 而不是“commit 变没变”来判断改动是否生效。
func TestPublishTreeRevertingContentReturnsToTheEarlierCommit(t *testing.T) {
	u, _ := newTestUploadDao(t)
	base, keepItem, dropItem := seedRevision(t, u, "main")

	trimmed := mustPublishTree(t, u, treeParam("main", base, keepItem))
	restored := mustPublishTree(t, u, treeParam("main", trimmed.Commit, keepItem, dropItem))

	if restored.Commit != base {
		t.Fatalf("restored commit = %s, want the original %s", restored.Commit, base)
	}
	if !restored.Changed {
		t.Fatal("restoring a file must report a change even though the commit id is an old one")
	}
	if restored.PreviousCommit != trimmed.Commit {
		t.Fatalf("previousCommit = %s, want %s", restored.PreviousCommit, trimmed.Commit)
	}
}

func TestPublishTreeRejectsContentThatIsNotOnDisk(t *testing.T) {
	u, _ := newTestUploadDao(t)
	base, keepItem, _ := seedRevision(t, u, "main")

	ghost := manifestItem("weights/never-uploaded.bin", []byte("nothing-was-staged-for-this"))
	_, err := u.PublishTree(treeParam("main", base, keepItem, ghost))
	if err == nil {
		t.Fatal("expected a conflict when the target manifest references missing content")
	}
	coded, ok := err.(interface{ ErrorCode() string })
	if !ok || coded.ErrorCode() != "PUBLISH_CONTENT_NOT_READY" {
		t.Fatalf("got %v, want PUBLISH_CONTENT_NOT_READY", err)
	}
	if got := revisionCommit(t, u, "models", "dingo-local/demo", "main"); got != base {
		t.Fatalf("a rejected edit moved the pointer to %s", got)
	}
}

func TestPublishTreeRejectsUnknownRevision(t *testing.T) {
	u, _ := newTestUploadDao(t)
	_, keepItem, _ := seedRevision(t, u, "main")

	_, err := u.PublishTree(treeParam("v9", manifestCommitOf(t, keepItem), keepItem))
	if err == nil {
		t.Fatal("expected an error when editing a revision that does not exist")
	}
	coded, ok := err.(interface{ ErrorCode() string })
	if !ok || coded.ErrorCode() != "REVISION_NOT_FOUND" {
		t.Fatalf("got %v, want REVISION_NOT_FOUND", err)
	}
}

func manifestCommitOf(t *testing.T, files ...LocalManifestFile) string {
	t.Helper()
	commit, err := manifestCommit(files)
	if err != nil {
		t.Fatalf("compute manifest commit failed: %v", err)
	}
	return commit
}

// 空快照要能被创建、被写满、再被清空，而且“没有文件”在系统里只有一种标识。
//
// 新建仓库、新建 revision 走的都是“先发一份空清单”，用户之后才有地方加文件；
// 删掉最后一个文件走的是整树发布，目标清单为空。两条路径最终落在同一个状态上，
// 算出的标识必须逐字符相同——不同就意味着同一个“空”被当成了两个版本。
func TestEmptyRevisionRoundTrip(t *testing.T) {
	u, repos := newTestUploadDao(t)

	created := mustPublish(t, u, publishParam("v2"))
	if created.FileCount != 0 {
		t.Fatalf("new empty revision has %d files, want 0", created.FileCount)
	}
	if len(created.Commit) != 64 {
		t.Fatalf("new empty revision did not get a commit: %q", created.Commit)
	}
	if got := revisionCommit(t, u, "models", "dingo-local/demo", "v2"); got != created.Commit {
		t.Fatalf("revision v2 points at %s, want %s", got, created.Commit)
	}
	if !commitDirExists(repos, "models", "dingo-local/demo", created.Commit) {
		t.Fatalf("empty snapshot %s was not written", created.Commit)
	}

	content := []byte("first file")
	mustStage(t, u, deferredParam("config.yaml", content), content)
	item := manifestItem("config.yaml", content)
	filled := mustPublishTree(t, u, treeParam("v2", created.Commit, item))
	if filled.FileCount != 1 || filled.Added != 1 {
		t.Fatalf("adding to an empty revision reported %+v", filled)
	}

	cleared := mustPublishTree(t, u, treeParam("v2", filled.Commit))
	if cleared.FileCount != 0 || cleared.Removed != 1 {
		t.Fatalf("clearing the revision reported %+v", cleared)
	}
	// 清空之后回到的必须是同一个空快照，而不是第二个“空”。
	if cleared.Commit != created.Commit {
		t.Fatalf("cleared revision points at %s, want the canonical empty commit %s", cleared.Commit, created.Commit)
	}
	if got := revisionCommit(t, u, "models", "dingo-local/demo", "v2"); got != created.Commit {
		t.Fatalf("revision v2 points at %s after clearing, want %s", got, created.Commit)
	}
	manifest, err := u.fileDao.ReadLocalManifest("models", "dingo-local/demo", cleared.Commit)
	if err != nil {
		t.Fatalf("read the cleared manifest failed: %v", err)
	}
	if len(manifest) != 0 {
		t.Fatalf("cleared manifest still lists %d files", len(manifest))
	}
}
