package service

import (
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"dingospeed/internal/dao"
	"dingospeed/internal/downloader"
	"dingospeed/pkg/config"

	"go.uber.org/zap"
)

type UploadService struct {
	uploadDao *dao.UploadDao
	limitMu   sync.Mutex
	inFlight  int
}

func NewUploadService(uploadDao *dao.UploadDao) *UploadService {
	return &UploadService{uploadDao: uploadDao}
}

// UploadWholeFile 一次调用上传一个完整文件。rawSize 是调用方声明的完整字节大小，
// 由本方法在身份校验之后解析，保证未通过身份校验的请求不会先收到参数错误。
func (u *UploadService) UploadWholeFile(param dao.LocalUploadParam, rawSize, rawStart, token string, body io.Reader) (*dao.LocalUploadResult, error) {
	if err := validateUploadToken(token); err != nil {
		return nil, err
	}
	size, err := ParseDeclaredSize(rawSize)
	if err != nil {
		return nil, uploadError{status: 400, code: "UPLOAD_INVALID_ARGUMENT", msg: err.Error()}
	}
	param.Size = size
	start, err := ParseResumeStart(rawStart)
	if err != nil {
		return nil, uploadError{status: 400, code: "UPLOAD_INVALID_ARGUMENT", msg: err.Error()}
	}
	param.Start = start
	if err := validateUploadParam(param); err != nil {
		return nil, uploadError{status: 400, code: "UPLOAD_INVALID_ARGUMENT", msg: err.Error()}
	}
	if param.Start != nil && *param.Start > param.Size {
		return nil, uploadError{status: 400, code: "UPLOAD_INVALID_ARGUMENT", msg: "start exceeds declared size"}
	}
	if !u.acquireUploadSlot() {
		return nil, uploadError{status: 429, code: "UPLOAD_CONCURRENCY_LIMIT", msg: "too many concurrent uploads"}
	}
	defer u.releaseUploadSlot()
	startLog := "full"
	if param.Start != nil {
		startLog = fmt.Sprintf("%d", *param.Start)
	}
	zap.S().Infof("local upload start: %s/%s/%s/%s size=%d sha256=%s overwrite=%t start=%s",
		param.RepoType, param.Org, param.Repo, param.FilePath, param.Size, param.Sha256, param.Overwrite, startLog)
	result, err := u.uploadDao.UploadWholeFile(param, body)
	if err != nil {
		if _, ok := err.(interface{ StatusCode() int }); ok {
			return nil, err
		}
		return nil, uploadError{status: 500, code: "UPLOAD_INTERNAL_ERROR", msg: err.Error()}
	}
	zap.S().Infof("local upload done: %s/%s/%s/%s status=%s commit=%s blobReused=%t",
		param.RepoType, param.Org, param.Repo, param.FilePath, result.Status, result.Commit, result.BlobReused)
	return result, nil
}

func (u *UploadService) QueryProgress(param dao.LocalUploadParam, token string) (*dao.LocalUploadProgress, error) {
	if err := validateUploadToken(token); err != nil {
		return nil, err
	}
	if err := validateUploadLocator(param); err != nil {
		return nil, uploadError{status: 400, code: "UPLOAD_INVALID_ARGUMENT", msg: err.Error()}
	}
	result, err := u.uploadDao.QueryProgress(param)
	if err != nil {
		if _, ok := err.(interface{ StatusCode() int }); ok {
			return nil, err
		}
		return nil, uploadError{status: 500, code: "UPLOAD_INTERNAL_ERROR", msg: err.Error()}
	}
	return result, nil
}

func ParseDeclaredSize(raw string) (int64, error) {
	if raw == "" {
		return 0, fmt.Errorf("size is required")
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("size is invalid")
	}
	return size, nil
}

func ParseResumeStart(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	start, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || start < 0 {
		return nil, fmt.Errorf("start is invalid")
	}
	return &start, nil
}

