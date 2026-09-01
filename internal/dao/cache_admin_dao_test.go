package dao

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/util"

	"github.com/bytedance/sonic"
)

// 缓存管理的两级删除。测试的组织方式与实现的风险点一一对应：
//
//	A 扫描与列表    —— 页面看到的东西是不是磁盘上真实的东西
//	B 一级删除·本地  —— 必须动清单，只删 resolve 是无效的
//	C 一级删除·远端  —— 删 paths-info + resolve，不碰 blob
//	D 二级删除      —— 真删 blob，且挡得住竞态与在途传输
//	E 自动回收      —— 墓碑决定保留期起点；远端只回收有墓碑的
//	F 输入校验      —— orgRepo 直接参与拼路径

func newTestCacheAdminDao(t *testing.T) (*CacheAdminDao, *UploadDao, string) {
	t.Helper()
	u, repos := newTestUploadDao(t)
	return NewCacheAdminDao(u.fileDao), u, repos
}

// ---------------------------------------------------------------------------
// 远端缓存的造数：下载链路会落 blob + resolve 链接 + paths-info 缓存，这里照着造。
// ---------------------------------------------------------------------------

func writeBlobContent(t *testing.T, blobPath string, content []byte) {
	t.Helper()
	if err := util.MakeDirs(blobPath); err != nil {
		t.Fatalf("make blob dir failed: %v", err)
	}
	dingFile, err := downloader.NewDingCache(blobPath, testBlockSize)
	if err != nil {
		t.Fatalf("open blob failed: %v", err)
	}
	defer dingFile.Close()
	if err = dingFile.Resize(int64(len(content))); err != nil {
		t.Fatalf("resize blob failed: %v", err)
	}
	blockSize := int64(testBlockSize)
	for offset := int64(0); offset < int64(len(content)); offset += blockSize {
		end := offset + blockSize
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		block := make([]byte, blockSize)
		copy(block, content[offset:end])
		if err = dingFile.WriteBlock(offset/blockSize, block); err != nil {
			t.Fatalf("write block failed: %v", err)
		}
	}
}

func writePathsInfo(t *testing.T, fileDao *FileDao, repoType, orgRepo, commit, path, oid string, size int64) {
	t.Helper()
	body, err := sonic.Marshal([]map[string]interface{}{{
		"type": "file",
		"oid":  oid,
		"size": size,
		"path": path,
		"lfs":  map[string]interface{}{"oid": oid, "size": size},
	}})
	if err != nil {
		t.Fatalf("marshal paths-info failed: %v", err)
	}
	infoPath := filepath.Join(repoApiRoot(repoType, orgRepo), "paths-info", commit, filepath.FromSlash(path), "paths-info_post.json")
	if err = util.MakeDirs(infoPath); err != nil {
		t.Fatalf("make paths-info dir failed: %v", err)
	}
	if err = fileDao.WriteCacheRequest(infoPath, 200, map[string]string{}, body); err != nil {
		t.Fatalf("write paths-info failed: %v", err)
	}
}

// seedRemoteFile 造一份“从上游拉下来并缓存住了”的文件。
func seedRemoteFile(t *testing.T, fileDao *FileDao, orgRepo, commit, path string, content []byte) string {
	t.Helper()
	etag := sha256Hex(content)
	blobPath := BlobPath("models", orgRepo, etag)
	writeBlobContent(t, blobPath, content)
	resolvePath := ResolvePath("models", orgRepo, commit, path)
	if err := util.MakeDirs(resolvePath); err != nil {
		t.Fatalf("make resolve dir failed: %v", err)
	}
	if err := util.CreateLinkOrCopyIfNotExists(blobPath, resolvePath); err != nil {
		t.Fatalf("create resolve link failed: %v", err)
	}
	writePathsInfo(t, fileDao, "models", orgRepo, commit, path, etag, int64(len(content)))
	return etag
}

func findRow(rows []*CacheFileRow, path, sha string) *CacheFileRow {
	for _, row := range rows {
		if row.Path == path && (sha == "" || row.Sha == sha) {
			return row
		}
	}
	return nil
}

func findOrphan(rows []*RecycleRow, sha string) *RecycleRow {
	for _, row := range rows {
		if row.Sha == sha {
			return row
		}
	}
	return nil
}

func tombstoneExists(repoType, orgRepo, sha string) bool {
	return util.FileExists(recycleEntryPath(repoType, orgRepo, sha))
}

func ageTombstone(t *testing.T, repoType, orgRepo, sha string, age time.Duration) {
	t.Helper()
	path := recycleEntryPath(repoType, orgRepo, sha)
	b, err := util.ReadFileToBytes(path)
	if err != nil {
		t.Fatalf("read tombstone failed: %v", err)
	}
	var entry RecycleEntry
	if err = sonic.Unmarshal(b, &entry); err != nil {
		t.Fatalf("unmarshal tombstone failed: %v", err)
	}
	entry.UnlinkedAt = time.Now().Add(-age).Unix()
	if err = util.WriteDataToFileAtomic(path, entry); err != nil {
		t.Fatalf("write tombstone failed: %v", err)
	}
}

