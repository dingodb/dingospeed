package service

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/util"

	"github.com/shirou/gopsutil/mem"
	"go.uber.org/zap"
)

var once sync.Once

type SysService struct {
}

func NewSysService() *SysService {
	sysSvc := &SysService{}
	once.Do(
		func() {
			if config.SysConfig.EnableReadBlockCache() {
				go sysSvc.MemoryUsed()
			}

			if config.SysConfig.DiskClean.Enabled {
				go sysSvc.cycleCheckDiskUsage()
			}

			testProxyConnectivity()
			if config.SysConfig.DynamicProxy.Enabled {
				go sysSvc.cycleTestProxyConnectivity()
			}
		})
	return sysSvc
}

func (s SysService) MemoryUsed() {
	ticker := time.NewTicker(config.SysConfig.GetCollectTimePeriod())
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			memoryInfo, err := mem.VirtualMemory()
			if err != nil {
				fmt.Printf("获取内存信息时出错: %v\n", err)
				continue
			}
			config.SystemInfo.SetMemoryUsed(time.Now().Unix(), memoryInfo.UsedPercent)
		}
	}
}

func (s SysService) cycleCheckDiskUsage() {
	ticker := time.NewTicker(config.SysConfig.GetDiskCollectTimePeriod())
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			checkDiskUsage()
		}
	}
}

// 检查磁盘使用情况
func checkDiskUsage() {
	if !config.SysConfig.Online() {
		return
	}
	if !config.SysConfig.DiskClean.Enabled {
		return
	}

	currentSize, err := util.GetFolderSize(config.SysConfig.Repos())
	if err != nil {
		zap.S().Errorf("Error getting folder size: %v", err)
		return
	}

	limitSize := config.SysConfig.DiskClean.CacheSizeLimit
	limitSizeH := util.ConvertBytesToHumanReadable(limitSize)
	currentSizeH := util.ConvertBytesToHumanReadable(currentSize)

	if currentSize < limitSize {
		return
	}

	zap.S().Infof("Cache size exceeded! Limit: %s, Current: %s.\n", limitSizeH, currentSizeH)
	zap.S().Infof("Cleaning...")

	filesPath := filepath.Join(config.SysConfig.Repos(), "files")

	var allFiles []util.FileWithPath
	switch config.SysConfig.CacheCleanStrategy() {
	case "LRU":
		allFiles, err = util.SortFilesByAccessTime(filesPath)
		if err != nil {
			zap.S().Errorf("Error sorting files by access time in %s: %v\n", filesPath, err)
			return
		}
	case "FIFO":
		allFiles, err = util.SortFilesByModifyTime(filesPath)
		if err != nil {
			zap.S().Errorf("Error sorting files by modify time in %s: %v\n", filesPath, err)
			return
		}
	case "LARGE_FIRST":
		allFiles, err = util.SortFilesBySize(filesPath)
		if err != nil {
			zap.S().Errorf("Error sorting files by size in %s: %v\n", filesPath, err)
			return
		}
	default:
		zap.S().Errorf("Unknown cache clean strategy: %s\n", config.SysConfig.CacheCleanStrategy())
		return
	}

	for _, file := range allFiles {
		if currentSize < limitSize {
			break
		}
		filePath := file.Path
		fileSize := file.Info.Size()
		err := os.Remove(filePath)
		if err != nil {
			zap.S().Errorf("Error removing file %s: %v\n", filePath, err)
			continue
		}
		currentSize -= fileSize
		zap.S().Infof("Remove file: %s. File Size: %s\n", filePath, util.ConvertBytesToHumanReadable(fileSize))
	}

	currentSize, err = util.GetFolderSize(config.SysConfig.Repos())
	if err != nil {
		zap.S().Errorf("Error getting folder size after cleaning: %v\n", err)
		return
	}
	currentSizeH = util.ConvertBytesToHumanReadable(currentSize)
	zap.S().Infof("Cleaning finished. Limit: %s, Current: %s.\n", limitSizeH, currentSizeH)
}

func (s SysService) cycleTestProxyConnectivity() {
	ticker := time.NewTicker(config.SysConfig.GetDynamicProxyTimePeriod())
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			testProxyConnectivity()
		}
	}
}

