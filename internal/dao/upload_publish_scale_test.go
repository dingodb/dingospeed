package dao

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"dingospeed/pkg/util"
)

// §9.6 崩溃一致性：发布的生效顺序是「先写新快照的清单与元数据，最后原子发布版本标签」。
// 在最后一步之前崩掉，调用方通过版本标签看到的必须仍是发布前的状态。
func TestPublishCrashConsistencyKeepsOldRevisionUntilPublish(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	base := []byte("already effective")
	old := mustUpload(t, u, uploadParam("config.json", base), base)

	// 一批新文件已经完整落盘，等待发布。
	batch := make([]LocalManifestFile, 0, 3)
	for i := 0; i < 3; i++ {
		content := []byte(fmt.Sprintf("batch payload %d", i))
		name := fmt.Sprintf("shard-%d.bin", i)
		mustStage(t, u, deferredParam(name, content), content)
		batch = append(batch, manifestItem(name, content))
	}

	// 手工执行到“新快照的清单与 commit 元数据都写好了”，然后停在发布版本标签之前。
	_, currentManifest := u.currentManifestOf("models", orgRepo, "main")
	newCommit, newManifest, err := u.nextCommitBatch(currentManifest, batch)
	if err != nil {
		t.Fatalf("next commit failed: %v", err)
	}
	manifestPath := LocalManifestPath("models", orgRepo, newCommit)
	if err = util.MakeDirs(manifestPath); err != nil {
		t.Fatalf("create manifest dir failed: %v", err)
	}
	if err = util.WriteDataToFileAtomic(manifestPath, newManifest); err != nil {
		t.Fatalf("write manifest failed: %v", err)
	}
	if err = u.writeMeta("models", orgRepo, newCommit, newCommit, newManifest); err != nil {
		t.Fatalf("write commit metadata failed: %v", err)
	}

	// 崩溃点：版本标签必须仍然指向旧快照，旧清单必须原样。
	if got := revisionCommit(t, u, "models", orgRepo, "main"); got != old.Commit {
		t.Fatalf("revision became visible before publish: got %s want %s", got, old.Commit)
	}
	if got := manifestPaths(t, u, "models", orgRepo, old.Commit); strings.Join(got, ",") != "config.json" {
		t.Fatalf("old manifest changed after simulated crash: %v", got)
	}
	if _, err = os.Stat(manifestPath); err != nil {
		t.Fatalf("prepared new manifest must be intact: %v", err)
	}

	// 重启后重跑同一次发布：落到同一个快照标识，并正常生效。
	published := mustPublish(t, u, publishParam("main", batch...))
	if published.Commit != newCommit {
		t.Fatalf("publish commit mismatch: got %s want %s", published.Commit, newCommit)
	}
	if got := revisionCommit(t, u, "models", orgRepo, "main"); got != newCommit {
		t.Fatalf("revision did not publish new commit: got %s want %s", got, newCommit)
	}
	if got := manifestPaths(t, u, "models", orgRepo, newCommit); len(got) != 4 {
		t.Fatalf("published manifest has %d files, want 4: %v", len(got), got)
	}
	// 清单里的每一条都必须真的在盘上。
	for _, item := range u.readManifest("models", orgRepo, newCommit) {
		if !blobIsComplete(localBlobPath("models", orgRepo, item.Sha256), item.Size) {
			t.Fatalf("manifest references content that is not on disk: %s", item.Path)
		}
	}
	assertNoStagedResidue(t, repos)
}

// §9.6 场景 G：批量上传传到一半挂掉，重跑脚本（已传的走幂等快路径、没传完的续传）
// 再发布，结果必须与一次跑完完全一致。
func TestInterruptedBatchRerunMatchesUninterruptedRun(t *testing.T) {
	const orgRepo = "dingo-local/demo"

	files := map[string][]byte{
		"config.json":       []byte(`{"model_type":"demo"}`),
		"subdir/vocab.txt":  []byte("a\nb\nc\n"),
		"model.safetensors": bytes.Repeat([]byte("w"), testBlockSize*4),
	}
	names := []string{"config.json", "model.safetensors", "subdir/vocab.txt"}

	// 参照组：同一组文件逐个即时生效上传，跑在**独立的仓库目录**里并先跑完。
	// 内容是按摘要寻址的，参照组若和被测目录共用一份 blob 空间，后面
	// “传到一半中断”的那一步会直接命中幂等快路径，中断根本发生不了。
	var reference string
	func() {
		refDao, _ := newTestUploadDao(t)
		for _, name := range names {
			reference = mustUpload(t, refDao, uploadParam(name, files[name]), files[name]).Commit
		}
	}()

	u, repos := newTestUploadDao(t)

	// 第一轮：前两个文件传完，第三个（大文件）传到一半中断。
	mustStage(t, u, deferredParam("config.json", files["config.json"]), files["config.json"])
	mustStage(t, u, deferredParam("subdir/vocab.txt", files["subdir/vocab.txt"]), files["subdir/vocab.txt"])
	big := files["model.safetensors"]
	bigParam := deferredParam("model.safetensors", big)
	if _, err := u.UploadWholeFile(bigParam, bytes.NewReader(big[:testBlockSize*2])); err == nil {
		t.Fatalf("interrupted upload must fail")
	}
	if got := revisionCommit(t, u, "models", orgRepo, "main"); got != "" {
		t.Fatalf("an interrupted batch must leave nothing visible, got %s", got)
	}

	// 第二轮：重跑脚本。已传完的走幂等快路径，没传完的从可续传位置接着传。
	for _, name := range []string{"config.json", "subdir/vocab.txt"} {
		result := mustStage(t, u, deferredParam(name, files[name]), files[name])
		if !result.BlobReused {
			t.Fatalf("re-uploading %s should have taken the idempotent fast path", name)
		}
	}
	progress, err := u.QueryProgress(bigParam)
	if err != nil {
		t.Fatalf("query progress failed: %v", err)
	}
	if progress.ResumeOffset == 0 || progress.BlobComplete {
		t.Fatalf("unexpected resume state: %+v", progress)
	}
	mustUploadFrom(t, u, bigParam, progress.ResumeOffset, big[progress.ResumeOffset:])

	batch := make([]LocalManifestFile, 0, len(names))
	for _, name := range names {
		batch = append(batch, manifestItem(name, files[name]))
	}
	published := mustPublish(t, u, publishParam("main", batch...))

	if published.Commit != reference {
		t.Fatalf("interrupted-then-resumed batch produced %s, want %s", published.Commit, reference)
	}
	for _, name := range names {
		content := files[name]
		if got := readBlobPayload(t, repos, "models", orgRepo, sha256Hex(content), int64(len(content))); !bytes.Equal(got, content) {
			t.Fatalf("content of %s is not byte-identical after resume", name)
		}
	}
	assertNoStagedResidue(t, repos)
}

