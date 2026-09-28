package util

import (
	"crypto/tls"
	"dingospeed/pkg/proxypool"
	"dingospeed/pkg/transfersettings"
	"fmt"
	"net"
	"net/http"
	"net/url"
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
		globalHTTPClient = newModelScopeHTTPClient("")
	})
	return globalHTTPClient
}

// Keep ModelScope routing independent from HF's host-rewriting proxy pool.
// Capture startup configuration; changing it requires a process restart.
func newModelScopeHTTPClient(proxyAddress string) *http.Client {
	transport := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			if proxyAddress == "" {
				return nil, nil
			}
			return url.Parse(proxyAddress)
		},
		MaxConnsPerHost:       0,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       5 * time.Minute,
		DisableCompression:    true,
		DisableKeepAlives:     false,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Minute,
		ForceAttemptHTTP2:     true,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			MaxVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true, // 跳过证书校验，适配内网/代理场景，保留
			Renegotiation:      tls.RenegotiateFreelyAsClient,
		},
	}

	return &http.Client{
		Timeout:   30 * time.Minute, // 30分钟超时，完全适配7-8GB大文件下载，保留
		Transport: transport,
	}
}

// DoRequestWithRetry 带重试的HTTP请求
func DoRequestWithRetry(req *http.Request) (*http.Response, error) {
	attempts := config.SysConfig.Modelscope.MaxRetry
	if attempts < 1 {
		attempts = 1
	}
	tried := map[*proxypool.Member]bool{}
	var last error
	for i := 0; i < attempts; i++ {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		r, err := constructSourceRoute("modelscope", req.Method, tried)
		if err != nil {
			return nil, fmt.Errorf("%w (previous: %v)", err, last)
		}
		if r.member != nil {
			tried[r.member] = true
		}
		resp, err := transfersettings.Do("modelscope", r.client, req)
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		if req.Context().Err() == nil {
			r.report(code, err)
		}
		// Permission and missing-repository responses are terminal business results.
		if err == nil && !proxypool.IsFailure(code, nil) {
			return resp, nil
		}
		if req.Context().Err() != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return nil, req.Context().Err()
		}
		// The shared transport already exhausted healthy outlets and optional
		// fallback; do not replay that entire chain with a fresh retry budget.
		if r.managed {
			return resp, err
		}
		if err == nil && i+1 == attempts {
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		last = err
		if last == nil {
			last = fmt.Errorf("upstream HTTP %d", code)
		}
		if i+1 == attempts {
			break
		}
		timer := time.NewTimer(time.Duration(config.SysConfig.Modelscope.RetryDelay) * time.Second)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("modelscope upstream failed after %d attempts: %w", attempts, last)
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
