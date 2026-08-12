package dao

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"dingospeed/pkg/config"
	"dingospeed/pkg/util"
)

func deferredParam(filePath string, content []byte) LocalUploadParam {
	param := uploadParam(filePath, content)
	param.Deferred = true
	return param
}

func mustStage(t *testing.T, u *UploadDao, param LocalUploadParam, content []byte) *LocalUploadResult {
	t.Helper()
	result, err := u.UploadWholeFile(param, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("staged upload %s failed: %v", param.FilePath, err)
	}
	if result.Status != "staged" {
		t.Fatalf("staged upload %s returned status %q, want staged", param.FilePath, result.Status)
	}
	if result.Commit != "" {
		t.Fatalf("staged upload %s must not produce a commit, got %s", param.FilePath, result.Commit)
	}
	return result
}

func publishParam(revision string, files ...LocalManifestFile) LocalPublishParam {
	return LocalPublishParam{
		RepoType: "models",
		Org:      "dingo-local",
		Repo:     "demo",
		Revision: revision,
		Files:    files,
	}
}

func manifestItem(filePath string, content []byte) LocalManifestFile {
	return LocalManifestFile{Path: filePath, Sha256: sha256Hex(content), Size: int64(len(content))}
}

func mustPublish(t *testing.T, u *UploadDao, param LocalPublishParam) *LocalPublishResult {
	t.Helper()
	result, err := u.PublishFiles(param)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	return result
}

// revisionCommit 读回版本标签当前指向的快照标识，尚未发布过时返回空串。
func revisionCommit(t *testing.T, u *UploadDao, repoType, orgRepo, revision string) string {
	t.Helper()
	metaPath := filepath.Join(config.SysConfig.Repos(), "api", repoType, filepath.FromSlash(orgRepo), "revision", revision, "meta_get.json")
	if !util.FileExists(metaPath) {
		return ""
	}
	commit, err := u.fileDao.GetCommitHfOffline(repoType, orgRepo, revision)
	if err != nil {
		t.Fatalf("read revision %s failed: %v", revision, err)
	}
	return commit
}

