package service

import (
	"fmt"
	"sync"
	"time"

	"dingo-hfmirror/internal/model"
	"dingo-hfmirror/pkg/config"

	"github.com/shirou/gopsutil/mem"
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