// ===========================================================================
// A 扫描与列表
// ===========================================================================

func TestListFilesShowsPublishedUploads(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	weights := []byte("weights payload")
	config := []byte("{\"hidden\":8}")
	mustStage(t, u, deferredParam("weights/model.bin", weights), weights)
	mustStage(t, u, deferredParam("config.json", config), config)
	mustPublish(t, u, publishParam("main",
		manifestItem("weights/model.bin", weights),
		manifestItem("config.json", config)))

	rows := admin.ListFiles("models", orgRepo)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	row := findRow(rows, "weights/model.bin", sha256Hex(weights))
	if row == nil {
		t.Fatalf("uploaded file is missing from the listing: %+v", rows)
	}
	if row.Size != int64(len(weights)) {
		t.Fatalf("size = %d, want %d (size must come from the blob header, not the file on disk)", row.Size, len(weights))
	}
	if !row.Complete || row.CachedBytes != int64(len(weights)) {
		t.Fatalf("published upload must be reported as complete, got complete=%v cached=%d", row.Complete, row.CachedBytes)
	}
	if row.Source != CacheSourceUpload {
		t.Fatalf("source = %q, want %q", row.Source, CacheSourceUpload)
	}
	if len(row.Revisions) == 0 || row.Revisions[0] != "main" {
		t.Fatalf("revision tag main must be resolved back onto the row, got %v", row.Revisions)
	}
}

func TestListFilesKeepsOneRowPerPathForSharedContent(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	shared := []byte("same bytes in two places")
	mustStage(t, u, deferredParam("a.bin", shared), shared)
	mustPublish(t, u, publishParam("main",
		manifestItem("a.bin", shared),
		manifestItem("b.bin", shared)))

	rows := admin.ListFiles("models", orgRepo)
	if len(rows) != 2 {
		t.Fatalf("de-duplicated content must still show one row per path, got %d rows", len(rows))
	}
	if rows[0].Sha != rows[1].Sha {
		t.Fatalf("both rows must point at the same content: %s vs %s", rows[0].Sha, rows[1].Sha)
	}
}

func TestListFilesShowsRemoteCache(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "Qwen/Qwen2.5-0.5B"

	content := []byte("remote model shard")
	etag := seedRemoteFile(t, u.fileDao, orgRepo, "abc123", "model.safetensors", content)

	rows := admin.ListFiles("models", orgRepo)
	row := findRow(rows, "model.safetensors", etag)
	if row == nil {
		t.Fatalf("remote cached file is missing from the listing: %+v", rows)
	}
	if row.Source != CacheSourceRemote {
		t.Fatalf("source = %q, want %q", row.Source, CacheSourceRemote)
	}
	if !row.HasResolve {
		t.Fatalf("a downloaded file must be reported as having a resolve entry")
	}
	if row.Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", row.Size, len(content))
	}
}

// 部分缓存必须显示成部分：页面上把半个文件显示成“完整”会让人误以为删了也能重下。
func TestListFilesReportsPartialCache(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "Qwen/partial"

	full := make([]byte, testBlockSize*4)
	for i := range full {
		full[i] = byte(i)
	}
	etag := seedRemoteFile(t, u.fileDao, orgRepo, "c1", "big.bin", full)

	// 把最后一块的位图打掉，模拟下载到一半。
	blobPath := BlobPath("models", orgRepo, etag)
	clearLastBlock(t, blobPath)

	rows := admin.ListFiles("models", orgRepo)
	row := findRow(rows, "big.bin", etag)
	if row == nil {
		t.Fatalf("row not found")
	}
	if row.Complete {
		t.Fatalf("a partially cached file must not be reported as complete")
	}
	if row.CachedBytes >= row.Size {
		t.Fatalf("cachedBytes = %d must be smaller than size = %d", row.CachedBytes, row.Size)
	}
}

func clearLastBlock(t *testing.T, blobPath string) {
	t.Helper()
	f, err := os.OpenFile(blobPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open blob failed: %v", err)
	}
	defer f.Close()
	header := &downloader.DingCacheHeader{}
	if err = header.Read(f); err != nil {
		t.Fatalf("read header failed: %v", err)
	}
	if err = header.BlockMask.Clear(header.BlockNumber - 1); err != nil {
		t.Fatalf("clear block mask failed: %v", err)
	}
	if _, err = f.Seek(0, 0); err != nil {
		t.Fatalf("seek failed: %v", err)
	}
	if err = header.Write(f); err != nil {
		t.Fatalf("write header failed: %v", err)
	}
}