// commitDirs 列出该仓库下所有已经落盘的快照标识目录，用来数“产生了几个快照”。
// 版本标签的元数据目录与快照标识目录同级，按名字形态区分：标识一定是 64 位十六进制。
func commitDirs(t *testing.T, repos, repoType, orgRepo string) []string {
	t.Helper()
	root := filepath.Join(repos, "api", repoType, filepath.FromSlash(orgRepo), "revision")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read revision dir failed: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && isCommitDirName(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

func isCommitDirName(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s failed: %v", root, err)
	}
	return count
}

// §9.1 等价性（核心验收，针对坑 批-4）：同一组文件走“逐个即时生效”和
// “暂缓生效 + 一次发布”两条路径，快照标识必须逐字符相同。
func TestPublishIsEquivalentToSequentialUploads(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	files := map[string][]byte{
		"config.json":                  []byte(`{"model_type":"demo"}`),
		"README.md":                    []byte("# demo model"),
		"model.safetensors":            bytes.Repeat([]byte("w"), testBlockSize*3+7),
		"subdir/tokenizer.json":        []byte(`{"version":"1.0"}`),
		"subdir/nested/vocab.txt":      []byte("a\nb\nc\n"),
		"subdir/nested/deep/notes.txt": []byte("deeply nested"),
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	// 路径 A：逐个即时生效上传。
	var sequentialCommit string
	for _, name := range names {
		param := uploadParam(name, files[name])
		param.Revision = "va"
		sequentialCommit = mustUpload(t, u, param, files[name]).Commit
	}

	// 路径 B：逐个暂缓生效上传，最后一次发布。
	batch := make([]LocalManifestFile, 0, len(files))
	for _, name := range names {
		param := deferredParam(name, files[name])
		param.Revision = "vb"
		mustStage(t, u, param, files[name])
		batch = append(batch, manifestItem(name, files[name]))
	}
	published := mustPublish(t, u, publishParam("vb", batch...))

	if published.Commit != sequentialCommit {
		t.Fatalf("batch commit %s differs from sequential commit %s", published.Commit, sequentialCommit)
	}
	if !published.Changed || published.Status != "published" {
		t.Fatalf("publish should have changed the revision: %+v", published)
	}
	if published.Added != len(files) || published.Replaced != 0 || published.Unchanged != 0 {
		t.Fatalf("unexpected publish classification: %+v", published)
	}
	if published.FileCount != len(files) || published.Published != len(files) {
		t.Fatalf("unexpected publish counts: %+v", published)
	}

	if got := revisionCommit(t, u, "models", orgRepo, "vb"); got != sequentialCommit {
		t.Fatalf("revision vb points at %s, want %s", got, sequentialCommit)
	}
	if got := revisionCommit(t, u, "models", orgRepo, "va"); got != sequentialCommit {
		t.Fatalf("revision va points at %s, want %s", got, sequentialCommit)
	}

	// 两条路径的清单与内容都必须一致。
	wantPaths := strings.Join(names, ",")
	if got := strings.Join(manifestPaths(t, u, "models", orgRepo, published.Commit), ","); got != wantPaths {
		t.Fatalf("manifest paths %q, want %q", got, wantPaths)
	}
	for _, name := range names {
		content := files[name]
		got := readBlobPayload(t, repos, "models", orgRepo, sha256Hex(content), int64(len(content)))
		if !bytes.Equal(got, content) {
			t.Fatalf("blob payload for %s does not match uploaded content", name)
		}
	}
	assertNoStagedResidue(t, repos)
}

// §9.3 中间态不可见：发布之前，版本标签、快照标识与元数据都不得发生任何变化；
// 整批发布全程只应产生 1 个新的快照标识（逐个上传的对照组会产生 N 个）。
func TestStagedUploadsAreInvisibleUntilPublish(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	base := []byte("base content")
	first := mustUpload(t, u, uploadParam("config.json", base), base)
	commitsAfterBase := commitDirs(t, repos, "models", orgRepo)
	if len(commitsAfterBase) != 1 {
		t.Fatalf("expected 1 commit after the first upload, got %v", commitsAfterBase)
	}

	batch := make([]LocalManifestFile, 0, 5)
	for i := 0; i < 5; i++ {
		content := []byte(fmt.Sprintf("staged payload %d", i))
		name := fmt.Sprintf("shard-%d/data.bin", i)
		mustStage(t, u, deferredParam(name, content), content)
		batch = append(batch, manifestItem(name, content))

		if got := revisionCommit(t, u, "models", orgRepo, "main"); got != first.Commit {
			t.Fatalf("revision moved to %s while staging, want %s", got, first.Commit)
		}
		if got := commitDirs(t, repos, "models", orgRepo); len(got) != 1 {
			t.Fatalf("staging produced new commit metadata: %v", got)
		}
		if paths := manifestPaths(t, u, "models", orgRepo, first.Commit); strings.Join(paths, ",") != "config.json" {
			t.Fatalf("staging changed the effective manifest: %v", paths)
		}
	}

	// 暂存内容对下载侧不可见，但调用方可以用进度查询确认它已就绪。
	progressParam := uploadParam("shard-0/data.bin", []byte("staged payload 0"))
	progress, err := u.QueryProgress(progressParam)
	if err != nil {
		t.Fatalf("query staged progress failed: %v", err)
	}
	if !progress.BlobComplete || progress.Effective {
		t.Fatalf("staged content should be complete but not effective: %+v", progress)
	}

	published := mustPublish(t, u, publishParam("main", batch...))
	if got := revisionCommit(t, u, "models", orgRepo, "main"); got != published.Commit {
		t.Fatalf("revision did not publish: got %s want %s", got, published.Commit)
	}
	// 一次发布只产生一个新的快照标识：加上基线的那个，一共 2 个。
	if got := commitDirs(t, repos, "models", orgRepo); len(got) != 2 {
		t.Fatalf("publishing 5 files produced %d commits, want 1 new one: %v", len(got), got)
	}
	if published.FileCount != 6 {
		t.Fatalf("merged manifest has %d files, want 6", published.FileCount)
	}
}

// §9.2 完整性（针对坑 批-6）：发布是合并语义，二次发布不得抹掉此前已生效的文件。
func TestPublishMergesInsteadOfReplacing(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	firstBatch := make([]LocalManifestFile, 0, 3)
	for i := 0; i < 3; i++ {
		content := []byte(fmt.Sprintf("first batch %d", i))
		name := fmt.Sprintf("a/file-%d.bin", i)
		mustStage(t, u, deferredParam(name, content), content)
		firstBatch = append(firstBatch, manifestItem(name, content))
	}
	first := mustPublish(t, u, publishParam("main", firstBatch...))

	secondBatch := make([]LocalManifestFile, 0, 2)
	for i := 0; i < 2; i++ {
		content := []byte(fmt.Sprintf("second batch %d", i))
		name := fmt.Sprintf("b/nested/file-%d.bin", i)
		mustStage(t, u, deferredParam(name, content), content)
		secondBatch = append(secondBatch, manifestItem(name, content))
	}
	second := mustPublish(t, u, publishParam("main", secondBatch...))

	if second.Commit == first.Commit {
		t.Fatalf("appending files must produce a new commit")
	}
	if second.FileCount != 5 || second.Added != 2 || second.Replaced != 0 {
		t.Fatalf("unexpected merge result: %+v", second)
	}
	want := "a/file-0.bin,a/file-1.bin,a/file-2.bin,b/nested/file-0.bin,b/nested/file-1.bin"
	if got := strings.Join(manifestPaths(t, u, "models", orgRepo, second.Commit), ","); got != want {
		t.Fatalf("merged manifest is %q, want %q", got, want)
	}
	assertNoStagedResidue(t, repos)
}

// §9.1 / 场景 I：同一个版本标签下混用即时生效上传与批量发布，最终清单必须正确。
func TestPublishMergesWithImmediateUploads(t *testing.T) {
	u, _ := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	immediate := []byte("uploaded immediately")
	mustUpload(t, u, uploadParam("config.json", immediate), immediate)

	staged := []byte("staged for publish")
	mustStage(t, u, deferredParam("weights.bin", staged), staged)
	published := mustPublish(t, u, publishParam("main", manifestItem("weights.bin", staged)))

	later := []byte("uploaded after publish")
	after := mustUpload(t, u, uploadParam("extra.txt", later), later)

	want := "config.json,extra.txt,weights.bin"
	if got := strings.Join(manifestPaths(t, u, "models", orgRepo, after.Commit), ","); got != want {
		t.Fatalf("manifest is %q, want %q", got, want)
	}
	if published.FileCount != 2 {
		t.Fatalf("publish merged manifest has %d files, want 2", published.FileCount)
	}
}

// §9.4 发布前置校验（针对坑 批-2）：清单是调用方声明的，服务端必须自己核对内容
// 确实完整落盘。任何一条不满足都整次拒绝，且不留下任何副作用。
func TestPublishRejectsContentThatIsNotReady(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	ready := []byte("this one is ready")
	mustStage(t, u, deferredParam("ready.bin", ready), ready)

	// 一个从未上传过的文件。
	never := []byte("never uploaded")
	_, err := u.PublishFiles(publishParam("main",
		manifestItem("ready.bin", ready),
		manifestItem("missing.bin", never),
	))
	if errorCode(err) != "PUBLISH_CONTENT_NOT_READY" {
		t.Fatalf("publishing a never-uploaded file returned %v (code %s)", err, errorCode(err))
	}
	if !strings.Contains(err.Error(), "missing.bin") {
		t.Fatalf("error must name the offending path, got %v", err)
	}

	// 一个只传了一半的文件。
	half := bytes.Repeat([]byte("h"), testBlockSize*3)
	halfParam := deferredParam("half.bin", half)
	if _, err = u.UploadWholeFile(halfParam, bytes.NewReader(half[:testBlockSize])); err == nil {
		t.Fatalf("short body must be rejected")
	}
	_, err = u.PublishFiles(publishParam("main",
		manifestItem("ready.bin", ready),
		manifestItem("half.bin", half),
	))
	if errorCode(err) != "PUBLISH_CONTENT_NOT_READY" {
		t.Fatalf("publishing a half-uploaded file returned %v (code %s)", err, errorCode(err))
	}

	// 声明的大小与已落盘内容对不上。
	badSize := manifestItem("ready.bin", ready)
	badSize.Size++
	if _, err = u.PublishFiles(publishParam("main", badSize)); errorCode(err) != "PUBLISH_CONTENT_MISMATCH" {
		t.Fatalf("size mismatch returned %v (code %s)", err, errorCode(err))
	}

	// 上述拒绝都不得产生任何元数据。
	if got := commitDirs(t, repos, "models", orgRepo); len(got) != 0 {
		t.Fatalf("rejected publishes left commit metadata behind: %v", got)
	}
	if got := revisionCommit(t, u, "models", orgRepo, "main"); got != "" {
		t.Fatalf("rejected publishes moved the revision to %s", got)
	}
}

// §9.4：拒绝之后，此前已生效的版本必须原样不动。
func TestRejectedPublishLeavesEffectiveRevisionUntouched(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	base := []byte("effective content")
	first := mustUpload(t, u, uploadParam("config.json", base), base)
	before := commitDirs(t, repos, "models", orgRepo)

	missing := []byte("never uploaded")
	if _, err := u.PublishFiles(publishParam("main", manifestItem("ghost.bin", missing))); err == nil {
		t.Fatalf("publish of missing content must fail")
	}

	if got := revisionCommit(t, u, "models", orgRepo, "main"); got != first.Commit {
		t.Fatalf("revision changed after a failed publish: got %s want %s", got, first.Commit)
	}
	if got := commitDirs(t, repos, "models", orgRepo); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("failed publish changed commit metadata: %v -> %v", before, got)
	}
	if paths := manifestPaths(t, u, "models", orgRepo, first.Commit); strings.Join(paths, ",") != "config.json" {
		t.Fatalf("failed publish changed the effective manifest: %v", paths)
	}
}

// §9.5 覆盖与幂等（对应 BR-5）。
func TestPublishOverwriteAndIdempotency(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	original := []byte("original weights")
	mustStage(t, u, deferredParam("weights.bin", original), original)
	keep := []byte("untouched file")
	mustStage(t, u, deferredParam("keep.txt", keep), keep)
	first := mustPublish(t, u, publishParam("main",
		manifestItem("weights.bin", original),
		manifestItem("keep.txt", keep),
	))

	// 重复发布同一批：清单没变，标识必须保持不变且不重写元数据。
	repeat := mustPublish(t, u, publishParam("main",
		manifestItem("weights.bin", original),
		manifestItem("keep.txt", keep),
	))
	if repeat.Commit != first.Commit || repeat.Changed {
		t.Fatalf("republishing identical content must not change the commit: %+v", repeat)
	}
	if repeat.Unchanged != 2 || repeat.Added != 0 || repeat.Replaced != 0 {
		t.Fatalf("unexpected idempotent classification: %+v", repeat)
	}
	if got := commitDirs(t, repos, "models", orgRepo); len(got) != 1 {
		t.Fatalf("idempotent publish created extra commits: %v", got)
	}

	// 内容不同但未声明覆盖：整次拒绝，并指出冲突路径。
	updated := []byte("retrained weights")
	mustStage(t, u, deferredParam("weights.bin", updated), updated)
	added := []byte("brand new file")
	mustStage(t, u, deferredParam("extra.bin", added), added)
	conflictBatch := publishParam("main",
		manifestItem("weights.bin", updated),
		manifestItem("extra.bin", added),
	)
	_, err := u.PublishFiles(conflictBatch)
	if errorCode(err) != "PUBLISH_OVERWRITE_REQUIRED" {
		t.Fatalf("overwrite conflict returned %v (code %s)", err, errorCode(err))
	}
	if !strings.Contains(err.Error(), "weights.bin") || strings.Contains(err.Error(), "extra.bin") {
		t.Fatalf("error must name only the conflicting path, got %v", err)
	}
	if got := revisionCommit(t, u, "models", orgRepo, "main"); got != first.Commit {
		t.Fatalf("rejected publish moved the revision")
	}

	// 声明覆盖后成功，新增/覆盖/无变化三类混在一批里也要各自统计正确。
	conflictBatch.Overwrite = true
	conflictBatch.Files = append(conflictBatch.Files, manifestItem("keep.txt", keep))
	second := mustPublish(t, u, conflictBatch)
	if second.Commit == first.Commit {
		t.Fatalf("overwrite must produce a new commit")
	}
	if second.Added != 1 || second.Replaced != 1 || second.Unchanged != 1 {
		t.Fatalf("unexpected mixed-batch classification: %+v", second)
	}
	if second.FileCount != 3 {
		t.Fatalf("merged manifest has %d files, want 3", second.FileCount)
	}
	manifest := u.readManifest("models", orgRepo, second.Commit)
	item, ok := findManifestFile(manifest, "weights.bin")
	if !ok || item.Sha256 != sha256Hex(updated) {
		t.Fatalf("weights.bin was not replaced: %+v", item)
	}
	if got := readBlobPayload(t, repos, "models", orgRepo, sha256Hex(updated), int64(len(updated))); !bytes.Equal(got, updated) {
		t.Fatalf("updated blob content mismatch")
	}
}

// §9.7 并发：同一版本的第二个发布必须被明确拒绝，而不是排队。
func TestConcurrentPublishOfSameRevisionIsRejected(t *testing.T) {
	u, _ := newTestUploadDao(t)

	content := []byte("payload")
	mustStage(t, u, deferredParam("config.json", content), content)

	// 直接占住发布互斥，等价于“已有一个发布正在进行”。
	key := "upload-publish:models:dingo-local/demo:main"
	if !tryEnterLocalUpload(key) {
		t.Fatalf("publish key should be free")
	}
	_, err := u.PublishFiles(publishParam("main", manifestItem("config.json", content)))
	leaveLocalUpload(key)
	if errorCode(err) != "PUBLISH_IN_PROGRESS" {
		t.Fatalf("second publish returned %v (code %s)", err, errorCode(err))
	}

	// 互斥释放后同一次发布应当正常成功。
	mustPublish(t, u, publishParam("main", manifestItem("config.json", content)))
}

// §9.7（针对坑 批-7）：发布与即时生效上传共用同一把版本锁，并发时最终清单必须同时
// 包含两者——两条路径都要“读清单 → 合并 → 写回”，不共用就会互相覆盖。
func TestConcurrentPublishAndImmediateUploadKeepBothFiles(t *testing.T) {
	u, _ := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	stagedContent := []byte("staged payload")
	mustStage(t, u, deferredParam("staged.bin", stagedContent), stagedContent)
	immediateContent := []byte("immediate payload")

	var wg sync.WaitGroup
	var publishErr, uploadErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, publishErr = u.PublishFiles(publishParam("main", manifestItem("staged.bin", stagedContent)))
	}()
	go func() {
		defer wg.Done()
		_, uploadErr = upload(t, u, uploadParam("immediate.bin", immediateContent), immediateContent)
	}()
	wg.Wait()
	if publishErr != nil {
		t.Fatalf("publish failed: %v", publishErr)
	}
	if uploadErr != nil {
		t.Fatalf("immediate upload failed: %v", uploadErr)
	}

	finalCommit := revisionCommit(t, u, "models", orgRepo, "main")
	want := "immediate.bin,staged.bin"
	if got := strings.Join(manifestPaths(t, u, "models", orgRepo, finalCommit), ","); got != want {
		t.Fatalf("final manifest is %q, want %q", got, want)
	}
}

