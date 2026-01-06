package util

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"dingospeed/pkg/config"

	"go.uber.org/zap"
)

// 提取ModelScope版本和Python版本的正则表达式
var (
	msVersionRegex = regexp.MustCompile(`modelscope/(\d+\.\d+\.\d+)`)
	pyVersionRegex = regexp.MustCompile(`python/(\d+\.\d+\.\d+)`)
)

// 生成请求ID的辅助函数
func generateReqID() string {
	return "req-" + string(rand.Int63())
}

// ParseClientEnv 增强：从客户端UA提取版本+返回真实系统架构
func ParseClientEnv(clientUA string) (msVersion, system, arch, pythonVer string) {
	// 1. 提取ModelScope版本
	if msMatch := msVersionRegex.FindStringSubmatch(clientUA); len(msMatch) > 1 {
		msVersion = msMatch[1]
	} else {
		msVersion = "1.33.0" // 默认使用客户端主流版本
	}

	// 2. 提取Python版本
	if pyMatch := pyVersionRegex.FindStringSubmatch(clientUA); len(pyMatch) > 1 {
		pythonVer = pyMatch[1]
	} else {
		pythonVer = "3.13.2" // 默认使用客户端主流版本
	}

	// 3. 获取服务端真实系统/架构（替代Unknown）
	system = runtime.GOOS
	switch system {
	case "darwin":
		system = "macOS"
	case "windows":
		system = "Windows"
	case "linux":
		system = "Linux"
	}

	arch = runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}

	return
}

// AddCLIHeaders 修复：从客户端UA提取版本，构建兼容的User-Agent
func AddCLIHeaders(header http.Header, clientUA string) {
	msVersion, system, arch, pythonVer := ParseClientEnv(clientUA)

	userAgent := fmt.Sprintf("modelscope/%s (%s; %s) Python/%s", msVersion, system, arch, pythonVer)
	header.Set("User-Agent", userAgent)
	zap.S().Infof("构建兼容的 User-Agent: %s (客户端原始 UA: %s)", userAgent, clientUA)

	header.Set("Accept-Encoding", "identity")
	if header.Get("X-Request-ID") == "" {
		reqID := generateReqID()
		header.Set("X-Request-ID", reqID)
	}
}

// EnsureDir 确保目录存在
func EnsureDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		zap.S().Errorf("创建目录失败: %s, 错误: %v", filepath.Dir(path), err)
		return err
	}
	return nil
}

func GetCachePath(repoType, repoId, revision, filePath string) (string, bool) {
	parts := strings.Split(repoId, "/")
	if len(parts) != 2 {
		zap.S().Errorf("无效的repoId格式: %s，需为 org/repo 格式", repoId)
		return "", false
	}

	var cacheRoot string
	switch repoType {
	case "datasets":
		cacheRoot = config.SysConfig.Modelscope.DatasetCacheRoot
	case "models", "": // 兼容原逻辑，空值默认走 models
		cacheRoot = config.SysConfig.Modelscope.ModelCacheRoot
	default:
		zap.S().Warnf("未知的repoType: %s，默认使用models缓存目录", repoType)
		cacheRoot = config.SysConfig.Modelscope.ModelCacheRoot
	}

	targetCachePath := filepath.Join(cacheRoot, parts[0], parts[1], revision, filepath.Clean(filePath))
	fileInfo, err := os.Stat(targetCachePath)
	if err == nil {
		zap.S().Debugf("缓存文件存在: %s, 大小: %d字节", targetCachePath, fileInfo.Size())
		return targetCachePath, true
	}

	_ = EnsureDir(targetCachePath)
	return targetCachePath, false
}

// createHTTPClient 创建宽松超时的HTTP客户端
func CreateHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Minute,
		Transport: &http.Transport{
			MaxIdleConns:          10,
			IdleConnTimeout:       5 * time.Minute,
			DisableCompression:    true,
			MaxConnsPerHost:       2,
			DisableKeepAlives:     false,
			TLSHandshakeTimeout:   2 * time.Minute,
			ResponseHeaderTimeout: 5 * time.Minute,
			ExpectContinueTimeout: 1 * time.Minute,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				MaxVersion:         tls.VersionTLS13,
				InsecureSkipVerify: true,
				Renegotiation:      tls.RenegotiateFreelyAsClient,
			},
		},
	}
}

// DoRequestWithRetry 带重试的HTTP请求
func DoRequestWithRetry(req *http.Request) (*http.Response, error) {
	client := CreateHTTPClient()
	var resp *http.Response
	var err error

	for i := 0; i < config.SysConfig.Modelscope.MaxRetry; i++ {
		resp, err = client.Do(req)
		if err == nil {
			return resp, nil
		}

		if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline exceeded") {
			zap.S().Warnf("⚠️  Retry %d/%d: request timeout - %v", i+1, config.SysConfig.Modelscope.MaxRetry, err)
			time.Sleep(time.Duration(config.SysConfig.Modelscope.RetryDelay) * time.Duration(i+1))
			continue
		}

		return nil, err
	}

	return nil, fmt.Errorf("failed after %d retries: %v", config.SysConfig.Modelscope.MaxRetry, err)
}

// ParseRangeHeader 解析Range请求头，返回起始字节和结束字节（-1表示到末尾）
func ParseRangeHeader(r *http.Request) (start int64, end int64, err error) {
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		return 0, -1, nil
	}

	// 解析Range头格式：bytes=start-end
	parts := strings.SplitN(rangeHeader, "=", 2)
	if len(parts) != 2 || parts[0] != "bytes" {
		return 0, -1, fmt.Errorf("invalid Range header: %s", rangeHeader)
	}

	rangeParts := strings.SplitN(parts[1], "-", 2)
	start, err = strconv.ParseInt(rangeParts[0], 10, 64)
	if err != nil {
		return 0, -1, fmt.Errorf("invalid start byte: %s, err: %v", rangeParts[0], err)
	}

	if len(rangeParts) == 2 && rangeParts[1] != "" {
		end, err = strconv.ParseInt(rangeParts[1], 10, 64)
		if err != nil {
			return 0, -1, fmt.Errorf("invalid end byte: %s, err: %v", rangeParts[1], err)
		}
	} else {
		end = -1
	}

	return start, end, nil
}

func GetAPIPathPrefix(repoType string) string {
	repoType = strings.TrimSpace(strings.ToLower(repoType))
	switch repoType {
	case "dataset", "datasets":
		return "datasets"
	case "model", "models":
		return "models"
	default:
		zap.S().Warnf("无效的repoType: %s，默认使用models", repoType)
		return "models"
	}
}