// 列表接口只读，不能在扫描过程中改动缓存——readBlobStat 若误用 NewDingCache
// 就会以 O_RDWR 打开并可能重写头部，页面刷一下就把缓存改了。
func TestListFilesDoesNotMutateCache(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "Qwen/readonly"

	content := []byte("must not be touched")
	etag := seedRemoteFile(t, u.fileDao, orgRepo, "c1", "f.bin", content)
	blobPath := BlobPath("models", orgRepo, etag)

	before, err := os.Stat(blobPath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err = os.Chtimes(blobPath, old, old); err != nil {
		t.Fatalf("chtimes failed: %v", err)
	}

	admin.ListFiles("models", orgRepo)

	after, err := os.Stat(blobPath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if !after.ModTime().Equal(old) {
		t.Fatalf("listing must not touch the blob (mtime changed from %v to %v)", old, after.ModTime())
	}
	if after.Size() != before.Size() {
		t.Fatalf("listing must not resize the blob: %d -> %d", before.Size(), after.Size())
	}
}

func TestListReposAggregatesBothSources(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)

	payload := []byte("published")
	mustStage(t, u, deferredParam("a.bin", payload), payload)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))
	seedRemoteFile(t, u.fileDao, "Qwen/remote-demo", "c1", "f.bin", []byte("remote"))

	repos := admin.ListRepos()
	var upload, remote *CacheRepo
	for _, repo := range repos {
		switch repo.Source {
		case CacheSourceUpload:
			upload = repo
		case CacheSourceRemote:
			remote = repo
		}
	}
	if upload == nil || remote == nil {
		t.Fatalf("both an upload repo and a remote repo must be listed, got %+v", repos)
	}
	if upload.FileCount != 1 || upload.TotalSize != int64(len(payload)) {
		t.Fatalf("upload repo aggregate = %+v", upload)
	}
	if remote.FileCount != 1 {
		t.Fatalf("remote repo aggregate = %+v", remote)
	}
}

// ===========================================================================
// B 一级删除·本地上传
// ===========================================================================

// 这是整个功能最关键的一条：本地上传的权威引用是清单，不是 resolve 链接。
// 只删链接的话 blob 永远不会变成未引用，也就永远进不了回收站。
func TestSoftDeleteUploadRemovesEntryFromEverySnapshot(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	target := []byte("content to be deleted")
	other := []byte("unrelated content")
	mustStage(t, u, deferredParam("target.bin", target), target)
	first := mustPublish(t, u, publishParam("main", manifestItem("target.bin", target)))

	// 再发一版：旧快照仍然引用 target.bin，删除时必须把两份清单都摘掉。
	mustStage(t, u, deferredParam("other.bin", other), other)
	second := mustPublish(t, u, publishParam("main",
		manifestItem("target.bin", target),
		manifestItem("other.bin", other)))
	if first.Commit == second.Commit {
		t.Fatalf("test setup is wrong: expected two distinct snapshots")
	}

	sha := sha256Hex(target)
	results, err := admin.SoftDelete([]DeleteItem{{RepoType: "models", OrgRepo: orgRepo, Path: "target.bin", Sha: sha}})
	if err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}
	if len(results) != 1 || results[0].Status != "deleted" {
		t.Fatalf("soft delete result = %+v", results[0])
	}

	// 旧标识整体作废：标识是清单内容的摘要，内容变了就不能继续用同一个标识。
	for _, commit := range []string{first.Commit, second.Commit} {
		if util.FileExists(LocalManifestPath("models", orgRepo, commit)) {
			t.Fatalf("snapshot %s must be superseded, not rewritten in place", commit)
		}
	}
	// 版本标签改指到新的快照上，其余文件原样保留。
	current := mustReadManifest(t, "models", orgRepo, revisionCommit(t, u, "models", orgRepo, "main"))
	if _, found := findManifestFile(current, "target.bin"); found {
		t.Fatalf("the current snapshot still references the deleted file")
	}
	if _, found := findManifestFile(current, "other.bin"); !found {
		t.Fatalf("deleting one file must not disturb the rest of the snapshot")
	}

	// 内容还在盘上——一级删除不碰 blob。
	if !blobExists("models", orgRepo, sha) {
		t.Fatalf("soft delete must not remove the blob")
	}
	if !tombstoneExists("models", orgRepo, sha) {
		t.Fatalf("soft delete must leave a recycle tombstone")
	}
	if findRow(admin.ListFiles("models", orgRepo), "target.bin", sha) != nil {
		t.Fatalf("deleted file must disappear from the file listing")
	}
	if findOrphan(admin.ListOrphans("models", orgRepo), sha) == nil {
		t.Fatalf("deleted content must show up in the recycle bin")
	}
}

func mustReadManifest(t *testing.T, repoType, orgRepo, commit string) []LocalManifestFile {
	t.Helper()
	manifest, err := readManifestFile(LocalManifestPath(repoType, orgRepo, commit))
	if err != nil {
		t.Fatalf("read manifest failed: %v", err)
	}
	return manifest
}