// §9.7：不同版本标签的发布互不影响。
func TestPublishToDifferentRevisionsIsIndependent(t *testing.T) {
	u, _ := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	shared := []byte("shared payload")
	onlyV2 := []byte("only in v2")
	for _, revision := range []string{"v1", "v2"} {
		param := deferredParam("config.json", shared)
		param.Revision = revision
		mustStage(t, u, param, shared)
	}
	extra := deferredParam("extra.bin", onlyV2)
	extra.Revision = "v2"
	mustStage(t, u, extra, onlyV2)

	v1 := mustPublish(t, u, publishParam("v1", manifestItem("config.json", shared)))
	v2 := mustPublish(t, u, publishParam("v2",
		manifestItem("config.json", shared),
		manifestItem("extra.bin", onlyV2),
	))

	if v1.Commit == v2.Commit {
		t.Fatalf("different content must produce different commits")
	}
	if got := strings.Join(manifestPaths(t, u, "models", orgRepo, v1.Commit), ","); got != "config.json" {
		t.Fatalf("v1 manifest is %q", got)
	}
	if got := strings.Join(manifestPaths(t, u, "models", orgRepo, v2.Commit), ","); got != "config.json,extra.bin" {
		t.Fatalf("v2 manifest is %q", got)
	}
	if got := revisionCommit(t, u, "models", orgRepo, "v1"); got != v1.Commit {
		t.Fatalf("v1 revision moved to %s", got)
	}
}

