package service

import (
	"fmt"
	"io"
	"strconv"
	"sync"

	"dingospeed/internal/dao"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"

	"go.uber.org/zap"
)

// chunkLimiter 是分块上传专用的并发闸门。它与 UploadService.acquireUploadSlot 完全分开：
// 那把槽位是“整文件上传”语义，默认只有 4 个，套到分块上会让同一个文件的四个并发块
// 把其它所有文件挡成 429。这里存在的理由只有一个——分块内容必须整体进内存才能在
// 置位前算 sha，不设上限就是把内存交给调用方决定。
type chunkLimiter struct {
	mu       sync.Mutex
	inFlight int
}

var uploadChunkLimiter chunkLimiter

func (l *chunkLimiter) acquire() bool {
	limit := config.SysConfig.GetUploadChunkConcurrentLimit()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight >= limit {
		return false
	}
	l.inFlight++
	return true
}

func (l *chunkLimiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight > 0 {
		l.inFlight--
	}
}

// UploadChunk 校验并落盘一个分块。
//
// contentLength 必须是明确的字节数：幂等快路径要在读请求体之前就判断“本 chunk 覆盖的块
// 是否已经全部置位”，没有长度就算不出覆盖了哪些块，只能先把内容读进内存再丢掉。
func (u *UploadService) UploadChunk(param dao.LocalChunkUploadParam, rawSize, rawOffset, chunkSha string,
	contentLength int64, body io.Reader) (*dao.LocalChunkUploadResult, error) {
	if err := validateUploadLocator(dao.LocalUploadParam{
		RepoType: param.RepoType,
		Org:      param.Org,
		Repo:     param.Repo,
		Revision: param.Revision,
		FilePath: param.FilePath,
		Sha256:   param.Sha256,
	}); err != nil {
		return nil, chunkArgError(err.Error())
	}
	size, err := ParseDeclaredSize(rawSize)
	if err != nil {
		return nil, chunkArgError(err.Error())
	}
	param.Size = size
	offset, err := parseChunkOffset(rawOffset)
	if err != nil {
		return nil, chunkArgError(err.Error())
	}
	param.Offset = offset
	if !sha256Pattern.MatchString(chunkSha) {
		return nil, chunkArgError("chunkSha256 must be a 64-character lowercase hex string")
	}
	param.ChunkSha256 = chunkSha

	if contentLength < 0 {
		return nil, uploadError{
			status: 411,
			code:   "UPLOAD_LENGTH_REQUIRED",
			msg:    "chunk upload requires an explicit Content-Length",
		}
	}
	param.Length = contentLength

	maxSize := int64(downloader.DEFAULT_BLOCK_MASK_MAX) * config.SysConfig.Download.BlockSize
	if param.Size > maxSize {
		return nil, chunkArgError("size exceeds cache format limit: max %d bytes", maxSize)
	}
	if maxChunk := config.SysConfig.GetUploadChunkMaxBytes(); param.Length > maxChunk {
		return nil, uploadError{
			status: 413,
			code:   "UPLOAD_CHUNK_TOO_LARGE",
			msg:    fmt.Sprintf("chunk length %d exceeds the limit of %d bytes", param.Length, maxChunk),
		}
	}
	// 越界的 chunk 一定是调用方切分错了。放过去会写出一份大小与声明不符的内容，
	// 而 publish 只比对 size 与位图，发现不了内部错位。
	if param.Offset+param.Length > param.Size {
		return nil, chunkArgError("chunk [%d,%d) exceeds declared size %d", param.Offset, param.Offset+param.Length, param.Size)
	}

	if !uploadChunkLimiter.acquire() {
		return nil, uploadError{status: 429, code: "UPLOAD_CHUNK_CONCURRENCY_LIMIT", msg: "too many concurrent chunk uploads"}
	}
	defer uploadChunkLimiter.release()

	result, err := u.uploadDao.UploadChunk(param, body)
	if err != nil {
		if _, ok := err.(interface{ StatusCode() int }); ok {
			return nil, err
		}
		return nil, uploadError{status: 500, code: "UPLOAD_INTERNAL_ERROR", msg: err.Error()}
	}
	zap.S().Debugf("local chunk upload done: %s/%s/%s/%s offset=%d length=%d status=%s blocks=%d",
		param.RepoType, param.Org, param.Repo, param.FilePath, param.Offset, param.Length, result.Status, result.Blocks)
	return result, nil
}

func parseChunkOffset(raw string) (int64, error) {
	if raw == "" {
		return 0, fmt.Errorf("offset is required")
	}
	offset, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("offset is invalid")
	}
	return offset, nil
}

func chunkArgError(format string, args ...interface{}) uploadError {
	return uploadError{status: 400, code: "UPLOAD_INVALID_ARGUMENT", msg: fmt.Sprintf(format, args...)}
}
