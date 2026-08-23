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
func (u *UploadService) UploadWholeFile(param dao.LocalUploadParam, rawSize, rawStart string, body io.Reader) (*dao.LocalUploadResult, error) {
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

// PublishFiles 让一批已经完整落盘的文件一次性生效。
func (u *UploadService) PublishFiles(param dao.LocalPublishParam) (*dao.LocalPublishResult, error) {
	if err := validatePublishParam(param); err != nil {
		return nil, uploadError{status: 400, code: "PUBLISH_INVALID_ARGUMENT", msg: err.Error()}
	}
	zap.S().Infof("local publish start: %s/%s/%s revision=%s files=%d overwrite=%t",
		param.RepoType, param.Org, param.Repo, param.Revision, len(param.Files), param.Overwrite)
	result, err := u.uploadDao.PublishFiles(param)
	if err != nil {
		if _, ok := err.(interface{ StatusCode() int }); ok {
			return nil, err
		}
		return nil, uploadError{status: 500, code: "PUBLISH_INTERNAL_ERROR", msg: err.Error()}
	}
	zap.S().Infof("local publish done: %s/%s/%s revision=%s commit=%s status=%s added=%d replaced=%d unchanged=%d total=%d",
		param.RepoType, param.Org, param.Repo, param.Revision, result.Commit, result.Status,
		result.Added, result.Replaced, result.Unchanged, result.FileCount)
	return result, nil
}

func validatePublishParam(param dao.LocalPublishParam) error {
	if err := validateRepoLocator(param.RepoType, param.Org, param.Repo, param.Revision); err != nil {
		return err
	}
	return validateManifestList(param.Files)
}

// validateManifestList 校验一份清单本身的合法性，批量发布与整树发布共用。
//
// 空清单是合法的：它表示一个还没有任何文件的版本。新建仓库、新建 revision 都要先
// 有这样一个空版本，用户才有地方把文件加进去；清空一个 revision 同理。空清单在
// 存储层本来就是可表示的（缓存管理删到最后一个文件就会写出一份空清单），
// 在入口处禁掉它只会让这三个入口无法完成第一步。
func validateManifestList(files []dao.LocalManifestFile) error {
	if maxFiles := config.SysConfig.GetUploadPublishMaxFiles(); len(files) > maxFiles {
		return fmt.Errorf("too many files in one publish: %d, max %d", len(files), maxFiles)
	}
	maxSize := int64(downloader.DEFAULT_BLOCK_MASK_MAX) * config.SysConfig.Download.BlockSize
	seen := make(map[string]struct{}, len(files))
	for _, item := range files {
		if err := validateFileLocator(item.Path, item.Sha256); err != nil {
			return err
		}
		if item.Size < 0 {
			return fmt.Errorf("size is invalid for %s", item.Path)
		}
		if item.Size > maxSize {
			return fmt.Errorf("size of %s exceeds cache format limit: max %d bytes", item.Path, maxSize)
		}
		// 同一路径在一份清单里出现两次，说明调用方的清单本身就是错的。
		// 择一或者后者覆盖前者会让调用方拿到成功响应，生效的却不是他以为的那个文件。
		if _, ok := seen[item.Path]; ok {
			return fmt.Errorf("duplicate path in publish list: %s", item.Path)
		}
		seen[item.Path] = struct{}{}
	}
	return nil
}

// registrationRevision 是控制面写登记信息用的版本标签，不接受用户编辑。
const registrationRevision = "meta"

// PublishTree 用一份完整的目标清单取代某个 revision 当前的清单。
// 新增与删除在这里合成一次提交，只产生一个新快照。
func (u *UploadService) PublishTree(param dao.LocalPublishTreeParam) (*dao.LocalPublishTreeResult, error) {
	if err := validatePublishTreeParam(param); err != nil {
		return nil, uploadError{status: 400, code: "PUBLISH_TREE_INVALID_ARGUMENT", msg: err.Error()}
	}
	zap.S().Infof("local publish tree start: %s/%s/%s revision=%s base=%s files=%d",
		param.RepoType, param.Org, param.Repo, param.Revision, param.BaseCommit, len(param.Files))
	result, err := u.uploadDao.PublishTree(param)
	if err != nil {
		if _, ok := err.(interface{ StatusCode() int }); ok {
			return nil, err
		}
		return nil, uploadError{status: 500, code: "PUBLISH_TREE_INTERNAL_ERROR", msg: err.Error()}
	}
	zap.S().Infof("local publish tree done: %s/%s/%s revision=%s commit=%s previous=%s status=%s added=%d replaced=%d removed=%d total=%d",
		param.RepoType, param.Org, param.Repo, param.Revision, result.Commit, result.PreviousCommit, result.Status,
		result.Added, result.Replaced, result.Removed, result.FileCount)
	return result, nil
}

func validatePublishTreeParam(param dao.LocalPublishTreeParam) error {
	if err := validateRepoLocator(param.RepoType, param.Org, param.Repo, param.Revision); err != nil {
		return err
	}
	// 登记快照由控制面维护，用户编辑它会让登记信息与仓库状态对不上。
	if param.Revision == registrationRevision {
		return fmt.Errorf("revision %s is reserved for model registration and cannot be edited", registrationRevision)
	}
	if !sha256Pattern.MatchString(param.BaseCommit) {
		return fmt.Errorf("baseCommit must be a 64-character lowercase hex string")
	}
	// 目标清单为空意味着“把这个 revision 清空”。标签仍然保留，指向一份空清单，
	// 与新建出来还没加文件的 revision 是同一个状态；下载方拿到的是一个空仓库，
	// 而不是 404。要真正去掉这个版本是删除 revision，那是另一个入口的事。
	return validateManifestList(param.Files)
}

func (u *UploadService) QueryProgress(param dao.LocalUploadParam) (*dao.LocalUploadProgress, error) {
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
	if err := validateRepoLocator(param.RepoType, param.Org, param.Repo, param.Revision); err != nil {
		return err
	}
	return validateFileLocator(param.FilePath, param.Sha256)
}

// validateRepoLocator 校验定位到某一个仓库版本的四个字段。批量发布没有单个文件路径，
// 但这四个字段一样会参与磁盘路径拼接，校验不能因为“换了个入口”就少做一遍。
func validateRepoLocator(repoType, org, repo, revision string) error {
	if repoType != "models" && repoType != "datasets" {
		return fmt.Errorf("repoType must be models or datasets")
	}
	if org != config.SysConfig.Upload.Namespace {
		return fmt.Errorf("org must be %s", config.SysConfig.Upload.Namespace)
	}
	if !validRepoSegment(org) || !validRepoSegment(repo) || !validRepoSegment(revision) {
		return fmt.Errorf("org, repo, and revision must be safe relative path segments")
	}
	return nil
}

func validateFileLocator(filePath, sha string) error {
	if filePath == "" || strings.Contains(filePath, "\\") {
		return fmt.Errorf("filePath is invalid")
	}
	clean := path.Clean(filePath)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(filePath) || clean != filePath {
		return fmt.Errorf("filePath must be a clean relative path")
	}
	if len(filePath) > maxFilePathLen {
		return fmt.Errorf("filePath is too long: max %d characters", maxFilePathLen)
	}
	for _, part := range strings.Split(filePath, "/") {
		if !validFileNameSegment(part) {
			return fmt.Errorf("filePath contains an unsafe segment: %q", part)
		}
	}
	if !sha256Pattern.MatchString(sha) {
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