// 测试代理连通性
var successMsg = "，当前代理已恢复连通性"
var failMsg = "，当前代理无法连接，请检查网络或代理设置"

const (
	proxyTestTimeout = 5 * time.Second
	dialTimeout      = 3 * time.Second
)

// 新增：跟踪连续失败次数和是否已发送失败消息
var (
	continuousFailCount int
	hasSentFailureMsg   bool
)

// testProxyConnectivity 测试代理服务器的连接连通性
func testProxyConnectivity() {
	proxyURL, err := url.Parse(config.SysConfig.GetHttpProxy())
	if err != nil {
		util.ProxyIsAvailable = false
		zap.S().Warnf("代理URL解析失败: %v, 代理地址: %s", err, config.SysConfig.GetHttpProxy())
		handleContinuousFailure(proxyURL) // 解析失败也算一次失败
		return
	}

	// 创建优化的HTTP客户端
	testClient := createTestClient(proxyURL)

	// 执行代理测试请求
	req, err := http.NewRequest("HEAD", "https://www.google.com", nil)
	if err != nil {
		handleProxyTestError(err, "请求创建失败", proxyURL)
		return
	}

	// 设置标准化请求头
	setTestRequestHeaders(req)

	// 执行请求并处理响应
	handleProxyTestResponse(testClient, req, proxyURL)
}

// createTestClient 创建测试用的HTTP客户端
func createTestClient(proxyURL *url.URL) *http.Client {
	return &http.Client{
		Timeout: proxyTestTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			DialContext: (&net.Dialer{
				Timeout: dialTimeout,
			}).DialContext,
		},
	}
}

// setTestRequestHeaders 设置测试请求的标准头
func setTestRequestHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; proxy-test/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
}

// handleProxyTestError 统一处理代理测试错误
func handleProxyTestError(err error, errorMsg string, proxyURL *url.URL) {
	if util.ProxyIsAvailable {
		util.ProxyIsAvailable = false
	}
	handleContinuousFailure(proxyURL) // 处理连续失败计数
}

// handleProxyTestResponse 处理代理测试响应
func handleProxyTestResponse(client *http.Client, req *http.Request, proxyURL *url.URL) {
	resp, err := client.Do(req)
	if err != nil {
		handleProxyTestError(err, "代理请求执行失败", proxyURL)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusBadRequest {
		handleProxyTestSuccess(proxyURL)
		return
	}

	handleProxyTestFailure(resp.StatusCode, proxyURL)
}

// handleProxyTestSuccess 处理代理测试成功
func handleProxyTestSuccess(proxyURL *url.URL) {
	// 成功时重置失败计数器和标记
	continuousFailCount = 0
	hasSentFailureMsg = false

	if !util.ProxyIsAvailable {
		util.ProxyIsAvailable = true
		util.SendData(config.SysConfig.GetHttpProxyName() + successMsg) // 成功立即发消息
	}
	zap.S().Infof("代理请求测试成功: %s", proxyURL.String())
}

// handleProxyTestFailure 处理代理测试失败
func handleProxyTestFailure(statusCode int, proxyURL *url.URL) {
	util.ProxyIsAvailable = false
	zap.S().Warnf("代理测试返回非成功状态码: %d, 代理地址: %s", statusCode, proxyURL.String())
	handleContinuousFailure(proxyURL) // 处理连续失败计数
}

// 新增：处理连续失败计数逻辑
func handleContinuousFailure(proxyURL *url.URL) {
	// 每次失败累加计数器
	continuousFailCount++
	zap.S().Debugf("代理连续失败次数: %d, 代理地址: %s", continuousFailCount, proxyURL.String())

	// 当连续失败达到5次且未发送过失败消息时，发送通知
	if continuousFailCount >= config.SysConfig.GetMaxContinuousFails() && !hasSentFailureMsg {
		util.SendData(config.SysConfig.GetHttpProxyName() + failMsg)
		hasSentFailureMsg = true // 标记已发送，避免重复发送
		zap.S().Warnf("代理连续失败%d次，已发送通知: %s", config.SysConfig.GetMaxContinuousFails(), proxyURL.String())
	}
}