// metadataBytes 统计元数据（清单与版本信息）占用的字节数，内容文件不计。
func metadataBytes(t *testing.T, repos string) int64 {
	t.Helper()
	var total int64
	root := filepath.Join(repos, "api")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk api dir failed: %v", err)
	}
	return total
}

// §9.11 规模：批量发布与逐个上传的元数据写入量对比。
// 逐个上传每次都要重写一次全量清单，写入的元数据字节数是 O(N²)；
// 批量发布只写一次，与 N 无关。
//
// 默认跑 200 个文件（足以让两者拉开数量级），交付所需的 1000 文件规模测量用
// DINGO_PUBLISH_SCALE_FILES=1000 触发，结果记录在交付文档里。
//
// 这里只断言**元数据写入量**这类结构性指标，不断言耗时：本机实测同一段上传循环
// 的墙上时间在不同时刻能相差一个数量级（后台扫描等外部因素），把它写进断言只会
// 得到一个随机失败的测试。
func TestPublishScaleComparedToSequentialUploads(t *testing.T) {
	fileCount := 200
	if raw := os.Getenv("DINGO_PUBLISH_SCALE_FILES"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("DINGO_PUBLISH_SCALE_FILES must be a positive integer, got %q", raw)
		}
		fileCount = parsed
	}

	content := func(i int) []byte { return []byte(fmt.Sprintf("payload of file number %d", i)) }
	name := func(i int) string { return fmt.Sprintf("shard-%03d/data.bin", i) }

	// 逐个即时生效上传。两个 dao 不能同时存活（测试夹具会替换全局配置），
	// 所以顺序路径先整个跑完，度量之后再建批量路径的夹具。
	var seqCommit string
	var seqMeta int64
	var seqObjects int
	func() {
		seqDao, seqRepos := newTestUploadDao(t)
		for i := 0; i < fileCount; i++ {
			seqCommit = mustUpload(t, seqDao, uploadParam(name(i), content(i)), content(i)).Commit
		}
		seqMeta = metadataBytes(t, seqRepos)
		seqObjects = countFiles(t, filepath.Join(seqRepos, "api"))
	}()

	// 暂缓生效上传 + 一次发布。
	batchDao, batchRepos := newTestUploadDao(t)
	batch := make([]LocalManifestFile, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		mustStage(t, batchDao, deferredParam(name(i), content(i)), content(i))
		batch = append(batch, manifestItem(name(i), content(i)))
	}
	published := mustPublish(t, batchDao, publishParam("main", batch...))
	batchMeta := metadataBytes(t, batchRepos)
	batchObjects := countFiles(t, filepath.Join(batchRepos, "api"))

	t.Logf("files=%d  sequential: %d metadata objects / %d bytes   batch: %d metadata objects / %d bytes",
		fileCount, seqObjects, seqMeta, batchObjects, batchMeta)
	t.Logf("metadata write reduction: %.1fx fewer bytes, %.1fx fewer objects",
		float64(seqMeta)/float64(batchMeta), float64(seqObjects)/float64(batchObjects))

	if published.Commit != seqCommit {
		t.Fatalf("batch commit %s differs from sequential commit %s at scale", published.Commit, seqCommit)
	}
	if published.FileCount != fileCount {
		t.Fatalf("published manifest has %d files, want %d", published.FileCount, fileCount)
	}
	// 一次发布只写 1 份清单 + 2 份 commit 元数据 + 2 份版本标签元数据，与 N 无关。
	if batchObjects != 5 {
		t.Fatalf("batch publish wrote %d metadata objects, want 5 regardless of file count", batchObjects)
	}
	// 逐个上传每次重写全量清单，元数据字节数必然远大于批量。
	if batchMeta*20 > seqMeta {
		t.Fatalf("expected batch metadata (%d bytes) to be far smaller than sequential (%d bytes)", batchMeta, seqMeta)
	}
}
