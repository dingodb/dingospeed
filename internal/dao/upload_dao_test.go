package dao

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"dingospeed/internal/data"
	"dingospeed/pkg/config"
	"dingospeed/pkg/util"
)

const testBlockSize = 16

func newTestUploadDao(t *testing.T) (*UploadDao, string) {
	t.Helper()
	repos := t.TempDir()
	oldConfig := config.SysConfig
	config.SysConfig = &config.Config{
		Server:   config.ServerConfig{Repos: repos},
		Download: config.Download{BlockSize: testBlockSize},
		Upload:   config.Upload{Namespace: "dingo-local"},
	}
	t.Cleanup(func() { config.SysConfig = oldConfig })

	baseData := data.NewBaseData()
	lockDao := NewLockDao(baseData)
	fileDao := NewFileDao(nil, baseData, lockDao)
	return NewUploadDao(fileDao, lockDao), repos
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func uploadParam(filePath string, content []byte) LocalUploadParam {
	return LocalUploadParam{
		RepoType: "models",
		Org:      "dingo-local",
		Repo:     "demo",
		Revision: "main",
		FilePath: filePath,
		Size:     int64(len(content)),
		Sha256:   sha256Hex(content),
	}
}

func upload(t *testing.T, u *UploadDao, param LocalUploadParam, content []byte) (*LocalUploadResult, error) {
	t.Helper()
	return u.UploadWholeFile(param, bytes.NewReader(content))
}

func uploadFrom(t *testing.T, u *UploadDao, param LocalUploadParam, start int64, content []byte) (*LocalUploadResult, error) {
	t.Helper()
	param.Start = &start
	return u.UploadWholeFile(param, bytes.NewReader(content))
}

func mustUpload(t *testing.T, u *UploadDao, param LocalUploadParam, content []byte) *LocalUploadResult {
	t.Helper()
	result, err := upload(t, u, param, content)
	if err != nil {
		t.Fatalf("upload %s failed: %v", param.FilePath, err)
	}
	return result
}

func errorCode(err error) string {
	if e, ok := err.(interface{ ErrorCode() string }); ok {
		return e.ErrorCode()
	}
	return ""
}

// readBlobPayload 读回 blob 中的有效负载，用于验证落盘内容逐字节正确。
func readBlobPayload(t *testing.T, repos, repoType, orgRepo, sha string, size int64) []byte {
	t.Helper()
	blobPath := filepath.Join(repos, "files", repoType, orgRepo, "blobs", sha)
	f, err := os.Open(blobPath)
	if err != nil {
		t.Fatalf("open blob failed: %v", err)
	}
	defer f.Close()
	if _, err = f.Seek(int64(36+1024*1024/8), io.SeekStart); err != nil {
		t.Fatalf("seek blob failed: %v", err)
	}
	payload := make([]byte, size)
	if _, err = io.ReadFull(f, payload); err != nil {
		t.Fatalf("read blob payload failed: %v", err)
	}
	return payload
}

func manifestPaths(t *testing.T, u *UploadDao, repoType, orgRepo, commit string) []string {
	t.Helper()
	manifest := u.readManifest(repoType, orgRepo, commit)
	paths := make([]string, 0, len(manifest))
	for _, item := range manifest {
		paths = append(paths, item.Path)
	}
	sort.Strings(paths)
	return paths
}

func assertNoStagedResidue(t *testing.T, repos string) {
	t.Helper()
	err := filepath.Walk(repos, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, localUploadStageSuffix) {
			t.Errorf("staged upload residue left behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repos failed: %v", err)
	}
}

func countStagedResidue(t *testing.T, repos string) int {
	t.Helper()
	count := 0
	err := filepath.Walk(repos, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, localUploadStageSuffix) {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repos failed: %v", err)
	}
	return count
}

func stagedBlobPath(repos, repoType, orgRepo, sha string) string {
	return filepath.Join(repos, "files", repoType, orgRepo, "blobs", sha+localUploadStageSuffix)
}

// §9.8 覆盖与幂等
func TestUploadOverwriteAndIdempotency(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	first := []byte("hello dingo local upload")

	created := mustUpload(t, u, uploadParam("config.json", first), first)
	if created.Status != "effective" || created.BlobReused {
		t.Fatalf("unexpected first upload result: %+v", created)
	}
	if got := readBlobPayload(t, repos, "models", orgRepo, created.Sha256, created.Size); !bytes.Equal(got, first) {
		t.Fatalf("blob payload mismatch: %q", got)
	}

	// 同一位置传完全相同的内容：走幂等快路径，不重复写入，快照标识不变。
	again := mustUpload(t, u, uploadParam("config.json", first), first)
	if again.Status != "already_exists" || !again.BlobReused {
		t.Fatalf("expected idempotent fast path, got %+v", again)
	}
	if again.Commit != created.Commit {
		t.Fatalf("commit must not change when content is unchanged: %s -> %s", created.Commit, again.Commit)
	}

	// 同一位置传不同内容且未声明覆盖：拒绝，已生效内容不受影响。
	changed := []byte("hello dingo local upload v2")
	conflictParam := uploadParam("config.json", changed)
	if _, err := upload(t, u, conflictParam, changed); err == nil {
		t.Fatalf("expected overwrite to be rejected")
	} else if errorCode(err) != "UPLOAD_OVERWRITE_REQUIRED" {
		t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
	}
	if got := readBlobPayload(t, repos, "models", orgRepo, created.Sha256, created.Size); !bytes.Equal(got, first) {
		t.Fatalf("rejected overwrite must not touch effective content, got %q", got)
	}

	// 显式声明覆盖：成功，快照标识更新。
	conflictParam.Overwrite = true
	overwritten := mustUpload(t, u, conflictParam, changed)
	if overwritten.Commit == created.Commit {
		t.Fatalf("commit must change after overwrite")
	}
	if got := readBlobPayload(t, repos, "models", orgRepo, overwritten.Sha256, overwritten.Size); !bytes.Equal(got, changed) {
		t.Fatalf("overwritten payload mismatch: %q", got)
	}

	// 追加新文件：不算覆盖，清单包含新旧全部文件，快照标识再次更新。
	nested := []byte("nested payload")
	appended := mustUpload(t, u, uploadParam("subdir/tokenizer.json", nested), nested)
	if appended.Commit == overwritten.Commit {
		t.Fatalf("commit must change after appending a new file")
	}
	got := manifestPaths(t, u, "models", orgRepo, appended.Commit)
	want := []string{"config.json", "subdir/tokenizer.json"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("manifest mismatch: got %v want %v", got, want)
	}
	assertNoStagedResidue(t, repos)
}

func TestResumeAgainstEffectiveFileUsesFriendlyIdempotency(t *testing.T) {
	u, _ := newTestUploadDao(t)
	first := []byte("hello dingo local upload")
	created := mustUpload(t, u, uploadParam("config.json", first), first)

	// 同一路径、同 sha + size 已经生效时，即使调用方带着错误 start 进来，
	// 服务端也应按幂等成功处理，让恢复脚本可以自然收敛。
	again, err := uploadFrom(t, u, uploadParam("config.json", first), 0, first)
	if err != nil {
		t.Fatalf("effective same-content resume should be idempotent: %v", err)
	}
	if again.Status != "already_exists" || !again.BlobReused || again.Commit != created.Commit {
		t.Fatalf("unexpected idempotent resume result: %+v", again)
	}

	// 同一路径已是不同内容时，续传不能承担覆盖语义；覆盖必须重新发起全量上传。
	changed := []byte("hello dingo local upload v2")
	changedParam := uploadParam("config.json", changed)
	changedParam.Overwrite = true
	if _, err = uploadFrom(t, u, changedParam, 0, changed); err == nil {
		t.Fatalf("resume of different content must be rejected")
	} else if errorCode(err) != "UPLOAD_FULL_OVERWRITE_REQUIRED" {
		t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
	}
}

// §9.8 同一仓库不同版本标签各自独立
func TestUploadRevisionsAreIndependent(t *testing.T) {
	u, _ := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	mainContent := []byte("main revision content")
	mainResult := mustUpload(t, u, uploadParam("config.json", mainContent), mainContent)

	v1Content := []byte("v1 revision content")
	v1Param := uploadParam("config.json", v1Content)
	v1Param.Revision = "v1"
	v1Result := mustUpload(t, u, v1Param, v1Content)

	if mainResult.Commit == v1Result.Commit {
		t.Fatalf("different revisions with different content must not share a commit")
	}
	if got := manifestPaths(t, u, "models", orgRepo, mainResult.Commit); strings.Join(got, ",") != "config.json" {
		t.Fatalf("main manifest mismatch: %v", got)
	}
	if got := manifestPaths(t, u, "models", orgRepo, v1Result.Commit); strings.Join(got, ",") != "config.json" {
		t.Fatalf("v1 manifest mismatch: %v", got)
	}
}

// §9.10 内容问题：实际字节数与声明不符、摘要与实际内容不符
func TestUploadRejectsInvalidContent(t *testing.T) {
	cases := []struct {
		name     string
		declared []byte
		body     []byte
	}{
		{"body shorter than declared", []byte("0123456789abcdef0123"), []byte("0123456789")},
		{"body longer than declared", []byte("0123456789"), []byte("0123456789abcdef0123")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, repos := newTestUploadDao(t)
			param := uploadParam("weights.bin", tc.declared)
			if _, err := upload(t, u, param, tc.body); err == nil {
				t.Fatalf("expected upload to be rejected")
			} else if errorCode(err) != "UPLOAD_INVALID_CONTENT" {
				t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
			}
			blobPath := filepath.Join(repos, "files", "models", "dingo-local/demo", "blobs", param.Sha256)
			if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
				t.Fatalf("failed upload must not leave a blob at the final location")
			}
			if got := countStagedResidue(t, repos); got != 1 {
				t.Fatalf("failed upload must keep one resumable staged file, got %d", got)
			}
		})
	}

	t.Run("sha256 mismatch", func(t *testing.T) {
		u, repos := newTestUploadDao(t)
		param := uploadParam("weights.bin", []byte("declared content!!!!"))
		if _, err := upload(t, u, param, []byte("different content....")[:20]); err == nil {
			t.Fatalf("expected sha256 mismatch to be rejected")
		} else if errorCode(err) != "UPLOAD_INVALID_CONTENT" {
			t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
		}
		if got := countStagedResidue(t, repos); got != 1 {
			t.Fatalf("sha mismatch must keep one staged file for retention cleanup, got %d", got)
		}
	})
}

