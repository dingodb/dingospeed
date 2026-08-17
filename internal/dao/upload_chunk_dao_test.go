package dao

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func chunkParam(filePath string, content []byte) LocalChunkUploadParam {
	return LocalChunkUploadParam{
		RepoType: "models",
		Org:      "dingo-local",
		Repo:     "demo",
		Revision: "main",
		FilePath: filePath,
		Sha256:   sha256Hex(content),
		Size:     int64(len(content)),
	}
}

// putChunk 按调用方切好的偏移与长度发一个分块，chunk 摘要由内容自身算出。
func putChunk(t *testing.T, u *UploadDao, param LocalChunkUploadParam, content []byte, offset, length int64) (*LocalChunkUploadResult, error) {
	t.Helper()
	param.Offset = offset
	param.Length = length
	chunk := content[offset : offset+length]
	param.ChunkSha256 = sha256Hex(chunk)
	return u.UploadChunk(param, bytes.NewReader(chunk))
}

func mustPutChunk(t *testing.T, u *UploadDao, param LocalChunkUploadParam, content []byte, offset, length int64) *LocalChunkUploadResult {
	t.Helper()
	result, err := putChunk(t, u, param, content, offset, length)
	if err != nil {
		t.Fatalf("chunk upload [%d,%d) failed: %v", offset, offset+length, err)
	}
	return result
}

// chunkPlan 把 size 按 chunkLen 切成 (offset, length) 列表，最后一段允许不足整块。
func chunkPlan(size, chunkLen int64) [][2]int64 {
	var plan [][2]int64
	for offset := int64(0); offset < size; offset += chunkLen {
		length := chunkLen
		if offset+length > size {
			length = size - offset
		}
		plan = append(plan, [2]int64{offset, length})
	}
	return plan
}

func progressOf(t *testing.T, u *UploadDao, param LocalChunkUploadParam) *LocalUploadProgress {
	t.Helper()
	progress, err := u.QueryProgress(LocalUploadParam{
		RepoType: param.RepoType,
		Org:      param.Org,
		Repo:     param.Repo,
		Revision: param.Revision,
		FilePath: param.FilePath,
		Sha256:   param.Sha256,
	})
	if err != nil {
		t.Fatalf("query progress failed: %v", err)
	}
	return progress
}

func randomContent(size int, seed int64) []byte {
	buf := make([]byte, size)
	r := rand.New(rand.NewSource(seed))
	r.Read(buf)
	return buf
}

// §7 同一文件多块乱序并发上传后，位图无空洞，publish 成功。
func TestChunkUploadOutOfOrderConcurrentThenPublish(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"

	// 4 个块一个 chunk，末尾故意留一个不足整块的尾巴。
	content := randomContent(testBlockSize*10+5, 1)
	param := chunkParam("weights.bin", content)
	plan := chunkPlan(param.Size, testBlockSize*4)

	// 乱序：把切分计划打乱之后再并发发出去。
	rand.New(rand.NewSource(7)).Shuffle(len(plan), func(i, j int) { plan[i], plan[j] = plan[j], plan[i] })

	var wg sync.WaitGroup
	errs := make([]error, len(plan))
	for i, seg := range plan {
		wg.Add(1)
		go func(i int, offset, length int64) {
			defer wg.Done()
			_, errs[i] = putChunk(t, u, param, content, offset, length)
		}(i, seg[0], seg[1])
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent chunk %d failed: %v", i, err)
		}
	}

	progress := progressOf(t, u, param)
	if !progress.BlobComplete || len(progress.MissingRanges) != 0 || progress.Size != param.Size {
		t.Fatalf("bitmap has holes after concurrent chunk upload: %+v", progress)
	}
	if progress.BlockSize != testBlockSize {
		t.Fatalf("progress must report the authoritative block size, got %d", progress.BlockSize)
	}
	if got := readBlobPayload(t, repos, "models", orgRepo, param.Sha256, param.Size); !bytes.Equal(got, content) {
		t.Fatalf("blob payload mismatch after concurrent chunk upload")
	}

	published := mustPublish(t, u, publishParam("main", manifestItem("weights.bin", content)))
	if published.Status != "published" || published.Added != 1 {
		t.Fatalf("unexpected publish result: %+v", published)
	}
	// 分块上传不走暂存 + rename，因此不该留下任何 .uploading 残留。
	assertNoStagedResidue(t, repos)
}