// §9.8 断点续传在暂缓生效路径下不得退化：中断、查进度、续传、发布，
// 结果必须与一次传完逐字节一致。
func TestDeferredUploadSupportsResume(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	content := bytes.Repeat([]byte("abcdefgh"), testBlockSize)
	param := deferredParam("weights.bin", content)

	// 只传前两个块就中断。
	if _, err := u.UploadWholeFile(param, bytes.NewReader(content[:testBlockSize*2])); err == nil {
		t.Fatalf("interrupted upload must fail")
	}
	progress, err := u.QueryProgress(param)
	if err != nil {
		t.Fatalf("query progress failed: %v", err)
	}
	if progress.ResumeOffset != testBlockSize*2 || progress.Status != "uploading" {
		t.Fatalf("unexpected progress: %+v", progress)
	}
	if progress.BlobComplete {
		t.Fatalf("interrupted content must not be reported complete")
	}

	// 起始位置不等于可续传偏移仍然必须被拒绝。
	bad := progress.ResumeOffset + testBlockSize
	badParam := param
	badParam.Start = &bad
	if _, err = u.UploadWholeFile(badParam, bytes.NewReader(content[bad:])); errorCode(err) != "UPLOAD_RESUME_OFFSET_MISMATCH" {
		t.Fatalf("offset mismatch returned %v (code %s)", err, errorCode(err))
	}

	resumed := mustUploadFrom(t, u, param, progress.ResumeOffset, content[progress.ResumeOffset:])
	if resumed.Status != "staged" || resumed.Commit != "" {
		t.Fatalf("resumed deferred upload must stay staged: %+v", resumed)
	}
	if got := revisionCommit(t, u, "models", orgRepo, "main"); got != "" {
		t.Fatalf("resumed deferred upload became visible at %s", got)
	}

	published := mustPublish(t, u, publishParam("main", manifestItem("weights.bin", content)))
	if got := readBlobPayload(t, repos, "models", orgRepo, sha256Hex(content), int64(len(content))); !bytes.Equal(got, content) {
		t.Fatalf("resumed content is not byte-identical")
	}
	if got := revisionCommit(t, u, "models", orgRepo, "main"); got != published.Commit {
		t.Fatalf("publish did not move the revision")
	}
	assertNoStagedResidue(t, repos)
}