func TestUploadResumeProgressAndMultipleInterruptions(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	content := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef-tail")
	param := uploadParam("weights.bin", content)

	if _, err := upload(t, u, param, content[:20]); err == nil {
		t.Fatalf("expected the interrupted full upload to return an error")
	} else if errorCode(err) != "UPLOAD_INVALID_CONTENT" {
		t.Fatalf("unexpected initial interruption error: %s (%v)", errorCode(err), err)
	}
	progress, err := u.QueryProgress(param)
	if err != nil {
		t.Fatalf("query progress failed: %v", err)
	}
	if progress.ResumeOffset != testBlockSize || progress.Size != int64(len(content)) || progress.Effective {
		t.Fatalf("unexpected progress after first interruption: %+v", progress)
	}

	for _, wantOffset := range []int64{2 * testBlockSize, 3 * testBlockSize} {
		start := progress.ResumeOffset
		end := start + 20
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		if _, err = uploadFrom(t, u, param, start, content[start:end]); err == nil {
			t.Fatalf("expected interruption at start %d to return an error", start)
		} else if errorCode(err) != "UPLOAD_INVALID_CONTENT" {
			t.Fatalf("unexpected interruption error: %s (%v)", errorCode(err), err)
		}
		progress, err = u.QueryProgress(param)
		if err != nil {
			t.Fatalf("query progress failed: %v", err)
		}
		if progress.ResumeOffset != wantOffset {
			t.Fatalf("resume offset mismatch: got %d want %d", progress.ResumeOffset, wantOffset)
		}
	}

	resumed := mustUploadFrom(t, u, param, progress.ResumeOffset, content[progress.ResumeOffset:])
	if resumed.Status != "effective" || resumed.BlobReused {
		t.Fatalf("unexpected resumed result: %+v", resumed)
	}
	if got := readBlobPayload(t, repos, "models", orgRepo, resumed.Sha256, resumed.Size); !bytes.Equal(got, content) {
		t.Fatalf("resumed payload mismatch: got %q want %q", got, content)
	}
	progress, err = u.QueryProgress(param)
	if err != nil {
		t.Fatalf("query progress after effective failed: %v", err)
	}
	if !progress.Effective || !progress.BlobComplete || progress.ResumeOffset != int64(len(content)) {
		t.Fatalf("unexpected effective progress: %+v", progress)
	}
	assertNoStagedResidue(t, repos)
}

