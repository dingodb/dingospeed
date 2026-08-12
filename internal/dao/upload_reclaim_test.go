package dao

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/util"
)

// ageBlob 把 blob 的修改时间往前拨，用来模拟“放在那儿很久没人管了”。
func ageBlob(t *testing.T, repoType, orgRepo, sha string, age time.Duration) {
	t.Helper()
	blobPath := localBlobPath(repoType, orgRepo, sha)
	old := time.Now().Add(-age)
	if err := os.Chtimes(blobPath, old, old); err != nil {
		t.Fatalf("age blob %s failed: %v", sha, err)
	}
}

func blobExists(repoType, orgRepo, sha string) bool {
	return util.FileExists(localBlobPath(repoType, orgRepo, sha))
}

// §9.9 回收：暂缓生效上传后一直没发布的内容，保留期内保留、超期可回收。
func TestUnreferencedContentIsReclaimedAfterRetention(t *testing.T) {
	u, _ := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	abandoned := []byte("uploaded but never published")
	mustStage(t, u, deferredParam("abandoned.bin", abandoned), abandoned)
	sha := sha256Hex(abandoned)

	// 保留期内：必须留着，它可能正等着一次还没发出来的发布。
	removed, err := u.CleanupUnreferencedBlobs(48 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 0 || !blobExists("models", orgRepo, sha) {
		t.Fatalf("content within the retention window must be kept (removed=%d)", removed)
	}

	// 保留期内仍然可以正常发布。
	published := mustPublish(t, u, publishParam("main", manifestItem("abandoned.bin", abandoned)))
	if published.Commit == "" {
		t.Fatalf("content within the retention window must still be publishable")
	}

	// 已被引用之后，无论多久都不能删。
	ageBlob(t, "models", orgRepo, sha, 90*24*time.Hour)
	removed, err = u.CleanupUnreferencedBlobs(48 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 0 || !blobExists("models", orgRepo, sha) {
		t.Fatalf("published content must never be reclaimed (removed=%d)", removed)
	}
}

func TestExpiredUnreferencedContentIsReclaimed(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	abandoned := []byte("abandoned batch member")
	mustStage(t, u, deferredParam("abandoned.bin", abandoned), abandoned)
	sha := sha256Hex(abandoned)
	ageBlob(t, "models", orgRepo, sha, 200*time.Hour)

	removed, err := u.CleanupUnreferencedBlobs(168 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 reclaimed file, got %d", removed)
	}
	if blobExists("models", orgRepo, sha) {
		t.Fatalf("expired unreferenced content should be gone")
	}
	// 回收之后重新上传必须能正常走完，不留下坏状态。
	mustStage(t, u, deferredParam("abandoned.bin", abandoned), abandoned)
	mustPublish(t, u, publishParam("main", manifestItem("abandoned.bin", abandoned)))
	if got := readBlobPayload(t, repos, "models", orgRepo, sha, int64(len(abandoned))); !bytes.Equal(got, abandoned) {
		t.Fatalf("re-uploaded content mismatch")
	}
}

// §9.9 的重点：误删一个被引用的内容就是数据丢失。
// 引用可能来自另一个版本标签、来自一个不再被任何标签指向的旧快照，
// 也可能是同一份内容被多处共享。三种情况都必须挡住。
func TestReclaimNeverDeletesReferencedContent(t *testing.T) {
	u, _ := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	// v1 与 v2 共享同一份内容（同 sha），另有一个只属于 v2 的文件。
	shared := []byte("shared across revisions")
	onlyV2 := []byte("only in v2")
	for _, revision := range []string{"v1", "v2"} {
		param := deferredParam("config.json", shared)
		param.Revision = revision
		mustStage(t, u, param, shared)
	}
	extra := deferredParam("extra.bin", onlyV2)
	extra.Revision = "v2"
	mustStage(t, u, extra, onlyV2)

	mustPublish(t, u, publishParam("v1", manifestItem("config.json", shared)))
	mustPublish(t, u, publishParam("v2",
		manifestItem("config.json", shared),
		manifestItem("extra.bin", onlyV2),
	))

	// 覆盖 v2 里的 config.json：老内容从 v2 的最新快照里消失了，
	// 但 v1 仍然引用它，而且 v2 的旧快照也仍然引用它。
	updated := []byte("updated in v2 only")
	updatedParam := deferredParam("config.json", updated)
	updatedParam.Revision = "v2"
	mustStage(t, u, updatedParam, updated)
	overwrite := publishParam("v2", manifestItem("config.json", updated))
	overwrite.Overwrite = true
	mustPublish(t, u, overwrite)

	// 一个真正没人要的文件，作为对照：它必须被回收。
	orphan := []byte("nobody references this")
	mustStage(t, u, deferredParam("orphan.bin", orphan), orphan)

	for _, content := range [][]byte{shared, onlyV2, updated, orphan} {
		ageBlob(t, "models", orgRepo, sha256Hex(content), 200*time.Hour)
	}

	removed, err := u.CleanupUnreferencedBlobs(168 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected exactly the orphan to be reclaimed, got %d", removed)
	}
	if blobExists("models", orgRepo, sha256Hex(orphan)) {
		t.Fatalf("the orphan should have been reclaimed")
	}
	if !blobExists("models", orgRepo, sha256Hex(shared)) {
		t.Fatalf("content referenced by another revision tag must never be reclaimed")
	}
	if !blobExists("models", orgRepo, sha256Hex(onlyV2)) {
		t.Fatalf("content referenced by an older snapshot must never be reclaimed")
	}
	if !blobExists("models", orgRepo, sha256Hex(updated)) {
		t.Fatalf("content referenced by the current snapshot must never be reclaimed")
	}

	// v1 与 v2 的两个版本在回收之后都还能正常读到自己的内容。
	v1Commit := revisionCommit(t, u, "models", orgRepo, "v1")
	if item, ok := findManifestFile(u.readManifest("models", orgRepo, v1Commit), "config.json"); !ok || item.Sha256 != sha256Hex(shared) {
		t.Fatalf("v1 manifest broken after reclaim: %+v", item)
	}
}

// 回收只作用于本地命名空间：公开模型的缓存与自研内容共用同一棵目录树，
// 误伤等于改动了磁盘清理对公开模型的既有行为。
func TestReclaimIgnoresPublicCacheBlobs(t *testing.T) {
	u, repos := newTestUploadDao(t)

	publicBlob := filepath.Join(repos, "files", "models", "openai-community", "gpt2", "blobs", "deadbeef")
	if err := util.MakeDirs(publicBlob); err != nil {
		t.Fatalf("create public blob dir failed: %v", err)
	}
	if err := os.WriteFile(publicBlob, []byte("cached from upstream"), 0o644); err != nil {
		t.Fatalf("write public blob failed: %v", err)
	}
	old := time.Now().Add(-200 * time.Hour)
	if err := os.Chtimes(publicBlob, old, old); err != nil {
		t.Fatalf("age public blob failed: %v", err)
	}

	removed, err := u.CleanupUnreferencedBlobs(168 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 0 {
		t.Fatalf("public cache must not be touched, removed %d", removed)
	}
	if !util.FileExists(publicBlob) {
		t.Fatalf("public cache blob was deleted")
	}
}

// 未完成的暂存文件归暂存清理管，回收任务不碰它。
func TestReclaimLeavesStagedFilesToStagingCleanup(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	content := bytes.Repeat([]byte("x"), testBlockSize*3)
	param := deferredParam("partial.bin", content)
	if _, err := u.UploadWholeFile(param, bytes.NewReader(content[:testBlockSize])); err == nil {
		t.Fatalf("interrupted upload must fail")
	}
	stagePath := stagedBlobPath(repos, "models", orgRepo, sha256Hex(content))
	if !util.FileExists(stagePath) {
		t.Fatalf("expected a staged file to exist")
	}
	old := time.Now().Add(-200 * time.Hour)
	if err := os.Chtimes(stagePath, old, old); err != nil {
		t.Fatalf("age staged file failed: %v", err)
	}

	removed, err := u.CleanupUnreferencedBlobs(1 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 0 || !util.FileExists(stagePath) {
		t.Fatalf("staged files must be left to the staging cleanup (removed=%d)", removed)
	}

	staged, err := u.CleanupExpiredStagedUploads(168 * time.Hour)
	if err != nil {
		t.Fatalf("staging cleanup failed: %v", err)
	}
	if staged != 1 || util.FileExists(stagePath) {
		t.Fatalf("staging cleanup should have removed the staged file (removed=%d)", staged)
	}
}

// 反复放弃的批次不会让残留无限累积。
func TestAbandonedBatchesDoNotAccumulate(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	for round := 0; round < 5; round++ {
		for i := 0; i < 4; i++ {
			content := []byte(fmt.Sprintf("round %d file %d", round, i))
			mustStage(t, u, deferredParam(fmt.Sprintf("shard-%d.bin", i), content), content)
			ageBlob(t, "models", orgRepo, sha256Hex(content), 200*time.Hour)
		}
	}
	blobDir := filepath.Join(repos, "files", "models", filepath.FromSlash(orgRepo), "blobs")
	if got := countFiles(t, blobDir); got != 20 {
		t.Fatalf("expected 20 abandoned blobs, got %d", got)
	}

	removed, err := u.CleanupUnreferencedBlobs(168 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if removed != 20 {
		t.Fatalf("expected all 20 abandoned blobs to be reclaimed, got %d", removed)
	}
	if got := countFiles(t, blobDir); got != 0 {
		t.Fatalf("expected no residue, got %d files", got)
	}
}

// 回收与发布并发时，绝不能让清单指向一个刚被删掉的 blob。
func TestReclaimDoesNotRaceWithPublish(t *testing.T) {
	u, _ := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 30; i++ {
			if _, err := u.CleanupUnreferencedBlobs(time.Nanosecond); err != nil {
				t.Errorf("cleanup failed: %v", err)
				return
			}
		}
	}()

	for i := 0; i < 30; i++ {
		content := []byte(fmt.Sprintf("racing payload %d", i))
		name := fmt.Sprintf("file-%02d.bin", i)
		mustStage(t, u, deferredParam(name, content), content)
		if _, err := u.PublishFiles(publishParam("main", manifestItem(name, content))); err != nil {
			// 内容在发布前就被回收掉是允许的结果，重传一次即可；
			// 不允许的是发布成功却指向一个不存在的 blob。
			if errorCode(err) != "PUBLISH_CONTENT_NOT_READY" {
				t.Fatalf("unexpected publish error: %v", err)
			}
			continue
		}
	}
	<-done

	// 最终清单里的每一条都必须真的在盘上。
	commit := revisionCommit(t, u, "models", orgRepo, "main")
	if commit == "" {
		t.Fatalf("nothing was published")
	}
	manifest := u.readManifest("models", orgRepo, commit)
	if len(manifest) == 0 {
		t.Fatalf("manifest is empty")
	}
	for _, item := range manifest {
		if !blobIsComplete(localBlobPath("models", orgRepo, item.Sha256), item.Size) {
			t.Fatalf("manifest references content that is not on disk: %s", item.Path)
		}
	}
}

func TestOrphanRetentionConfigDefaults(t *testing.T) {
	cfg := &config.Config{}
	if got := cfg.GetUploadOrphanRetention(); got != 24*7*time.Hour {
		t.Fatalf("default orphan retention is %s, want 168h", got)
	}
	cfg.Upload.OrphanRetentionHours = 12
	if got := cfg.GetUploadOrphanRetention(); got != 12*time.Hour {
		t.Fatalf("configured orphan retention is %s, want 12h", got)
	}
}