// §9.8：续传时换成另一个内容不同的文件，整体摘要校验必须拦住，
// 暂缓生效路径不得因为“反正还没生效”就放宽这一关。
func TestDeferredResumeWithDifferentContentStillFailsHash(t *testing.T) {
	u, _ := newTestUploadDao(t)

	content := bytes.Repeat([]byte("original"), testBlockSize)
	param := deferredParam("weights.bin", content)
	if _, err := u.UploadWholeFile(param, bytes.NewReader(content[:testBlockSize*2])); err == nil {
		t.Fatalf("interrupted upload must fail")
	}

	tampered := bytes.Repeat([]byte("tampered"), testBlockSize)
	start := int64(testBlockSize * 2)
	resumeParam := param
	resumeParam.Start = &start
	_, err := u.UploadWholeFile(resumeParam, bytes.NewReader(tampered[start:]))
	if errorCode(err) != "UPLOAD_INVALID_CONTENT" {
		t.Fatalf("spliced content returned %v (code %s)", err, errorCode(err))
	}
	if _, err = u.PublishFiles(publishParam("main", manifestItem("weights.bin", content))); errorCode(err) != "PUBLISH_CONTENT_NOT_READY" {
		t.Fatalf("publishing spliced content returned %v (code %s)", err, errorCode(err))
	}
}