// §7 重复上传同一块 → already_present，不重复写。
func TestChunkUploadIsIdempotent(t *testing.T) {
	u, _ := newTestUploadDao(t)
	content := randomContent(testBlockSize*4, 2)
	param := chunkParam("weights.bin", content)

	first := mustPutChunk(t, u, param, content, 0, testBlockSize*2)
	if first.Status != "written" || first.Blocks != 2 {
		t.Fatalf("unexpected first chunk result: %+v", first)
	}
	again := mustPutChunk(t, u, param, content, 0, testBlockSize*2)
	if again.Status != "already_present" || again.Blocks != 0 {
		t.Fatalf("repeated chunk must take the idempotent fast path: %+v", again)
	}

	// 与已写入的块部分重叠：已置位的块被跳过，只补新的那一块。
	overlap := mustPutChunk(t, u, param, content, testBlockSize, testBlockSize*2)
	if overlap.Status != "written" || overlap.Blocks != 1 {
		t.Fatalf("overlapping chunk must only write the missing block: %+v", overlap)
	}
}

// §7 chunk sha 不匹配 → 400，且该块位仍为 0，磁盘无任何字节被改。
func TestChunkUploadShaMismatchLeavesDiskUntouched(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	content := randomContent(testBlockSize*4, 3)
	param := chunkParam("weights.bin", content)

	// 先写好前两块，用来验证失败的分块不会波及已经落盘的内容。
	mustPutChunk(t, u, param, content, 0, testBlockSize*2)
	before := readBlobPayload(t, repos, "models", orgRepo, param.Sha256, param.Size)

	bad := param
	bad.Offset = testBlockSize * 2
	bad.Length = testBlockSize * 2
	bad.ChunkSha256 = sha256Hex([]byte("not the content that follows"))
	if _, err := u.UploadChunk(bad, bytes.NewReader(content[testBlockSize*2:])); err == nil {
		t.Fatalf("expected chunk sha mismatch to be rejected")
	} else if errorCode(err) != "UPLOAD_CHUNK_SHA_MISMATCH" {
		t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
	}

	after := readBlobPayload(t, repos, "models", orgRepo, param.Sha256, param.Size)
	if !bytes.Equal(before, after) {
		t.Fatalf("rejected chunk must not change a single byte on disk")
	}
	progress := progressOf(t, u, param)
	want := []LocalByteRange{{Offset: testBlockSize * 2, Length: testBlockSize * 2}}
	if fmt.Sprint(progress.MissingRanges) != fmt.Sprint(want) {
		t.Fatalf("rejected chunk must leave its blocks unset: got %v want %v", progress.MissingRanges, want)
	}

	// 用正确的摘要重发同一段：位是 0，所以必须能盖上去。
	if result := mustPutChunk(t, u, param, content, testBlockSize*2, testBlockSize*2); result.Blocks != 2 {
		t.Fatalf("retry after sha mismatch must write both blocks: %+v", result)
	}
	if got := readBlobPayload(t, repos, "models", orgRepo, param.Sha256, param.Size); !bytes.Equal(got, content) {
		t.Fatalf("payload mismatch after retry")
	}
}

// §7 非对齐 offset / 非整数倍长度 → 400。
func TestChunkUploadRejectsMisalignedRequests(t *testing.T) {
	content := randomContent(testBlockSize*4, 4)

	cases := []struct {
		name   string
		offset int64
		length int64
	}{
		{"offset not block aligned", 1, testBlockSize},
		{"length not a block multiple and not the tail", 0, testBlockSize + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, repos := newTestUploadDao(t)
			param := chunkParam("weights.bin", content)
			param.Offset = tc.offset
			param.Length = tc.length
			body := content[tc.offset : tc.offset+tc.length]
			param.ChunkSha256 = sha256Hex(body)
			if _, err := u.UploadChunk(param, bytes.NewReader(body)); err == nil {
				t.Fatalf("expected misaligned chunk to be rejected")
			} else if errorCode(err) != "UPLOAD_INVALID_ARGUMENT" {
				t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
			}
			progress := progressOf(t, u, chunkParam("weights.bin", content))
			if len(progress.MissingRanges) != 1 || progress.MissingRanges[0].Length != param.Size {
				t.Fatalf("rejected request must not set any block: %+v", progress)
			}
			assertNoStagedResidue(t, repos)
		})
	}

	// 尾块允许不足整块。
	t.Run("tail chunk may be shorter than a block", func(t *testing.T) {
		u, _ := newTestUploadDao(t)
		tail := randomContent(testBlockSize*2+3, 5)
		param := chunkParam("weights.bin", tail)
		mustPutChunk(t, u, param, tail, 0, testBlockSize*2)
		result := mustPutChunk(t, u, param, tail, testBlockSize*2, 3)
		if result.Blocks != 1 {
			t.Fatalf("tail chunk must write its block: %+v", result)
		}
		if progress := progressOf(t, u, param); !progress.BlobComplete {
			t.Fatalf("file must be complete after the tail chunk: %+v", progress)
		}
	})
}

