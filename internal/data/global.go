package data

import (
	"context"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"

	"go.uber.org/zap"
)

// 缓存预读取的文件块，默认每个文件16个块

var (
	FileBlockCache   Cache
	fileProcessChan  chan *FileProcessParam
	localProcessChan chan *FileProcessParam
)

func GetFileProcessChan() <-chan *FileProcessParam {
	return fileProcessChan
}

func GetLocalProcessChan() chan *FileProcessParam {
	return localProcessChan
}

type FileProcessParam struct {
	ProcessId int64  `json:"processId"`
	Datatype  string `json:"datatype"`
	Org       string `json:"org"`
	Repo      string `json:"repo"`
	Name      string `json:"name"`
	Etag      string `json:"etag"`
	FileSize  int64  `json:"fileSize"`
	StartPos  int64  `json:"startPos"`
	EndPos    int64  `json:"endPos"`
	Status    int32  `json:"status"`
}

func ReportFileProcess(ctx context.Context, processParam *FileProcessParam) {
	if config.SysConfig.IsCluster() {
		if v := ctx.Value(consts.KeyProcessId); v != nil {
			processId := v.(int64)
			zap.S().Infof("processId:%d, startPos:%d, endPos:%d, status:%d", processId, processParam.StartPos, processParam.EndPos, processParam.Status)
			processParam.ProcessId = processId
			select {
			case fileProcessChan <- processParam:
				return
			case <-time.After(3 * time.Second):
				WriteLocalProcessChan(processParam)
				return
			}
		}
	}
	WriteLocalProcessChan(processParam)
}

func WriteLocalProcessChan(processParam *FileProcessParam) {
	select {
	case localProcessChan <- processParam:
	case <-time.After(3 * time.Second):
		zap.S().Errorf("localProcessChan write timeout...")
	}
}
