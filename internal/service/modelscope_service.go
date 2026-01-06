package service

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"dingospeed/pkg/config"
	"dingospeed/pkg/util"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type ModelscopeService struct{}

func NewModelscopeService() *ModelscopeService {
	return &ModelscopeService{}
}

func (s *ModelscopeService) ForwardModelInfo(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)

	zap.S().Infof("转发%s信息请求到官方: %s", apiPrefix, officialURL)
	return s.forwardRequest(c, officialURL)
}

func (s *ModelscopeService) ForwardRevisions(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/revisions?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)

	zap.S().Infof("转发%s版本请求到官方: %s", apiPrefix, officialURL)
	return s.forwardRequest(c, officialURL)
}

func (s *ModelscopeService) ForwardFileList(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/repo/files?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)

	zap.S().Infof("转发%s文件列表请求到官方: %s", apiPrefix, officialURL)
	return s.forwardRequest(c, officialURL)
}

func (s *ModelscopeService) ForwardRepoTree(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/repo/tree?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)

	zap.S().Infof("转发%s文件树请求到官方: %s", apiPrefix, officialURL)
	return s.forwardRequest(c, officialURL)
}

func (s *ModelscopeService) ForwardRepoTreeByDatasetId(c echo.Context, datasetId string) error {
	officialURL := fmt.Sprintf("%s/api/v1/datasets/%s/repo/tree?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		url.PathEscape(datasetId),
		c.Request().URL.RawQuery)

	zap.S().Infof("转发文件树请求到官方: %s", officialURL)
	return s.forwardRequest(c, officialURL)
}

// HandleFileDownload 处理ModelScope文件下载请求
func (s *ModelscopeService) HandleFileDownload(c echo.Context, owner, repo, repoType string) error {
	repoId := fmt.Sprintf("%s/%s", owner, repo)
	revision := c.Request().URL.Query().Get("Revision")
	filePath := c.Request().URL.Query().Get("FilePath")

	if revision == "" {
		revision = "master"
	}
	if filePath == "" {
		zap.S().Error("请求参数缺失: FilePath为空")
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "400",
			"error": "missing FilePath parameter",
		})
	}

	msCfg := config.SysConfig.Modelscope
	if err := util.EnsureDir(filepath.Join(msCfg.ModelCacheRoot, "dummy")); err != nil {
		zap.S().Errorf("初始化模型缓存根目录失败: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "init model cache root dir failed",
			"msg":   err.Error(),
		})
	}

	cachePath, cacheExists := util.GetCachePath(repoType, repoId, revision, filePath)
	if cachePath == "" {
		zap.S().Errorf("生成缓存路径失败: 无效的repoId格式 %s", repoId)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "get cache path failed",
			"msg":   "invalid repoId format, require org/repo",
		})
	}
	zap.S().Infof("生成缓存路径: %s (缓存文件是否存在: %t)", cachePath, cacheExists)

	// 创建缓存文件上级目录（util.EnsureDir需要传入文件路径，以创建其上级目录）
	if err := util.EnsureDir(cachePath); err != nil {
		zap.S().Errorf("初始化缓存文件上级目录失败: %s, err: %v", filepath.Dir(cachePath), err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "init cache file parent dir failed",
			"msg":   err.Error(),
		})
	}

	cachedSize := util.GetFileSize(cachePath)
	zap.S().Infof("缓存文件状态: %s (已下载: %d字节)", cachePath, cachedSize)
	if cacheExists && cachedSize == 0 {
		zap.S().Warnf("缓存文件存在但大小为0，视为无效缓存: %s", cachePath)
		if err := os.Remove(cachePath); err != nil {
			zap.S().Errorf("删除空缓存文件失败: %s, err: %v", cachePath, err)
		}
		cachedSize = 0
		cacheExists = false
	}

	zap.S().Infof("缓存文件状态: %s (已下载: %d字节)", cachePath, cachedSize)

	clientStart, clientEnd, err := util.ParseRangeHeader(c.Request())
	if err != nil {
		zap.S().Errorf("解析Range失败: %v", err)
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "400",
			"error": "parse Range header failed",
			"msg":   err.Error(),
		})
	}

	actualStart := clientStart
	if cachedSize > 0 && actualStart < cachedSize {
		actualStart = cachedSize
	}
	zap.S().Infof("续传起始位置: 客户端请求=%d, 缓存末尾=%d, 实际起始=%d", clientStart, cachedSize, actualStart)

	headerWritten := false
	c.Response().Header().Set("Transfer-Encoding", "chunked")
	c.Response().Header().Set("Content-Type", "application/octet-stream")
	c.Response().Header().Set("Access-Control-Expose-Headers", "Content-Range, Content-Type")

	var cacheWritten int64 = 0
	if cachedSize > 0 && clientStart < cachedSize {
		cacheWritten, headerWritten, err = s.writeCacheData(c, cachePath, clientStart, clientEnd, cachedSize, headerWritten)
		if err != nil {
			zap.S().Errorf("写入缓存数据失败: %v", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"code":  "500",
				"error": "write cache data failed",
				"msg":   err.Error(),
			})
		}

		if clientEnd != -1 && (clientStart+cacheWritten-1) >= clientEnd {
			zap.S().Infof("缓存数据已满足客户端Range请求，无需续传")
			return nil
		}
	}

	if err := s.downloadAndWriteRemaining(c, owner, repo, actualStart, clientEnd, cachePath, headerWritten, repoType); err != nil {
		return err
	}

	return nil
}

