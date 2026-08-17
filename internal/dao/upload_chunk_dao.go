package dao

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"

	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"
	"dingospeed/pkg/util"
)

// 分块上传对标下载侧的缓存写入，不引入任何下载侧没有的机制：
// 同一个最终路径（blobs/<sha>）、同一个块位图、同一条“不覆盖已置位的块”规则、
// 同一个“失败就重传”模型。没有暂存文件、没有 rename、没有 finalize、没有状态机。
//
// 由此推出两条必须钉住的不变量：
//
//  1. 某块位 = 1 只表示“写入这块时其内容通过了 chunk 级 sha 校验”。它不承诺整个
//     文件正确——blob 名与下载侧的 etag 一样是外部声明值，信任层级一致。
//  2. “不覆盖”指不覆盖**已置位**的块，而不是不覆盖已有字节。上次写一半崩掉留下的
//     垃圾字节其块位是 0，重传必须能盖上去。
//
// 可见性闸门不在这里，而在清单：本地仓库的文件信息完全从 manifest 派生，
// 而 publish 时 verifyPublishContent → inspectCompleteBlob 会检查“位图无空洞 +
// size 匹配”。因此不完整的 blob 对下载侧天然不可见，分块上传不需要自己的生效开关。
type LocalChunkUploadParam struct {
	RepoType string
	Org      string
	Repo     string
	Revision string
	FilePath string
	// Sha256 是整文件摘要，也就是 blob 文件名；本接口不校验它。
	Sha256 string
	// Size 是整文件总字节数，用于绑定 blob 的 header。
	Size int64
	// Offset 是本 chunk 在整文件中的起始字节偏移，必须按块对齐。
	Offset int64
	// Length 是本 chunk 的字节数，取自 Content-Length。
	Length int64
	// ChunkSha256 是本 chunk 内容的摘要，是唯一被真正校验的东西。
	ChunkSha256 string
}

type LocalChunkUploadResult struct {
	RepoType  string `json:"repoType"`
	Repo      string `json:"repo"`
	Revision  string `json:"revision"`
	FilePath  string `json:"filePath"`
	Sha256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"`
	BlockSize int64  `json:"blockSize"`
	// Blocks 是本次真正写入的块数。已置位的块会被跳过，因此它可能小于本 chunk 覆盖的块数。
	Blocks int `json:"blocks"`
	// Status 为 already_present 表示本 chunk 覆盖的块此前已全部置位，请求体未被读取。
	Status string `json:"status"`
}

func uploadBlobLockKey(repoType, orgRepo, sha string) string {
	return fmt.Sprintf("upload-blob:%s:%s:%s", repoType, orgRepo, sha)
}