// §7 传一半中断 → 进度接口能列出缺块 → 只补缺块 → publish 成功。
func TestChunkUploadResumeFromMissingRanges(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	content := randomContent(testBlockSize*8, 6)
	param := chunkParam("weights.bin", content)

	// 只传第 0-1 块和第 4-5 块，中间与末尾各留一个空洞。
	mustPutChunk(t, u, param, content, 0, testBlockSize*2)
	mustPutChunk(t, u, param, content, testBlockSize*4, testBlockSize*2)

	progress := progressOf(t, u, param)
	if progress.Status != "uploading" || progress.BlobComplete {
		t.Fatalf("partial blob must report as uploading: %+v", progress)
	}
	want := []LocalByteRange{
		{Offset: testBlockSize * 2, Length: testBlockSize * 2},
		{Offset: testBlockSize * 6, Length: testBlockSize * 2},
	}
	if fmt.Sprint(progress.MissingRanges) != fmt.Sprint(want) {
		t.Fatalf("missing ranges mismatch: got %v want %v", progress.MissingRanges, want)
	}
	// 相邻缺块必须合并成一段，且首个空洞与老接口的顺序续传语义保持一致。
	if progress.ResumeOffset != testBlockSize*2 {
		t.Fatalf("resume offset must point at the first hole, got %d", progress.ResumeOffset)
	}

	// 发布此刻必须被拒：位图有空洞。
	if _, err := u.PublishFiles(publishParam("main", manifestItem("weights.bin", content))); err == nil {
		t.Fatalf("expected publish of an incomplete blob to be rejected")
	} else if errorCode(err) != "PUBLISH_CONTENT_NOT_READY" {
		t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
	}

	// 只补缺块。
	for _, gap := range progress.MissingRanges {
		mustPutChunk(t, u, param, content, gap.Offset, gap.Length)
	}
	if progress = progressOf(t, u, param); !progress.BlobComplete || len(progress.MissingRanges) != 0 {
		t.Fatalf("blob must be complete after filling the holes: %+v", progress)
	}
	if got := readBlobPayload(t, repos, "models", orgRepo, param.Sha256, param.Size); !bytes.Equal(got, content) {
		t.Fatalf("payload mismatch after resume")
	}
	mustPublish(t, u, publishParam("main", manifestItem("weights.bin", content)))
}

// §7 未 publish 的完整 blob，下载侧 404：清单是唯一的可见性闸门。
func TestCompleteChunkedBlobStaysInvisibleUntilPublish(t *testing.T) {
	u, _ := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	content := randomContent(testBlockSize*3, 8)
	param := chunkParam("subdir/weights.bin", content)
	mustPutChunk(t, u, param, content, 0, param.Size)

	if progress := progressOf(t, u, param); !progress.BlobComplete || progress.Effective {
		t.Fatalf("chunked blob must be complete but not effective: %+v", progress)
	}
	// 版本标签尚未指向任何快照，下载链路解析不到文件信息。
	if commit, _ := u.fileDao.GetCommitHfOffline("models", orgRepo, "main"); commit != "" {
		t.Fatalf("chunk upload must not create a revision, got commit %s", commit)
	}

	result := mustPublish(t, u, publishParam("main", manifestItem("subdir/weights.bin", content)))
	info, err := u.fileDao.GetPathsInfo("", "models", orgRepo, result.Commit, "", "subdir/weights.bin")
	if err != nil {
		t.Fatalf("published file must be resolvable: %v", err)
	}
	if info.Oid != param.Sha256 || info.Size != param.Size {
		t.Fatalf("unexpected paths info after publish: %+v", info)
	}
}

// 零字节文件：块数为 0，位图天然无空洞，publish 必须判定为完整。
func TestChunkUploadZeroByteFile(t *testing.T) {
	u, _ := newTestUploadDao(t)
	var empty []byte
	param := chunkParam("EMPTY", empty)

	result, err := u.UploadChunk(param, bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("zero-byte chunk upload failed: %v", err)
	}
	if result.Blocks != 0 || result.Status != "already_present" {
		t.Fatalf("unexpected zero-byte result: %+v", result)
	}
	progress := progressOf(t, u, param)
	if !progress.BlobComplete || len(progress.MissingRanges) != 0 || progress.Size != 0 {
		t.Fatalf("zero-byte blob must be complete: %+v", progress)
	}
	mustPublish(t, u, publishParam("main", manifestItem("EMPTY", empty)))
}

