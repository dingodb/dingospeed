package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dingo-hfmirror/internal/model"
	"dingo-hfmirror/pkg/config"
	"dingo-hfmirror/pkg/util"

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

func (s SysService) Info() *model.SystemInfo {
	sysInfo := &model.SystemInfo{}
	return sysInfo
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