// 数据集类型与模型类型的批量发布行为必须一致。
func TestPublishDatasets(t *testing.T) {
	u, _ := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	content := []byte("dataset shard")
	param := deferredParam("data/train-00000.parquet", content)
	param.RepoType = "datasets"
	mustStage(t, u, param, content)

	publish := publishParam("main", manifestItem("data/train-00000.parquet", content))
	publish.RepoType = "datasets"
	result := mustPublish(t, u, publish)

	if got := revisionCommit(t, u, "datasets", orgRepo, "main"); got != result.Commit {
		t.Fatalf("datasets revision points at %s, want %s", got, result.Commit)
	}
	if got := strings.Join(manifestPaths(t, u, "datasets", orgRepo, result.Commit), ","); got != "data/train-00000.parquet" {
		t.Fatalf("datasets manifest is %q", got)
	}
}

// §8 性能目标：一次发布 N 个文件的元数据写入量与 N 无关——只有 1 份清单、
// 2 份 commit 元数据和 2 份版本标签元数据，其余全部是内容本身。
func TestPublishMetadataFootprintIsConstant(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	const fileCount = 200

	batch := make([]LocalManifestFile, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		content := []byte(fmt.Sprintf("payload of file number %d", i))
		name := fmt.Sprintf("shard-%03d/data.bin", i)
		mustStage(t, u, deferredParam(name, content), content)
		batch = append(batch, manifestItem(name, content))
	}

	// 发布之前，磁盘上只有内容，没有任何元数据。
	if got := countFiles(t, repos); got != fileCount {
		t.Fatalf("staging wrote %d files, want %d blobs and no metadata", got, fileCount)
	}

	result := mustPublish(t, u, publishParam("main", batch...))
	if result.FileCount != fileCount {
		t.Fatalf("manifest has %d files, want %d", result.FileCount, fileCount)
	}
	// N 个 blob + 1 份清单 + 2 份 commit 元数据 + 2 份版本标签元数据。
	if want := fileCount + 5; countFiles(t, repos) != want {
		t.Fatalf("metadata footprint is %d files, want %d", countFiles(t, repos), want)
	}
	if got := commitDirs(t, repos, "models", orgRepo); len(got) != 1 {
		t.Fatalf("publishing %d files produced %d commits, want 1", fileCount, len(got))
	}
}