// 元数据是清单的派生物：漏改 meta_get 的话文件树接口还会把已删的文件列出来。
func TestSoftDeleteRewritesRepoMetadata(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	payload := []byte("listed in metadata")
	keep := []byte("still there")
	mustStage(t, u, deferredParam("gone.bin", payload), payload)
	mustStage(t, u, deferredParam("keep.bin", keep), keep)
	before := mustPublish(t, u, publishParam("main",
		manifestItem("gone.bin", payload),
		manifestItem("keep.bin", keep)))

	if _, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "gone.bin", Sha: sha256Hex(payload),
	}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}

	after := revisionCommit(t, u, "models", orgRepo, "main")
	if after == before.Commit {
		t.Fatalf("the revision tag must move to a new snapshot identity after a deletion")
	}
	for _, revision := range []string{"main", after} {
		siblings := readSiblings(t, "models", orgRepo, revision)
		if _, ok := siblings["gone.bin"]; ok {
			t.Fatalf("revision %s metadata still lists the deleted file", revision)
		}
		if _, ok := siblings["keep.bin"]; !ok {
			t.Fatalf("revision %s metadata lost an unrelated file", revision)
		}
	}

	// 清单缓存必须被显式清掉，否则下载侧还会读到旧清单。
	if _, err := u.fileDao.ReadLocalManifest("models", orgRepo, before.Commit); err == nil {
		t.Fatalf("FileDao still serves the superseded snapshot from its cache")
	}
	manifest, err := u.fileDao.ReadLocalManifest("models", orgRepo, after)
	if err != nil {
		t.Fatalf("read manifest through FileDao failed: %v", err)
	}
	if _, found := findManifestFile(manifest, "gone.bin"); found {
		t.Fatalf("the new snapshot still contains the deleted file")
	}
}

func readSiblings(t *testing.T, repoType, orgRepo, revision string) map[string]struct{} {
	t.Helper()
	metaPath := filepath.Join(repoApiRoot(repoType, orgRepo), "revision", revision, "meta_get.json")
	body, err := readCacheContent(metaPath)
	if err != nil {
		t.Fatalf("read meta %s failed: %v", revision, err)
	}
	var meta struct {
		Siblings []struct {
			RFilename string `json:"rfilename"`
		} `json:"siblings"`
	}
	if err = sonic.Unmarshal(body, &meta); err != nil {
		t.Fatalf("unmarshal meta failed: %v", err)
	}
	result := make(map[string]struct{})
	for _, item := range meta.Siblings {
		result[item.RFilename] = struct{}{}
	}
	return result
}

// 去重内容：删掉其中一个路径不能让内容进回收站，另一个路径还在用它。
func TestSoftDeleteSharedContentWaitsForTheLastPath(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	shared := []byte("referenced twice")
	sha := sha256Hex(shared)
	mustStage(t, u, deferredParam("a.bin", shared), shared)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", shared), manifestItem("b.bin", shared)))

	results, err := admin.SoftDelete([]DeleteItem{{RepoType: "models", OrgRepo: orgRepo, Path: "a.bin", Sha: sha}})
	if err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}
	if results[0].Status != "deleted" {
		t.Fatalf("result = %+v", results[0])
	}
	if tombstoneExists("models", orgRepo, sha) {
		t.Fatalf("content that is still referenced must not enter the recycle bin")
	}
	if findOrphan(admin.ListOrphans("models", orgRepo), sha) != nil {
		t.Fatalf("still-referenced content must not be listed in the recycle bin")
	}

	if _, err = admin.SoftDelete([]DeleteItem{{RepoType: "models", OrgRepo: orgRepo, Path: "b.bin", Sha: sha}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}
	if !tombstoneExists("models", orgRepo, sha) {
		t.Fatalf("the last reference going away must produce a tombstone")
	}
}

func TestSoftDeleteIsIdempotent(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	payload := []byte("delete me twice")
	sha := sha256Hex(payload)
	mustStage(t, u, deferredParam("a.bin", payload), payload)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))

	item := DeleteItem{RepoType: "models", OrgRepo: orgRepo, Path: "a.bin", Sha: sha}
	if _, err := admin.SoftDelete([]DeleteItem{item}); err != nil {
		t.Fatalf("first delete failed: %v", err)
	}
	results, err := admin.SoftDelete([]DeleteItem{item})
	if err != nil {
		t.Fatalf("second delete failed: %v", err)
	}
	if results[0].Status != "skipped" {
		t.Fatalf("deleting an already deleted entry must be skipped, got %+v", results[0])
	}
	if !blobExists("models", orgRepo, sha) {
		t.Fatalf("a repeated soft delete must not escalate into removing the blob")
	}
}

// 需求里明确要的行为：删掉之后重新上传同一份文件，引用要能重新建立起来，
// 而且因为内容还在盘上，走的是幂等快路径（秒传）。
func TestReuploadAfterSoftDeleteReusesContentAndVoidsTombstone(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	payload := []byte("re-uploaded later")
	sha := sha256Hex(payload)
	mustStage(t, u, deferredParam("a.bin", payload), payload)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))
	if _, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "a.bin", Sha: sha,
	}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}

	staged := mustStage(t, u, deferredParam("a.bin", payload), payload)
	if !staged.BlobReused {
		t.Fatalf("content still on disk must be reused instead of re-written")
	}
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))

	if findRow(admin.ListFiles("models", orgRepo), "a.bin", sha) == nil {
		t.Fatalf("re-published file must be visible again")
	}
	if findOrphan(admin.ListOrphans("models", orgRepo), sha) != nil {
		t.Fatalf("a tombstone for content that is referenced again must be void")
	}

	// 作废的墓碑由持有仓库锁的回收任务清掉，而不是在无锁的列表路径上顺手删。
	if _, err := u.CleanupUnreferencedBlobs(168 * time.Hour); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if tombstoneExists("models", orgRepo, sha) {
		t.Fatalf("the cleanup sweep must drop the void tombstone")
	}
	if !blobExists("models", orgRepo, sha) {
		t.Fatalf("re-referenced content must never be reclaimed")
	}
}

