package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
			if config.SysConfig.Cache.Enabled {
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
var mapProxy = map[string]string{
	"http://100.64.1.68:1080":   "马鞍山服务器",
	"http://10.201.20.164:1080": "黄海服务器",
	"http://10.201.44.68:1080":  "黄山服务器",
}
var successMsg = "，当前代理已恢复连通性"
var failMsg = "，当前代理无法连接，请检查网络或代理设置"
var webhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=1058c6dd-d1ba-480e-9d86-04cc326d35d0"

const (
	proxyTestTimeout  = 5 * time.Second
	dialTimeout       = 3 * time.Second
	successStatusCode = 200
	failureStatusCode = 400
)

// testProxyConnectivity 测试代理服务器的连接连通性
func testProxyConnectivity() {
	proxyURL, err := url.Parse(config.SysConfig.GetHttpProxy())
	if err != nil {
		util.ProxyIsAvailable = false
		zap.S().Warnf("代理URL解析失败: %v, 代理地址: %s", err, config.SysConfig.GetHttpProxy())
		return
	}

	// 创建优化的HTTP客户端
	testClient := createTestClient(proxyURL)

	// 执行代理测试请求
	req, err := http.NewRequest("GET", "https://www.google.com", nil)
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
	// 可添加更多标准请求头
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
}

// handleProxyTestError 统一处理代理测试错误
func handleProxyTestError(err error, errorMsg string, proxyURL *url.URL) {
	if util.ProxyIsAvailable {
		util.ProxyIsAvailable = false
		sendData(mapProxy[config.SysConfig.GetHttpProxy()] + failMsg)
	}
	zap.S().Warnf("代理测试%s: %v, 代理地址: %s", errorMsg, err, proxyURL.String())
}

// handleProxyTestResponse 处理代理测试响应
func handleProxyTestResponse(client *http.Client, req *http.Request, proxyURL *url.URL) {
	resp, err := client.Do(req)
	if err != nil {
		handleProxyTestError(err, "代理请求执行失败", proxyURL)
		return
	}
	defer resp.Body.Close()

	// 读取响应体（防止连接池问题）
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode >= successStatusCode && resp.StatusCode < failureStatusCode {
		handleProxyTestSuccess(proxyURL)
		return
	}

	handleProxyTestFailure(resp.StatusCode, proxyURL)
}

// handleProxyTestSuccess 处理代理测试成功
func handleProxyTestSuccess(proxyURL *url.URL) {
	if !util.ProxyIsAvailable {
		util.ProxyIsAvailable = true
		sendData(mapProxy[config.SysConfig.GetHttpProxy()] + successMsg)
	}
	zap.S().Infof("代理请求测试成功: %s", proxyURL.String())
}

// handleProxyTestFailure 处理代理测试失败
func handleProxyTestFailure(statusCode int, proxyURL *url.URL) {
	util.ProxyIsAvailable = false
	zap.S().Warnf("代理测试返回非成功状态码: %d, 代理地址: %s", statusCode, proxyURL.String())
	sendData(mapProxy[config.SysConfig.GetHttpProxy()] + failMsg)
}

func sendData(content string) {
	msg := Message{
		MsgType: "text",
		Text: TextMsg{
			Content: content,
		},
	}

	if err := sendMessage(msg); err != nil {
		zap.S().Errorf("微信机器人发送消息失败: %v", err)
	}

	return
}

// Response 企业微信 API 返回结果
type Response struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// Message 通用消息结构
type Message struct {
	MsgType string  `json:"msgtype"`
	Text    TextMsg `json:"text,omitempty"`
}

// TextMsg 文本消息
type TextMsg struct {
	Content             string   `json:"content"`
	MentionedList       []string `json:"mentioned_list,omitempty"`
	MentionedMobileList []string `json:"mentioned_mobile_list,omitempty"`
}

// sendMessage 发送消息的基础方法
func sendMessage(msg Message) error {
	// 转换为 JSON
	jsonData, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("消息序列化失败: %v", err)
	}

	// 创建请求
	req, err := http.NewRequest("POST", webhook, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("创建请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 发送请求
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("发送请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取响应失败: %v", err)
	}

	// 解析响应
	var result Response
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("解析响应失败: %v, 响应内容: %s", err, string(body))
	}

	if result.ErrCode != 0 {
		return fmt.Errorf("发送消息失败: %s", result.ErrMsg)
	}

	return nil
}
