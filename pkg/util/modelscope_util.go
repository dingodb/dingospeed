package util

import (
	"crypto/tls"
	"dingospeed/pkg/transfersettings"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"dingospeed/pkg/config"

	"go.uber.org/zap"
)

// 提取ModelScope版本和Python版本的正则表达式
var (
	msVersionRegex = regexp.MustCompile(`modelscope/(\d+\.\d+\.\d+)`)
	pyVersionRegex = regexp.MustCompile(`python/(\d+\.\d+\.\d+)`)
)

// ParseClientEnv 增强：从客户端UA提取版本+返回真实系统架构
func ParseClientEnv(clientUA string) (msVersion, system, arch, pythonVer string) {
	if msMatch := msVersionRegex.FindStringSubmatch(clientUA); len(msMatch) > 1 {
		msVersion = msMatch[1]
	} else {
		msVersion = "1.33.0"
	}

	if pyMatch := pyVersionRegex.FindStringSubmatch(clientUA); len(pyMatch) > 1 {
		pythonVer = pyMatch[1]
	} else {
		pythonVer = "3.13.2"
	}

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

func AddCLIHeaders(header http.Header, clientUA string) {
	msVersion, system, arch, pythonVer := ParseClientEnv(clientUA)

	userAgent := fmt.Sprintf("modelscope/%s (%s; %s) Python/%s", msVersion, system, arch, pythonVer)
	header.Set("User-Agent", userAgent)
	zap.S().Infof("构建兼容的 User-Agent: %s (客户端原始 UA: %s)", userAgent, clientUA)

	header.Set("Accept-Encoding", "identity")
}

func EnsureDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		zap.S().Errorf("创建目录失败: %s, 错误: %v", filepath.Dir(path), err)
		return err
	}
	return nil
}

var (
	httpClientOnce   sync.Once
	globalHTTPClient *http.Client
)

// CreateHTTPClient 单例创建HTTP客户端
func CreateHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		transport := &http.Transport{
			MaxConnsPerHost:       0,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   50,
			IdleConnTimeout:       5 * time.Minute,
			DisableCompression:    true,
			DisableKeepAlives:     false,
			TLSHandshakeTimeout:   2 * time.Minute,
			ResponseHeaderTimeout: 5 * time.Minute,
			ExpectContinueTimeout: 1 * time.Minute,
			ForceAttemptHTTP2:     true,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				MaxVersion:         tls.VersionTLS13,
				InsecureSkipVerify: true, // 跳过证书校验，适配内网/代理场景，保留
				Renegotiation:      tls.RenegotiateFreelyAsClient,
			},
		}

		globalHTTPClient = &http.Client{
			Timeout:   30 * time.Minute, // 30分钟超时，完全适配7-8GB大文件下载，保留
			Transport: transport,
		}
	})

	return globalHTTPClient
}

// DoRequestWithRetry 带重试的HTTP请求
func DoRequestWithRetry(req *http.Request) (*http.Response, error) {
	client := CreateHTTPClient()
	var resp *http.Response
	var err error
	attempts := config.SysConfig.Modelscope.MaxRetry
	if attempts < 1 {
		attempts = 1 // A retry setting must never suppress the initial request.
	}

	for i := 0; i < attempts; i++ {
		resp, err = transfersettings.Do("modelscope", client, req)
		if err == nil {
			return resp, nil
		}

		if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline exceeded") {
			zap.S().Warnf("⚠️  Retry %d/%d: request timeout - %v", i+1, attempts, err)
			if i+1 == attempts {
				break
			}
			timer := time.NewTimer(time.Duration(config.SysConfig.Modelscope.RetryDelay) * time.Second * time.Duration(i+1))
			select {
			case <-req.Context().Done():
				timer.Stop()
				return nil, req.Context().Err()
			case <-timer.C:
			}
			continue
		}

		return nil, err
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", attempts, err)
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