// ===========================================================================
// C 一级删除·远端缓存
// ===========================================================================

func TestSoftDeleteRemoteRemovesResolveAndPathsInfo(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "Qwen/Qwen2.5-0.5B"

	content := []byte("cached from upstream")
	etag := seedRemoteFile(t, u.fileDao, orgRepo, "c1", "model.bin", content)

	results, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "model.bin", Sha: etag,
	}})
	if err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}
	if results[0].Status != "deleted" {
		t.Fatalf("result = %+v", results[0])
	}

	if _, statErr := os.Lstat(ResolvePath("models", orgRepo, "c1", "model.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("resolve entry must be removed, stat err = %v", statErr)
	}
	infoPath := filepath.Join(repoApiRoot("models", orgRepo), "paths-info", "c1", "model.bin", "paths-info_post.json")
	if util.FileExists(infoPath) {
		t.Fatalf("paths-info cache must be removed as well")
	}
	if !blobExists("models", orgRepo, etag) {
		t.Fatalf("soft delete must not remove the blob")
	}
	if !tombstoneExists("models", orgRepo, etag) {
		t.Fatalf("soft delete must leave a recycle tombstone")
	}
	if findOrphan(admin.ListOrphans("models", orgRepo), etag) == nil {
		t.Fatalf("deleted remote content must show up in the recycle bin")
	}
}

// 远端 blob 没有墓碑就不该出现在回收站里：resolve 可能只是被 diskClean 的 LRU
// 删掉了，把它显示成“待彻底删除”会怂恿用户删掉正常的缓存。
func TestRemoteBlobWithoutTombstoneIsNotListedAsOrphan(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "Qwen/lru-victim"

	content := []byte("resolve was evicted by lru")
	etag := seedRemoteFile(t, u.fileDao, orgRepo, "c1", "f.bin", content)
	// 模拟 LRU：直接删掉链接与 paths-info，blob 留着。
	_ = os.Remove(ResolvePath("models", orgRepo, "c1", "f.bin"))
	_ = os.RemoveAll(filepath.Join(repoApiRoot("models", orgRepo), "paths-info"))

	if findOrphan(admin.ListOrphans("models", orgRepo), etag) != nil {
		t.Fatalf("an unreferenced remote blob without a tombstone must not be listed as an orphan")
	}
}

// ===========================================================================
// D 二级删除
// ===========================================================================

func TestPurgeRemovesBlobAndTombstone(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	payload := []byte("purge me")
	sha := sha256Hex(payload)
	mustStage(t, u, deferredParam("a.bin", payload), payload)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))
	if _, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "a.bin", Sha: sha,
	}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}

	results, err := admin.PurgeOrphans([]DeleteItem{{RepoType: "models", OrgRepo: orgRepo, Sha: sha}})
	if err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	if results[0].Status != "deleted" {
		t.Fatalf("purge result = %+v", results[0])
	}
	if blobExists("models", orgRepo, sha) {
		t.Fatalf("purge must remove the blob")
	}
	if tombstoneExists("models", orgRepo, sha) {
		t.Fatalf("purge must remove the tombstone")
	}
}

// 从页面加载到点确认之间可能有一次发布把内容重新引用起来，此时必须跳过。
func TestPurgeSkipsContentThatBecameReferencedAgain(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	payload := []byte("raced with a publish")
	sha := sha256Hex(payload)
	mustStage(t, u, deferredParam("a.bin", payload), payload)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))
	if _, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "a.bin", Sha: sha,
	}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}
	// 用户还盯着回收站页面时，另一条链路把它重新发布了。
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))

	results, err := admin.PurgeOrphans([]DeleteItem{{RepoType: "models", OrgRepo: orgRepo, Sha: sha}})
	if err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	if results[0].Status != "skipped" {
		t.Fatalf("purge must skip content that is referenced again, got %+v", results[0])
	}
	if !blobExists("models", orgRepo, sha) {
		t.Fatalf("re-referenced content must survive the purge")
	}
}

// 正在被传输的文件不能删：Linux 上 unlink 之后写入会全部落到孤儿 inode 上，
// 表现为一个明明“下载完了”却读不出内容的文件。
func TestPurgeSkipsContentInUse(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	payload := []byte("currently downloading")
	sha := sha256Hex(payload)
	mustStage(t, u, deferredParam("a.bin", payload), payload)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))
	if _, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "a.bin", Sha: sha,
	}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}

	blobPath := localBlobPath("models", orgRepo, sha)
	if _, err := downloader.GetInstance().GetDingFile(blobPath, int64(len(payload))); err != nil {
		t.Fatalf("acquire handle failed: %v", err)
	}
	defer downloader.GetInstance().ReleasedDingFile(blobPath)

	results, err := admin.PurgeOrphans([]DeleteItem{{RepoType: "models", OrgRepo: orgRepo, Sha: sha}})
	if err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	if results[0].Status != "failed" {
		t.Fatalf("purging content that is in use must fail loudly, got %+v", results[0])
	}
	if !blobExists("models", orgRepo, sha) {
		t.Fatalf("content in use must not be removed")
	}
}

