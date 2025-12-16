package data

import (
	"context"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"

	"github.com/bytedance/sonic"
	"go.uber.org/zap"
)

type LocalOperation struct {
	Type int    `json:"type"`
	Body string `json:"body"`
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

// 缓存预读取的文件块，默认每个文件16个块

var (
	FileBlockCache     Cache
	fileProcessChan    chan *FileProcessParam
	localOperationChan chan *LocalOperation
)

func GetFileProcessChan() <-chan *FileProcessParam {
	return fileProcessChan
}

func GetLocalOperationChan() chan *LocalOperation {
	return localOperationChan
}

func ReportFileProcess(ctx context.Context, processParam *FileProcessParam) {
	// 已升级到集群模型才有上报操作
	if config.SysConfig.GetOriginSchedulerModel() == consts.SchedulerModeCluster {
		if config.SysConfig.IsCluster() {
			if v := ctx.Value(consts.KeyProcessId); v != nil {
				processId := v.(int64)
				zap.S().Infof("processId:%d, startPos:%d, endPos:%d, status:%d", processId, processParam.StartPos, processParam.EndPos, processParam.Status)
				processParam.ProcessId = processId
				select {
				case fileProcessChan <- processParam:
					return
				case <-time.After(3 * time.Second):
					WriteLocalOperationChan(consts.OperationProcess, processParam)
					return
				}
			}
		}
		WriteLocalOperationChan(consts.OperationProcess, processParam)
	}
}

func WriteLocalOperationChan(operationType int, body interface{}) {
	marshal, err := sonic.Marshal(body)
	if err != nil {
		zap.S().Errorf("marshal err.%v", err)
		return
	}
	opera := &LocalOperation{
		Type: operationType,
		Body: string(marshal),
	}
	select {
	case localOperationChan <- opera:
	case <-time.After(3 * time.Second):
		zap.S().Errorf("localOperationChan write timeout...")
	}
}