// UploadChunk 把一个 chunk 的内容写进 blobs/<sha>，并为其中每个整块置位。
//
// 它刻意不走 tryEnterLocalUpload 与上传槽位：那两者都是“整文件”粒度的语义，
// 用在这里会让同一文件的第二个并发块直接 409、或者让四个并发块把其它文件全挡成 429，
// 而乱序并发正是分块上传存在的理由。并发安全由三层保证：
// blob 读锁（挡住老接口的 rename 与回收的 remove）、DingCacheManager 的进程内唯一
// 句柄（位图不丢更新、header 不撕裂）、以及 WriteBlock 内部 fileLock 串行的置位与刷盘。
func (u *UploadDao) UploadChunk(param LocalChunkUploadParam, body io.Reader) (*LocalChunkUploadResult, error) {
	orgRepo := util.GetOrgRepo(param.Org, param.Repo)
	repos := config.SysConfig.Repos()
	blobPath := localBlobPath(param.RepoType, orgRepo, param.Sha256)
	if err := ensureLocalUploadPathSafe(repos, blobPath); err != nil {
		return nil, err
	}
	if err := util.MakeDirs(blobPath); err != nil {
		return nil, err
	}

	// 读锁：分块之间互不阻塞，但与“整体替换这个文件”的动作互斥。
	blobKey := uploadBlobLockKey(param.RepoType, orgRepo, param.Sha256)
	uploadBlobLocks.RLock(blobKey)
	defer uploadBlobLocks.RUnlock(blobKey)

	// 直接复用下载侧的句柄管理器。它顺带解决三件事：同一路径进程内唯一句柄、
	// 创建与 Resize 在管理器的 mu 下串行、Resize 只在首次（FileSize==0）执行一次。
	dingFile, err := downloader.GetInstance().GetDingFile(blobPath, param.Size)
	if err != nil {
		return nil, err
	}
	defer downloader.GetInstance().ReleasedDingFile(blobPath)

	// blob 的 header 一旦绑定了大小就是权威值。声明值与它不符说明调用方对同一个 sha
	// 抱有两种不同的大小认知，继续写下去只会写出一份谁都不认的内容。
	boundSize := dingFile.GetFileSize()
	if boundSize != param.Size {
		return nil, localUploadError{
			status: http.StatusConflict,
			code:   "UPLOAD_SIZE_BINDING_MISMATCH",
			msg:    fmt.Sprintf("blob is bound to size %d, declared size is %d", boundSize, param.Size),
		}
	}

	// 块大小以 header 为准，不以配置为准：文件创建之后配置被改过的话，
	// 按配置切分就是静默的数据损坏。进度接口把这个值回给 agent 也是同一个理由。
	blockSize := dingFile.GetBlockSize()
	if blockSize <= 0 {
		return nil, fmt.Errorf("invalid cache block size: %d", blockSize)
	}
	if err = validateChunkAlignment(param, blockSize); err != nil {
		return nil, err
	}

	result := &LocalChunkUploadResult{
		RepoType:  param.RepoType,
		Repo:      orgRepo,
		Revision:  param.Revision,
		FilePath:  param.FilePath,
		Sha256:    param.Sha256,
		Size:      param.Size,
		Offset:    param.Offset,
		Length:    param.Length,
		BlockSize: blockSize,
		Status:    "already_present",
	}

	// 零字节 chunk 不覆盖任何块。它唯一的作用是让零字节文件的 blob 被创建出来
	// ——上面的 GetDingFile 已经做完了，位图天然“无空洞”，publish 会判定为完整。
	if param.Length == 0 {
		return result, nil
	}

	firstBlock := param.Offset / blockSize
	lastBlock := (param.Offset + param.Length - 1) / blockSize

	// 幂等快路径：本 chunk 覆盖的块已经全部置位，连请求体都不读。
	allPresent, err := allBlocksPresent(dingFile, firstBlock, lastBlock)
	if err != nil {
		return nil, err
	}
	if allPresent {
		return result, nil
	}

	// 校验必须发生在置位之前，所以内容要先整体进内存。上限由服务层的
	// maxChunkBytes 与分块并发上限共同约束（内存 = 并发数 × chunk 大小）。
	buf := make([]byte, param.Length)
	if _, err = io.ReadFull(body, buf); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, badLocalUploadRequest("body shorter than declared chunk length %d", param.Length)
		}
		return nil, err
	}
	var extra [1]byte
	if n, readErr := body.Read(extra[:]); n > 0 {
		return nil, badLocalUploadRequest("body longer than declared chunk length %d", param.Length)
	} else if readErr != nil && readErr != io.EOF {
		return nil, readErr
	}

	sum := sha256.Sum256(buf)
	if actual := hex.EncodeToString(sum[:]); actual != param.ChunkSha256 {
		return nil, localUploadError{
			status: http.StatusBadRequest,
			code:   "UPLOAD_CHUNK_SHA_MISMATCH",
			msg:    fmt.Sprintf("chunk sha256 mismatch: declared %s, actual %s", param.ChunkSha256, actual),
		}
	}

	// 到这里为止磁盘上一个字节都没被改动：校验没过的内容不会留下任何痕迹，
	// 相关块的位也仍然是 0。
	written, err := writeChunkBlocks(dingFile, param, blockSize, firstBlock, lastBlock, buf)
	if err != nil {
		return nil, err
	}
	result.Blocks = written
	result.Status = "written"
	return result, nil
}