// ===========================================================================
// E 自动回收
// ===========================================================================

// 回归测试：没有墓碑机制的话，一个很久以前上传的文件被删进回收站之后，
// 下一个清理周期（最多一小时）就会按 mtime 判定为过期并彻底删掉，回收站形同虚设。
func TestRecycledUploadRetentionStartsAtDeletionNotAtBlobMTime(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	payload := []byte("uploaded long ago")
	sha := sha256Hex(payload)
	mustStage(t, u, deferredParam("a.bin", payload), payload)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))
	ageBlob(t, "models", orgRepo, sha, 200*time.Hour)

	if _, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "a.bin", Sha: sha,
	}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}

	removed, err := u.CleanupUnreferencedBlobs(168 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 0 || !blobExists("models", orgRepo, sha) {
		t.Fatalf("content just moved to the recycle bin must survive its retention window (removed=%d)", removed)
	}
}

func TestRecycledUploadIsReclaimedAfterRetention(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	payload := []byte("expired in the recycle bin")
	sha := sha256Hex(payload)
	mustStage(t, u, deferredParam("a.bin", payload), payload)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))
	if _, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "a.bin", Sha: sha,
	}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}
	ageTombstone(t, "models", orgRepo, sha, 200*time.Hour)

	removed, err := u.CleanupUnreferencedBlobs(168 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 reclaimed file, got %d", removed)
	}
	if blobExists("models", orgRepo, sha) {
		t.Fatalf("expired recycle bin content must be removed")
	}
	if tombstoneExists("models", orgRepo, sha) {
		t.Fatalf("the tombstone must be removed together with the content")
	}
}

// 老行为不能变：没有墓碑的“暂缓生效上传等不到发布”仍按 blob mtime 判定。
func TestLegacyUnreferencedContentStillUsesBlobMTime(t *testing.T) {
	_, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	abandoned := []byte("staged but never published")
	sha := sha256Hex(abandoned)
	mustStage(t, u, deferredParam("abandoned.bin", abandoned), abandoned)
	ageBlob(t, "models", orgRepo, sha, 200*time.Hour)
	if tombstoneExists("models", orgRepo, sha) {
		t.Fatalf("test setup is wrong: this content must have no tombstone")
	}

	removed, err := u.CleanupUnreferencedBlobs(168 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 1 || blobExists("models", orgRepo, sha) {
		t.Fatalf("legacy unreferenced content must still be reclaimed by mtime (removed=%d)", removed)
	}
}

// 远端缓存只回收有墓碑的：无差别回收等于改动磁盘清理对公开模型的既有行为。
func TestCleanupRecycledBlobsOnlyTouchesTombstonedRemoteContent(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "Qwen/Qwen2.5-0.5B"

	deleted := []byte("deleted by the operator")
	untouched := []byte("still referenced")
	deletedEtag := seedRemoteFile(t, u.fileDao, orgRepo, "c1", "deleted.bin", deleted)
	untouchedEtag := seedRemoteFile(t, u.fileDao, orgRepo, "c1", "kept.bin", untouched)

	// 另一份：无引用但也无墓碑（LRU 把链接删了），不能被回收。
	strayEtag := seedRemoteFile(t, u.fileDao, orgRepo, "c1", "stray.bin", []byte("lru victim"))
	_ = os.Remove(ResolvePath("models", orgRepo, "c1", "stray.bin"))
	_ = os.RemoveAll(filepath.Join(repoApiRoot("models", orgRepo), "paths-info", "c1", "stray.bin"))
	ageBlob(t, "models", orgRepo, strayEtag, 500*time.Hour)

	if _, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "deleted.bin", Sha: deletedEtag,
	}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}

	// 保留期内：不动。
	removed, err := u.CleanupRecycledBlobs(168 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 0 || !blobExists("models", orgRepo, deletedEtag) {
		t.Fatalf("recycled remote content must survive its retention window (removed=%d)", removed)
	}

	ageTombstone(t, "models", orgRepo, deletedEtag, 200*time.Hour)
	removed, err = u.CleanupRecycledBlobs(168 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected exactly 1 reclaimed remote file, got %d", removed)
	}
	if blobExists("models", orgRepo, deletedEtag) {
		t.Fatalf("expired recycled remote content must be removed")
	}
	if !blobExists("models", orgRepo, untouchedEtag) {
		t.Fatalf("referenced remote content must never be reclaimed")
	}
	if !blobExists("models", orgRepo, strayEtag) {
		t.Fatalf("unreferenced remote content without a tombstone must not be reclaimed")
	}
}