// forwardRequest 通用请求转发逻辑
func (s *ModelscopeService) forwardRequest(c echo.Context, officialURL string) error {
	req, err := http.NewRequest(http.MethodGet, officialURL, nil)
	if err != nil {
		zap.S().Errorf("构建请求失败: %v", err)
		return err
	}

	util.AddCLIHeaders(req.Header, c.Request().Header.Get("User-Agent"))

	for k, v := range c.Request().Header {
		req.Header[k] = v
	}

	resp, err := util.DoRequestWithRetry(req)
	if err != nil {
		zap.S().Errorf("转发请求失败: %v", err)
		return err
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		c.Response().Header()[k] = v
	}
	c.Response().WriteHeader(resp.StatusCode)

	_, err = io.Copy(c.Response(), resp.Body)
	if err != nil {
		zap.S().Errorf("复制响应体失败: %v", err)
		return err
	}
	return nil
}

// writeCacheData 写入缓存中的数据到响应
func (s *ModelscopeService) writeCacheData(c echo.Context, cachePath string, clientStart, clientEnd, cachedSize int64, headerWritten bool) (int64, bool, error) {
	cacheFile, err := os.Open(cachePath)
	if err != nil {
		zap.S().Errorf("打开缓存文件失败: %s, err: %v", cachePath, err)
		return 0, headerWritten, fmt.Errorf("open cache file failed: %w", err)
	}
	defer cacheFile.Close()

	if _, err := cacheFile.Seek(clientStart, io.SeekStart); err != nil {
		zap.S().Errorf("定位缓存文件失败: %s, err: %v", cachePath, err)
		return 0, headerWritten, fmt.Errorf("seek cache file failed: %w", err)
	}

	cacheEnd := cachedSize - 1
	if clientEnd != -1 && clientEnd < cacheEnd {
		cacheEnd = clientEnd
	}
	cacheResponseSize := cacheEnd - clientStart + 1
	if cacheResponseSize <= 0 {
		zap.S().Warnf("缓存响应大小无效: %d (clientStart: %d, cacheEnd: %d)", cacheResponseSize, clientStart, cacheEnd)
		return 0, headerWritten, nil
	}

	if !headerWritten {
		contentRange := fmt.Sprintf("bytes %d-%d/%d", clientStart, cacheEnd, cachedSize)
		c.Response().Header().Set("Content-Range", contentRange)
		c.Response().WriteHeader(http.StatusPartialContent)
		headerWritten = true
		zap.S().Infof("设置缓存响应头: Content-Range=%s", contentRange)
	}

	buf := make([]byte, config.SysConfig.Modelscope.ChunkSize)
	written := int64(0)
	for written < cacheResponseSize {
		if c.Request().Context().Err() != nil {
			zap.S().Warnf("客户端断开连接，停止返回缓存数据: %s", cachePath)
			return written, headerWritten, nil
		}

		readSize := cacheResponseSize - written
		if readSize > int64(len(buf)) {
			readSize = int64(len(buf))
		}

		n, err := cacheFile.Read(buf[:readSize])
		if n > 0 {
			if _, writeErr := c.Response().Write(buf[:n]); writeErr != nil {
				zap.S().Errorf("返回缓存数据失败: %s, err: %v", cachePath, writeErr)
				return written, headerWritten, fmt.Errorf("write cache data to response failed: %w", writeErr)
			}
			written += int64(n)

			if f, ok := c.Response().Writer.(http.Flusher); ok {
				f.Flush()
			}
		}

		if err == io.EOF {
			zap.S().Infof("缓存文件读取到EOF: %s, 已读取%d字节", cachePath, written)
			break
		}
		if err != nil {
			zap.S().Errorf("读取缓存数据失败: %s, err: %v", cachePath, err)
			return written, headerWritten, fmt.Errorf("read cache file failed: %w", err)
		}
	}

	zap.S().Infof("✅ 返回缓存数据完成: %s, 范围%d-%d (共%d字节)", cachePath, clientStart, cacheEnd, written)
	return written, headerWritten, nil
}