func validateUploadLocator(param dao.LocalUploadParam) error {
	if param.RepoType != "models" && param.RepoType != "datasets" {
		return fmt.Errorf("repoType must be models or datasets")
	}
	if param.Org != config.SysConfig.Upload.Namespace {
		return fmt.Errorf("org must be %s", config.SysConfig.Upload.Namespace)
	}
	if !validRepoSegment(param.Org) || !validRepoSegment(param.Repo) || !validRepoSegment(param.Revision) {
		return fmt.Errorf("org, repo, and revision must be safe relative path segments")
	}
	if param.FilePath == "" || strings.Contains(param.FilePath, "\\") {
		return fmt.Errorf("filePath is invalid")
	}
	clean := path.Clean(param.FilePath)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(param.FilePath) || clean != param.FilePath {
		return fmt.Errorf("filePath must be a clean relative path")
	}
	if len(param.FilePath) > maxFilePathLen {
		return fmt.Errorf("filePath is too long: max %d characters", maxFilePathLen)
	}
	for _, part := range strings.Split(param.FilePath, "/") {
		if !validFileNameSegment(part) {
			return fmt.Errorf("filePath contains an unsafe segment: %q", part)
		}
	}
	if !sha256Pattern.MatchString(param.Sha256) {
		return fmt.Errorf("sha256 must be a 64-character lowercase hex string")
	}
	return nil
}

func validateUploadParam(param dao.LocalUploadParam) error {
	if err := validateUploadLocator(param); err != nil {
		return err
	}
	maxSize := int64(downloader.DEFAULT_BLOCK_MASK_MAX) * config.SysConfig.Download.BlockSize
	if param.Size > maxSize {
		return fmt.Errorf("size exceeds cache format limit: max %d bytes", maxSize)
	}
	return nil
}

const (
	maxSegmentLen  = 255
	maxFilePathLen = 1024
)

// repoSegmentPattern 约束组织名、仓库名与版本标签。这三者会出现在对外 URL 和目录名里，
// 沿用上游站点的命名习惯即可，不需要放宽。
var repoSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// windowsReservedNames 是 Windows 上不能作为文件名使用的保留设备名。
var windowsReservedNames = map[string]struct{}{
	"con": {}, "prn": {}, "aux": {}, "nul": {},
	"com1": {}, "com2": {}, "com3": {}, "com4": {}, "com5": {}, "com6": {}, "com7": {}, "com8": {}, "com9": {},
	"lpt1": {}, "lpt2": {}, "lpt3": {}, "lpt4": {}, "lpt5": {}, "lpt6": {}, "lpt7": {}, "lpt8": {}, "lpt9": {},
}

func validRepoSegment(s string) bool {
	return utf8.ValidString(s) && repoSegmentPattern.MatchString(s) && s != "." && s != ".."
}

// validFileNameSegment 校验仓库内文件路径的单个片段。
// 这里用黑名单而不是字符白名单：真实仓库里普遍存在 .gitattributes 这类以点开头的文件，
// 以及带空格或非 ASCII 的文件名，用白名单会把正常仓库挡在门外。
// 安全性由“任何能改变路径含义的字符一律拒绝”来保证。
func validFileNameSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if !utf8.ValidString(s) || len(s) > maxSegmentLen {
		return false
	}
	if strings.ContainsAny(s, `/\:*?"<>|`) || hasControlChar(s) {
		return false
	}
	// Windows 会静默丢弃结尾的点和空格，导致最终落盘名与声明名不一致。
	if strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") || strings.HasSuffix(s, ".") {
		return false
	}
	base := s
	if idx := strings.Index(base, "."); idx >= 0 {
		base = base[:idx]
	}
	if _, ok := windowsReservedNames[strings.ToLower(base)]; ok {
		return false
	}
	return true
}

type uploadError struct {
	status int
	code   string
	msg    string
}

func validateUploadToken(token string) error {
	if config.SysConfig.Upload.Token == "" {
		return uploadError{status: 403, code: "UPLOAD_DISABLED", msg: "upload token is not configured"}
	}
	if token == "" {
		return uploadError{status: 401, code: "UPLOAD_TOKEN_MISSING", msg: "missing upload token"}
	}
	if token != config.SysConfig.Upload.Token {
		return uploadError{status: 403, code: "UPLOAD_TOKEN_INVALID", msg: "invalid upload token"}
	}
	return nil
}

func (e uploadError) Error() string {
	return e.msg
}

func (e uploadError) StatusCode() int {
	return e.status
}

func (e uploadError) ErrorCode() string {
	return e.code
}

func (u *UploadService) acquireUploadSlot() bool {
	limit := config.SysConfig.Upload.ConcurrentLimit
	if limit <= 0 {
		limit = 1
	}
	u.limitMu.Lock()
	defer u.limitMu.Unlock()
	if u.inFlight >= limit {
		return false
	}
	u.inFlight++
	return true
}

func (u *UploadService) releaseUploadSlot() {
	u.limitMu.Lock()
	defer u.limitMu.Unlock()
	if u.inFlight > 0 {
		u.inFlight--
	}
}

func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