// 本地命名空间由 CleanupUnreferencedBlobs 那一趟处理，墓碑扫描不重复处理它，
// 否则同一次回收会被计两遍。
func TestCleanupRecycledBlobsSkipsLocalNamespace(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	payload := []byte("local content")
	sha := sha256Hex(payload)
	mustStage(t, u, deferredParam("a.bin", payload), payload)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))
	if _, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "a.bin", Sha: sha,
	}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}
	ageTombstone(t, "models", orgRepo, sha, 200*time.Hour)

	removed, err := u.CleanupRecycledBlobs(168 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 0 || !blobExists("models", orgRepo, sha) {
		t.Fatalf("the tombstone sweep must leave the local namespace to the manifest sweep (removed=%d)", removed)
	}
}

// ===========================================================================
// F 输入校验
// ===========================================================================

func TestDeleteRejectsPathEscape(t *testing.T) {
	admin, _, repos := newTestCacheAdminDao(t)

	outside := filepath.Join(filepath.Dir(repos), "outside.txt")
	if err := os.WriteFile(outside, []byte("do not touch"), 0o644); err != nil {
		t.Fatalf("write fixture failed: %v", err)
	}

	for _, orgRepo := range []string{"../../etc", "/absolute/repo", "a/../../b"} {
		results, err := admin.SoftDelete([]DeleteItem{{
			RepoType: "models", OrgRepo: orgRepo, Path: "x", Sha: "deadbeef",
		}})
		if err != nil {
			t.Fatalf("soft delete returned a transport error for %q: %v", orgRepo, err)
		}
		if results[0].Status != "failed" {
			t.Fatalf("orgRepo %q must be rejected, got %+v", orgRepo, results[0])
		}
		purged, err := admin.PurgeOrphans([]DeleteItem{{RepoType: "models", OrgRepo: orgRepo, Sha: "deadbeef"}})
		if err != nil {
			t.Fatalf("purge returned a transport error for %q: %v", orgRepo, err)
		}
		if purged[0].Status != "failed" {
			t.Fatalf("orgRepo %q must be rejected by purge, got %+v", orgRepo, purged[0])
		}
	}
	if !util.FileExists(outside) {
		t.Fatalf("a file outside the repository root was removed")
	}
}

func TestDeleteRejectsUnknownRepoType(t *testing.T) {
	admin, _, _ := newTestCacheAdminDao(t)
	results, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "../files", OrgRepo: "dingo-local/demo", Path: "a.bin", Sha: "x",
	}})
	if err != nil {
		t.Fatalf("soft delete returned a transport error: %v", err)
	}
	if results[0].Status != "failed" {
		t.Fatalf("unknown repoType must be rejected, got %+v", results[0])
	}
}

// 回收站里的条目要能算出剩余保留时间，页面上的倒计时靠它。
func TestOrphanRowCarriesExpiry(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	payload := []byte("check expiry")
	sha := sha256Hex(payload)
	mustStage(t, u, deferredParam("a.bin", payload), payload)
	mustPublish(t, u, publishParam("main", manifestItem("a.bin", payload)))
	if _, err := admin.SoftDelete([]DeleteItem{{
		RepoType: "models", OrgRepo: orgRepo, Path: "a.bin", Sha: sha,
	}}); err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}

	row := findOrphan(admin.ListOrphans("models", orgRepo), sha)
	if row == nil {
		t.Fatalf("orphan not listed")
	}
	retention := int64(config.SysConfig.GetUploadOrphanRetention().Seconds())
	if row.ExpiresAt != row.UnlinkedAt+retention {
		t.Fatalf("expiresAt = %d, want unlinkedAt(%d) + retention(%d)", row.ExpiresAt, row.UnlinkedAt, retention)
	}
	if row.Inferred {
		t.Fatalf("an entry created by an explicit delete must not be marked as inferred")
	}
	if len(row.Paths) == 0 || row.Paths[0] != "a.bin" {
		t.Fatalf("the recycle entry must remember where the content came from, got %v", row.Paths)
	}
	// 版本标签要在动清单之前抓下来，否则快照被取代之后就查不到了。
	if len(row.Revisions) == 0 || row.Revisions[0] != "main" {
		t.Fatalf("the recycle entry must remember which revision referenced the content, got %v", row.Revisions)
	}
	if row.Size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", row.Size, len(payload))
	}
}

// ---------------------------------------------------------------------------
// meta 与普通 revision 使用完全相同的列表和删除语义。
// ---------------------------------------------------------------------------

func stageMetaRevision(t *testing.T, u *UploadDao, metadata, readme []byte) *LocalPublishResult {
	t.Helper()
	mustStage(t, u, deferredParam("metadata.json", metadata), metadata)
	mustStage(t, u, deferredParam("README.md", readme), readme)
	param := publishParam("meta",
		manifestItem("metadata.json", metadata),
		manifestItem("README.md", readme))
	param.Overwrite = true
	return mustPublish(t, u, param)
}