func mustUploadFrom(t *testing.T, u *UploadDao, param LocalUploadParam, start int64, content []byte) *LocalUploadResult {
	t.Helper()
	result, err := uploadFrom(t, u, param, start, content)
	if err != nil {
		t.Fatalf("resume upload %s from %d failed: %v", param.FilePath, start, err)
	}
	return result
}

func TestUploadResumeRejectsOffsetMismatch(t *testing.T) {
	u, _ := newTestUploadDao(t)
	content := []byte("0123456789abcdef0123456789abcdef-tail")
	param := uploadParam("weights.bin", content)
	if _, err := upload(t, u, param, content[:20]); err == nil {
		t.Fatalf("expected interrupted upload")
	}

	for _, start := range []int64{0, 2 * testBlockSize} {
		if _, err := uploadFrom(t, u, param, start, content[start:]); err == nil {
			t.Fatalf("expected resume start %d to be rejected", start)
		} else if errorCode(err) != "UPLOAD_RESUME_OFFSET_MISMATCH" || !strings.Contains(err.Error(), "16") {
			t.Fatalf("unexpected resume offset error: code=%s err=%v", errorCode(err), err)
		}
	}
}

func TestUploadResumeRejectsSizeBindingMismatchAndDiscardsStage(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	content := []byte("0123456789abcdef0123456789abcdef-tail")
	param := uploadParam("weights.bin", content)
	if _, err := upload(t, u, param, content[:20]); err == nil {
		t.Fatalf("expected interrupted upload")
	}
	stagePath := stagedBlobPath(repos, "models", orgRepo, param.Sha256)
	if _, err := os.Stat(stagePath); err != nil {
		t.Fatalf("expected staged file before mismatch: %v", err)
	}

	bad := param
	bad.Size += testBlockSize
	if _, err := uploadFrom(t, u, bad, testBlockSize, content[testBlockSize:]); err == nil {
		t.Fatalf("expected size binding mismatch to be rejected")
	} else if errorCode(err) != "UPLOAD_RESUME_BINDING_MISMATCH" {
		t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
	}
	if _, err := os.Stat(stagePath); !os.IsNotExist(err) {
		t.Fatalf("incompatible staged file must be discarded, stat err: %v", err)
	}
}