func allBlocksPresent(dingFile *downloader.DingCache, firstBlock, lastBlock int64) (bool, error) {
	for i := firstBlock; i <= lastBlock; i++ {
		has, err := dingFile.HasBlock(i)
		if err != nil {
			return false, err
		}
		if !has {
			return false, nil
		}
	}
	return true, nil
}

// writeChunkBlocks 逐块落盘并置位，跳过已置位的块。
//
// 这里刻意不给「查位 → 置位」加互斥，与下载侧 remote_task.go 的形态完全一致：
// 两个覆盖同一块的分块可能都判定为「未置位」而都写一遍，写的是同样的字节，无害；
// 加锁也换不来更强的保证（无非是二者取一），却会把同一个 blob 的分块写者串成一队。
// 实测 8 并发 128 块的场景下，串行化会让墙钟耗时从 33ms 涨到 47ms。
//
// 前提是 DingCache 的位图读写本身是并发安全的——这一条曾经不成立
// （HasBlock 走 headerLock、setHeaderBlock 走 fileLock，互不排斥），
// 现已在 file.go 的 WriteBlock 内修正，并由
// downloader.TestConcurrentHasBlockAndWriteBlock 守住。
func writeChunkBlocks(dingFile *downloader.DingCache, param LocalChunkUploadParam,
	blockSize, firstBlock, lastBlock int64, buf []byte) (int, error) {
	written := 0
	block := make([]byte, blockSize)
	for i := firstBlock; i <= lastBlock; i++ {
		has, err := dingFile.HasBlock(i)
		if err != nil {
			return written, err
		}
		if has {
			continue
		}
		lo := i*blockSize - param.Offset
		hi := lo + blockSize
		if hi > param.Length {
			hi = param.Length
		}
		copy(block, buf[lo:hi])
		// WriteBlock 要求 buffer 恰好是一整块长，尾块不足的部分补零；
		// 真正落盘时它会按 FileSize 截断，补的零不会被写进文件。
		clear(block[hi-lo:])
		if err = dingFile.WriteBlock(i, block); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// validateChunkAlignment 用 header 里的权威块大小做对齐校验。
//
// 长度必须是块的整数倍，唯一的例外是文件尾块。放宽这一条会让一个跨在块中间结束的
// chunk 把半块内容置位，之后任何补齐都会因为“不覆盖已置位的块”而被跳过——
// 那是一份位图声称完整、内容却缺了一截的 blob，且没有任何环节能再发现它。
func validateChunkAlignment(param LocalChunkUploadParam, blockSize int64) error {
	if param.Offset%blockSize != 0 {
		return chunkAlignmentError("offset %d is not a multiple of block size %d", param.Offset, blockSize)
	}
	isTail := param.Offset+param.Length == param.Size
	if param.Length%blockSize != 0 && !isTail {
		return chunkAlignmentError("length %d is not a multiple of block size %d and does not end at the file tail",
			param.Length, blockSize)
	}
	maxBlocks := int64(downloader.DEFAULT_BLOCK_MASK_MAX)
	if param.Size > maxBlocks*blockSize {
		return chunkAlignmentError("size %d exceeds cache format limit: max %d bytes", param.Size, maxBlocks*blockSize)
	}
	return nil
}

func chunkAlignmentError(format string, args ...interface{}) localUploadError {
	return localUploadError{
		status: http.StatusBadRequest,
		code:   "UPLOAD_INVALID_ARGUMENT",
		msg:    fmt.Sprintf(format, args...),
	}
}