func TestListFilesShowsMetaRevision(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	weights := []byte("weights payload")
	mustStage(t, u, deferredParam("weights/model.bin", weights), weights)
	mustPublish(t, u, publishParam("main", manifestItem("weights/model.bin", weights)))
	metadata := []byte("{\"repo\":\"demo\"}")
	readme := []byte("# demo")
	stageMetaRevision(t, u, metadata, readme)

	rows := admin.ListFiles("models", orgRepo)
	if len(rows) != 3 || findRow(rows, "metadata.json", sha256Hex(metadata)) == nil ||
		findRow(rows, "README.md", sha256Hex(readme)) == nil ||
		findRow(rows, "weights/model.bin", sha256Hex(weights)) == nil {
		t.Fatalf("meta files must appear like ordinary revision files, got %+v", rows)
	}

	repos := admin.ListRepos()
	if len(repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(repos))
	}
	if repos[0].FileCount != 3 || repos[0].TotalSize != int64(len(weights)+len(metadata)+len(readme)) {
		t.Fatalf("repo totals must include meta content, got count=%d size=%d",
			repos[0].FileCount, repos[0].TotalSize)
	}
}

func TestListFilesShowsSupersededMetaSnapshotLikeAnyOtherSnapshot(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	first := stageMetaRevision(t, u, []byte("{\"v\":1}"), []byte("# v1"))
	second := stageMetaRevision(t, u, []byte("{\"v\":2}"), []byte("# v2"))
	if first.Commit == second.Commit {
		t.Fatalf("test setup is wrong: expected two distinct snapshots")
	}

	// 普通快照在保留期结束前仍参与缓存索引。
	if rows := admin.ListFiles("models", orgRepo); len(rows) != 4 {
		t.Fatalf("both ordinary snapshots must remain visible until normal cleanup, got %+v", rows)
	}
}

func TestListFilesMergesSharedContentAcrossMainAndMeta(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	readme := []byte("# demo")
	mustStage(t, u, deferredParam("README.md", readme), readme)
	mustPublish(t, u, publishParam("main", manifestItem("README.md", readme)))
	stageMetaRevision(t, u, []byte("{\"repo\":\"demo\"}"), readme)

	row := findRow(admin.ListFiles("models", orgRepo), "README.md", sha256Hex(readme))
	if row == nil {
		t.Fatal("shared README is missing")
	}
	if len(row.Revisions) != 2 || row.Revisions[0] != "main" || row.Revisions[1] != "meta" {
		t.Fatalf("revisions = %v, want [main meta]", row.Revisions)
	}
}

func TestSoftDeleteMetaFileUsesOrdinarySnapshotRewrite(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	metadata := []byte("{\"repo\":\"demo\"}")
	published := stageMetaRevision(t, u, metadata, []byte("# demo"))

	sha := sha256Hex(metadata)
	results, err := admin.SoftDelete([]DeleteItem{{RepoType: "models", OrgRepo: orgRepo, Path: "metadata.json", Sha: sha}})
	if err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}
	if len(results) != 1 || results[0].Status != "deleted" {
		t.Fatalf("deleting a meta file must use ordinary delete semantics, got %+v", results[0])
	}

	currentCommit := revisionCommit(t, u, "models", orgRepo, "meta")
	if currentCommit == published.Commit {
		t.Fatal("meta revision was not rewritten")
	}
	manifest := mustReadManifest(t, "models", orgRepo, currentCommit)
	if _, found := findManifestFile(manifest, "metadata.json"); found {
		t.Fatal("metadata.json survived ordinary delete")
	}
	if !tombstoneExists("models", orgRepo, sha) {
		t.Fatal("unreferenced metadata content did not enter the recycle bin")
	}
}

func TestSoftDeleteSharedMainAndMetaFileRemovesBothReferences(t *testing.T) {
	admin, u, _ := newTestCacheAdminDao(t)
	const orgRepo = "dingo-local/demo"

	readme := []byte("# demo")
	mustStage(t, u, deferredParam("README.md", readme), readme)
	mustPublish(t, u, publishParam("main", manifestItem("README.md", readme)))
	stageMetaRevision(t, u, []byte("{\"repo\":\"demo\"}"), readme)

	sha := sha256Hex(readme)
	results, err := admin.SoftDelete([]DeleteItem{{RepoType: "models", OrgRepo: orgRepo, Path: "README.md", Sha: sha}})
	if err != nil {
		t.Fatalf("soft delete failed: %v", err)
	}
	if len(results) != 1 || results[0].Status != "deleted" {
		t.Fatalf("shared ordinary file delete failed: %+v", results[0])
	}

	for _, revision := range []string{"main", "meta"} {
		current := mustReadManifest(t, "models", orgRepo, revisionCommit(t, u, "models", orgRepo, revision))
		if _, found := findManifestFile(current, "README.md"); found {
			t.Fatalf("README.md survived delete in %s", revision)
		}
	}
	if !tombstoneExists("models", orgRepo, sha) {
		t.Fatal("unreferenced shared content did not enter the recycle bin")
	}
	if findOrphan(admin.ListOrphans("models", orgRepo), sha) == nil {
		t.Fatal("shared content is missing from the recycle bin")
	}
	if findRow(admin.ListFiles("models", orgRepo), "README.md", sha) != nil {
		t.Fatal("deleted shared README still appears in cache listing")
	}
}