func TestUploadResumeWithDifferentContentFailsWholeFileHash(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	content := []byte("0123456789abcdef0123456789abcdef-tail")
	param := uploadParam("weights.bin", content)
	if _, err := upload(t, u, param, content[:20]); err == nil {
		t.Fatalf("expected interrupted upload")
	}

	wrong := append([]byte(nil), content...)
	copy(wrong[testBlockSize:], []byte("DIFFERENT-CONTENT-AFTER-RESUME"))
	if _, err := uploadFrom(t, u, param, testBlockSize, wrong[testBlockSize:]); err == nil {
		t.Fatalf("expected whole-file hash mismatch")
	} else if errorCode(err) != "UPLOAD_INVALID_CONTENT" {
		t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
	}
	blobPath := filepath.Join(repos, "files", "models", orgRepo, "blobs", param.Sha256)
	if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
		t.Fatalf("hash-mismatched resume must not create final blob, stat err: %v", err)
	}
}

func TestStagedUploadRetentionAndCleanup(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	effectiveContent := []byte("already effective payload")
	effective := mustUpload(t, u, uploadParam("config.json", effectiveContent), effectiveContent)
	effectiveBlob := filepath.Join(repos, "files", "models", orgRepo, "blobs", effective.Sha256)

	content := []byte("0123456789abcdef0123456789abcdef-tail")
	param := uploadParam("weights.bin", content)
	if _, err := upload(t, u, param, content[:20]); err == nil {
		t.Fatalf("expected interrupted upload")
	}
	stagePath := stagedBlobPath(repos, "models", orgRepo, param.Sha256)

	if removed, err := u.CleanupExpiredStagedUploads(24 * time.Hour); err != nil || removed != 0 {
		t.Fatalf("fresh staged upload must be retained, removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(stagePath); err != nil {
		t.Fatalf("fresh staged upload was removed: %v", err)
	}

	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stagePath, stale, stale); err != nil {
		t.Fatalf("chtimes staged upload failed: %v", err)
	}
	if removed, err := u.CleanupExpiredStagedUploads(24 * time.Hour); err != nil || removed != 1 {
		t.Fatalf("expected one stale staged upload to be removed, removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(stagePath); !os.IsNotExist(err) {
		t.Fatalf("stale staged upload must be removed, stat err: %v", err)
	}
	if _, err := os.Stat(effectiveBlob); err != nil {
		t.Fatalf("cleanup must not remove effective blob: %v", err)
	}
	if got := readBlobPayload(t, repos, "models", orgRepo, effective.Sha256, effective.Size); !bytes.Equal(got, effectiveContent) {
		t.Fatalf("effective blob changed after cleanup: %q", got)
	}
}

func TestRepeatedFullUploadInterruptionsReuseOneStagedFile(t *testing.T) {
	u, repos := newTestUploadDao(t)
	content := []byte("0123456789abcdef0123456789abcdef-tail")
	param := uploadParam("weights.bin", content)
	for i := 0; i < 5; i++ {
		if _, err := upload(t, u, param, content[:20]); err == nil {
			t.Fatalf("expected interrupted upload %d", i)
		}
		if got := countStagedResidue(t, repos); got != 1 {
			t.Fatalf("interrupted full uploads must reuse one staged file, got %d", got)
		}
	}
}

// 一次摘要不符的上传不得破坏同一 blob 上已生效的内容。
func TestFailedUploadDoesNotCorruptEffectiveBlob(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	good := []byte("the one true payload")
	effective := mustUpload(t, u, uploadParam("a.bin", good), good)

	// 另一个路径声明同一个 sha（因此指向同一个 blob），但发送等长的不同内容。
	evil := uploadParam("b.bin", good)
	corrupt := []byte("a completely wrong!!")
	if len(corrupt) != len(good) {
		t.Fatalf("test setup: payload lengths must match")
	}
	if _, err := upload(t, u, evil, corrupt); err != nil {
		// 允许两种结果：走去重快路径直接成功，或摘要校验失败被拒绝。
		if errorCode(err) != "UPLOAD_INVALID_CONTENT" {
			t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
		}
	}

	if got := readBlobPayload(t, repos, "models", orgRepo, effective.Sha256, effective.Size); !bytes.Equal(got, good) {
		t.Fatalf("effective blob was corrupted: got %q want %q", got, good)
	}
	assertNoStagedResidue(t, repos)
}

// §9.9 不同文件并发上传，最终清单必须同时包含全部文件（坑 8）
func TestConcurrentUploadsOfDifferentFilesKeepFullManifest(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	const fileCount = 8

	var wg sync.WaitGroup
	results := make([]*LocalUploadResult, fileCount)
	errs := make([]error, fileCount)
	for i := 0; i < fileCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := []byte(fmt.Sprintf("payload of file number %d", i))
			results[i], errs[i] = upload(t, u, uploadParam(fmt.Sprintf("shard-%d.bin", i), content), content)
		}(i)
	}
	wg.Wait()

	var lastCommit string
	for i := 0; i < fileCount; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent upload %d failed: %v", i, errs[i])
		}
		lastCommit = results[i].Commit
	}
	// 任取一个返回的 commit 都不足以判断最终态，直接从版本标签解析出当前快照。
	currentCommit, _ := u.fileDao.GetCommitHfOffline("models", orgRepo, "main")
	if currentCommit == "" {
		currentCommit = lastCommit
	}
	got := manifestPaths(t, u, "models", orgRepo, currentCommit)
	if len(got) != fileCount {
		t.Fatalf("final manifest lost entries: got %d (%v), want %d", len(got), got, fileCount)
	}
	assertNoStagedResidue(t, repos)
}