// 同一个 sha 上两次声明的大小不一致：必须拒绝，不能把内容写成谁都不认的样子。
func TestChunkUploadRejectsSizeBindingMismatch(t *testing.T) {
	u, _ := newTestUploadDao(t)
	content := randomContent(testBlockSize*3, 9)
	param := chunkParam("weights.bin", content)
	mustPutChunk(t, u, param, content, 0, testBlockSize)

	bad := param
	bad.Size = param.Size + testBlockSize
	bad.Offset = 0
	bad.Length = testBlockSize
	bad.ChunkSha256 = sha256Hex(content[:testBlockSize])
	if _, err := u.UploadChunk(bad, bytes.NewReader(content[:testBlockSize])); err == nil {
		t.Fatalf("expected size binding mismatch to be rejected")
	} else if errorCode(err) != "UPLOAD_SIZE_BINDING_MISMATCH" {
		t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
	}
}

// 请求体长度与声明的 chunk 长度不符时必须拒绝，且不置位。
func TestChunkUploadRejectsBodyLengthMismatch(t *testing.T) {
	content := randomContent(testBlockSize*2, 10)
	cases := []struct {
		name string
		body []byte
	}{
		{"body shorter than declared", content[:testBlockSize]},
		{"body longer than declared", append(append([]byte(nil), content...), 'x')},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, _ := newTestUploadDao(t)
			param := chunkParam("weights.bin", content)
			param.Offset = 0
			param.Length = int64(len(content))
			param.ChunkSha256 = sha256Hex(content)
			if _, err := u.UploadChunk(param, bytes.NewReader(tc.body)); err == nil {
				t.Fatalf("expected body length mismatch to be rejected")
			} else if errorCode(err) != "UPLOAD_INVALID_CONTENT" {
				t.Fatalf("unexpected error code: %s (%v)", errorCode(err), err)
			}
			progress := progressOf(t, u, chunkParam("weights.bin", content))
			if len(progress.MissingRanges) != 1 || progress.MissingRanges[0].Length != int64(len(content)) {
				t.Fatalf("rejected chunk must not set any block: %+v", progress)
			}
		})
	}
}

// 中途放弃的分块上传以不完整的 blobs/<sha> 形式存在，由未引用内容回收按保留期处理。
func TestIncompleteChunkedBlobIsReclaimedAsUnreferencedContent(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	content := randomContent(testBlockSize*4, 11)
	param := chunkParam("weights.bin", content)
	mustPutChunk(t, u, param, content, 0, testBlockSize*2)

	blobPath := filepath.Join(repos, "files", "models", orgRepo, "blobs", param.Sha256)
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("partial chunked blob must exist at the final location: %v", err)
	}
	// 保留期内不动它：还等着一次可能到来的续传。
	if removed, err := u.CleanupUnreferencedBlobs(0); err != nil || removed != 0 {
		t.Fatalf("fresh partial blob must be retained, removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("fresh partial blob was removed: %v", err)
	}
}

// 多个文件的分块交错并发上传，互不干扰。
func TestConcurrentChunkUploadsAcrossFilesDoNotBlockEachOther(t *testing.T) {
	u, repos := newTestUploadDao(t)
	const orgRepo = "dingo-local/demo"
	const fileCount = 6

	contents := make([][]byte, fileCount)
	params := make([]LocalChunkUploadParam, fileCount)
	for i := range contents {
		contents[i] = randomContent(testBlockSize*5+7, int64(100+i))
		params[i] = chunkParam(fmt.Sprintf("shard-%d.bin", i), contents[i])
	}

	var wg sync.WaitGroup
	errs := make(chan error, fileCount*8)
	for i := 0; i < fileCount; i++ {
		for _, seg := range chunkPlan(params[i].Size, testBlockSize*2) {
			wg.Add(1)
			go func(i int, offset, length int64) {
				defer wg.Done()
				if _, err := putChunk(t, u, params[i], contents[i], offset, length); err != nil {
					errs <- err
				}
			}(i, seg[0], seg[1])
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent chunk upload failed: %v", err)
	}

	files := make([]LocalManifestFile, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		if got := readBlobPayload(t, repos, "models", orgRepo, params[i].Sha256, params[i].Size); !bytes.Equal(got, contents[i]) {
			t.Fatalf("payload mismatch for shard %d", i)
		}
		files = append(files, manifestItem(fmt.Sprintf("shard-%d.bin", i), contents[i]))
	}
	if result := mustPublish(t, u, publishParam("main", files...)); result.Added != fileCount {
		t.Fatalf("unexpected publish result: %+v", result)
	}
}