// downloadAndWriteRemaining 下载剩余部分并写入响应+缓存
func (s *ModelscopeService) downloadAndWriteRemaining(c echo.Context, owner, repo string, actualStart, clientEnd int64, cachePath string, headerWritten bool, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	query := c.Request().URL.RawQuery
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/repo?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		query,
	)
	zap.S().Infof("请求ModelScope官方地址: %s", officialURL)

	req, err := http.NewRequest(http.MethodGet, officialURL, nil)
	if err != nil {
		zap.S().Errorf("构建请求失败: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "build request failed",
			"msg":   err.Error(),
		})
	}

	skipHeaders := map[string]bool{
		"Range":      true,
		"User-Agent": true,
		"Host":       true,
	}
	for k, v := range c.Request().Header {
		key := strings.ToLower(k)
		if !skipHeaders[key] {
			req.Header[k] = v
		}
	}

	// 设置ModelScope CLI头信息
	util.AddCLIHeaders(req.Header, c.Request().Header.Get("User-Agent"))

	// 设置Range头，仅下载剩余部分
	rangeHeader := fmt.Sprintf("bytes=%d-", actualStart)
	if clientEnd != -1 {
		rangeHeader = fmt.Sprintf("bytes=%d-%d", actualStart, clientEnd)
	}
	req.Header.Set("Range", rangeHeader)
	zap.S().Infof("向官方请求剩余部分: %s", rangeHeader)

	resp, err := util.DoRequestWithRetry(req)
	if err != nil {
		zap.S().Errorf("下载剩余部分失败: %v", err)
		return c.JSON(http.StatusBadGateway, map[string]string{
			"code":  "502",
			"error": "download remaining failed",
			"msg":   err.Error(),
		})
	}
	defer resp.Body.Close()

	// 校验ModelScope官方响应状态码
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		zap.S().Errorf("ModelScope返回错误状态码: %d, URL: %s", resp.StatusCode, officialURL)
		errorMsg := fmt.Sprintf("modelscope server return status code: %d", resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusNotFound:
			return c.JSON(http.StatusNotFound, map[string]string{
				"code":  "404",
				"error": "resource not found",
				"msg":   "model or file does not exist on ModelScope",
			})
		case http.StatusForbidden:
			return c.JSON(http.StatusForbidden, map[string]string{
				"code":  "403",
				"error": "forbidden",
				"msg":   "no permission to access the resource",
			})
		default:
			return c.JSON(http.StatusBadGateway, map[string]string{
				"code":  "502",
				"error": "modelscope server error",
				"msg":   errorMsg,
			})
		}
	}

	totalFileSize := int64(-1)
	contentRange := resp.Header.Get("Content-Range")
	if contentRange != "" {
		parts := strings.Split(contentRange, "/")
		if len(parts) == 2 {
			parsedSize, err := strconv.ParseInt(parts[1], 10, 64)
			if err == nil {
				totalFileSize = parsedSize
			} else {
				zap.S().Warnf("解析Content-Range失败: %s, err: %v", contentRange, err)
			}
		}
	}

	cacheFile, err := os.OpenFile(cachePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0664)
	if err != nil {
		zap.S().Errorf("打开缓存文件失败: %s, err: %v", cachePath, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "open cache file failed",
			"msg":   err.Error(),
		})
	}
	defer cacheFile.Close()

	if !headerWritten {
		if resp.StatusCode == http.StatusPartialContent {
			c.Response().Header().Set("Content-Range", resp.Header.Get("Content-Range"))
			c.Response().WriteHeader(http.StatusPartialContent)
		} else {
			c.Response().WriteHeader(http.StatusOK)
		}
		headerWritten = true
		zap.S().Infof("设置续传响应头，状态码: %d", resp.StatusCode)
	}

	buf := make([]byte, config.SysConfig.Modelscope.ChunkSize)
	written := int64(0)
	for {
		if c.Request().Context().Err() != nil {
			zap.S().Warnf("客户端断开连接，停止续传: %s", cachePath)
			return nil
		}

		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := cacheFile.Write(buf[:n]); writeErr != nil {
				zap.S().Errorf("写入缓存失败: %s, err: %v", cachePath, writeErr)
				return c.JSON(http.StatusInternalServerError, map[string]string{
					"code":  "500",
					"error": "write cache failed",
					"msg":   writeErr.Error(),
				})
			}

			if _, writeErr := c.Response().Write(buf[:n]); writeErr != nil {
				if strings.Contains(writeErr.Error(), "http2: stream closed") ||
					strings.Contains(writeErr.Error(), "broken pipe") ||
					strings.Contains(writeErr.Error(), "connection reset by peer") {
					zap.S().Warnf("客户端断开连接，停止返回续传数据: %s, err: %v", cachePath, writeErr)
					return nil
				}
				zap.S().Errorf("返回续传数据失败: %s, err: %v", cachePath, writeErr)
				return c.JSON(http.StatusInternalServerError, map[string]string{
					"code":  "500",
					"error": "write response failed",
					"msg":   writeErr.Error(),
				})
			}

			written += int64(n)
			if f, ok := c.Response().Writer.(http.Flusher); ok {
				f.Flush()
			}

			if written%(100*1024*1024) == 0 {
				zap.S().Infof("续传进度: %dMB, 文件: %s", written/(1024*1024), cachePath)
			}
		}

		// 处理读取错误
		if err == io.EOF {
			// 刷盘确保数据写入磁盘
			if syncErr := cacheFile.Sync(); syncErr != nil {
				zap.S().Warnf("缓存文件刷盘失败: %s, err: %v", cachePath, syncErr)
			}
			zap.S().Infof("续传完成: %s, 共下载%d字节，完整文件%d字节", cachePath, written, totalFileSize)
			break
		}
		if err != nil {
			zap.S().Errorf("续传中断: %s, err: %v", cachePath, err)
			return c.JSON(http.StatusBadGateway, map[string]string{
				"code":  "502",
				"error": "download interrupted",
				"msg":   err.Error(),
			})
		}
	}

	return nil
}