// blockingReader 在第一次读取时通知测试，然后阻塞直到被放行。
type blockingReader struct {
	inner   io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return b.inner.Read(p)
}

// §9.9 同一文件并发上传，第二个必须被明确拒绝
func TestConcurrentUploadOfSameFileIsRejected(t *testing.T) {
	u, _ := newTestUploadDao(t)
	content := []byte("a reasonably sized payload for the busy test")
	param := uploadParam("config.json", content)

	reader := &blockingReader{
		inner:   bytes.NewReader(content),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		_, err := u.UploadWholeFile(param, reader)
		done <- err
	}()

	<-reader.started
	if _, err := upload(t, u, param, content); err == nil {
		t.Fatalf("expected the second concurrent upload of the same file to be rejected")
	} else if errorCode(err) != "UPLOAD_FILE_BUSY" {
		t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
	}
	close(reader.release)
	if err := <-done; err != nil {
		t.Fatalf("first upload must not be affected: %v", err)
	}
}

// 数据集类型与含子目录、以点开头的文件名都必须能端到端落盘。
func TestUploadDatasetsAndRealisticFileNames(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	names := []string{".gitattributes", "README.md", "data/train-00001-of-00002.parquet"}
	var commit string
	for _, name := range names {
		content := []byte("content of " + name)
		param := uploadParam(name, content)
		param.RepoType = "datasets"
		result := mustUpload(t, u, param, content)
		commit = result.Commit
		if got := readBlobPayload(t, repos, "datasets", orgRepo, result.Sha256, result.Size); !bytes.Equal(got, content) {
			t.Fatalf("payload mismatch for %s: %q", name, got)
		}
	}

	got := manifestPaths(t, u, "datasets", orgRepo, commit)
	want := []string{".gitattributes", "README.md", "data/train-00001-of-00002.parquet"}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("manifest mismatch: got %v want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(repos, "modelscope")); !os.IsNotExist(err) {
		t.Fatalf("local upload must not write into ModelScope cache root, stat err: %v", err)
	}
}

// resolve 链接由下载链路在首次请求时建立，上传侧不预建。
func TestResolveLinkIsCreatedLazilyByTheDownloadPath(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	content := []byte("lazily linked payload")
	result := mustUpload(t, u, uploadParam("subdir/config.json", content), content)

	resolveDir := filepath.Join(repos, "files", "models", orgRepo, "resolve")
	if _, err := os.Stat(resolveDir); !os.IsNotExist(err) {
		t.Fatalf("upload must not pre-create the resolve tree, stat err: %v", err)
	}

	// 复现下载链路在服务文件之前做的那一步。
	blobPath := filepath.Join(repos, "files", "models", orgRepo, "blobs", result.Sha256)
	resolvePath := filepath.Join(resolveDir, result.Commit, filepath.FromSlash("subdir/config.json"))
	if err := u.fileDao.ConstructBlobsAndFileFile(blobPath, resolvePath); err != nil {
		t.Fatalf("download path failed to construct the resolve entry: %v", err)
	}
	info, err := os.Stat(resolvePath)
	if err != nil {
		t.Fatalf("resolve entry was not created on demand: %v", err)
	}
	blobInfo, err := os.Stat(blobPath)
	if err != nil {
		t.Fatalf("stat blob failed: %v", err)
	}
	if info.Size() != blobInfo.Size() {
		t.Fatalf("resolve entry does not point at the blob: %d vs %d", info.Size(), blobInfo.Size())
	}
}

// 本地仓库的单文件信息从清单派生，不依赖逐 commit 落盘的 paths-info。
func TestLocalPathsInfoIsDerivedFromManifest(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	content := []byte("payload for paths info")
	result := mustUpload(t, u, uploadParam("subdir/config.json", content), content)

	pathsInfoDir := filepath.Join(repos, "api", "models", orgRepo, "paths-info")
	if _, err := os.Stat(pathsInfoDir); !os.IsNotExist(err) {
		t.Fatalf("upload must not write per-commit paths-info files, stat err: %v", err)
	}

	info, err := u.fileDao.GetPathsInfo("", "models", orgRepo, result.Commit, "", "subdir/config.json")
	if err != nil {
		t.Fatalf("GetPathsInfo failed: %v", err)
	}
	if info.Type != "file" || info.Oid != result.Sha256 || info.Size != int64(len(content)) ||
		info.Path != "subdir/config.json" || info.Lfs.Oid != result.Sha256 || info.Lfs.Size != int64(len(content)) {
		t.Fatalf("unexpected paths info: %+v", info)
	}
	if _, err = u.fileDao.GetPathsInfo("", "models", orgRepo, result.Commit, "", "subdir"); err == nil {
		t.Fatalf("expected a directory prefix to have no file info")
	}
	if _, err = u.fileDao.GetPathsInfo("", "models", orgRepo, result.Commit, "", "missing.json"); err == nil {
		t.Fatalf("expected an unknown path to have no file info")
	}
}

// §9.12 崩溃一致性：新快照未发布到 revision 之前，即使 manifest 与 commit 元数据
// 已经写好，调用方通过版本标签仍只能看到旧状态。
func TestUploadCrashConsistencyKeepsOldRevisionUntilPublish(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	oldContent := []byte("old payload")
	old := mustUpload(t, u, uploadParam("config.json", oldContent), oldContent)

	newContent := []byte("new payload")
	param := uploadParam("config.json", newContent)
	param.Overwrite = true
	if _, err := u.materializeBlob(param, orgRepo, bytes.NewReader(newContent)); err != nil {
		t.Fatalf("materialize new blob failed: %v", err)
	}
	_, currentManifest := u.currentManifest(param)
	newCommit, newManifest, err := u.nextCommit(currentManifest, LocalManifestFile{
		Path:   param.FilePath,
		Sha256: param.Sha256,
		Size:   param.Size,
	})
	if err != nil {
		t.Fatalf("next commit failed: %v", err)
	}

	manifestPath := LocalManifestPath(param.RepoType, orgRepo, newCommit)
	if err = util.MakeDirs(manifestPath); err != nil {
		t.Fatalf("create manifest dir failed: %v", err)
	}
	if err = util.WriteDataToFileAtomic(manifestPath, newManifest); err != nil {
		t.Fatalf("write manifest failed: %v", err)
	}
	if err = u.writeMeta(param.RepoType, orgRepo, newCommit, newCommit, newManifest); err != nil {
		t.Fatalf("write commit metadata failed: %v", err)
	}

	currentCommit, err := u.fileDao.GetCommitHfOffline("models", orgRepo, "main")
	if err != nil {
		t.Fatalf("read current revision failed: %v", err)
	}
	if currentCommit != old.Commit {
		t.Fatalf("revision became visible before publish: got %s want %s", currentCommit, old.Commit)
	}
	if got := manifestPaths(t, u, "models", orgRepo, old.Commit); strings.Join(got, ",") != "config.json" {
		t.Fatalf("old manifest changed after simulated crash: %v", got)
	}
	if _, err = os.Stat(filepath.Join(repos, "api", "models", orgRepo, "revision", newCommit, localManifestFileName)); err != nil {
		t.Fatalf("prepared new manifest must be intact: %v", err)
	}

	published := mustUpload(t, u, param, newContent)
	if published.Commit != newCommit {
		t.Fatalf("publish commit mismatch: got %s want %s", published.Commit, newCommit)
	}
	currentCommit, err = u.fileDao.GetCommitHfOffline("models", orgRepo, "main")
	if err != nil {
		t.Fatalf("read published revision failed: %v", err)
	}
	if currentCommit != newCommit {
		t.Fatalf("revision did not publish new commit: got %s want %s", currentCommit, newCommit)
	}
}

// §9.13 逐个上传 1000 个文件时的元数据开销必须随文件数线性增长，而不是 O(N²)。
func TestUploadMetadataFootprintStaysLinear(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	const fileCount = 1000

	startedAt := time.Now()
	var commit string
	for i := 0; i < fileCount; i++ {
		content := []byte(fmt.Sprintf("payload of file number %d", i))
		name := fmt.Sprintf("shard-%03d/data.bin", i)
		commit = mustUpload(t, u, uploadParam(name, content), content).Commit
	}
	t.Logf("uploaded %d files in %s", fileCount, time.Since(startedAt))

	if got := manifestPaths(t, u, "models", orgRepo, commit); len(got) != fileCount {
		t.Fatalf("final manifest has %d entries, want %d", len(got), fileCount)
	}

	var objects int
	err := filepath.Walk(repos, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			objects++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repos failed: %v", err)
	}
	// 每次上传固定写 1 个 blob + 1 份清单 + 2 份 commit 元数据，
	// 外加全程复用的 2 份版本标签元数据。逐条落 paths-info / 建 resolve 链接的
	// 老写法在这个规模下会产生一万多个文件。
	if want := 4*fileCount + 2; objects != want {
		t.Fatalf("metadata footprint is %d files, want %d (quadratic growth regression?)", objects, want)
	}
}
