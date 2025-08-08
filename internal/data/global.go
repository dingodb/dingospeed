package data

import (
	"context"
	"time"

	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"

	"go.uber.org/zap"
)

// 缓存预读取的文件块，默认每个文件16个块

var FileBlockCache Cache
var fileProcessChan chan common.FileProcess

func GetFileProcessChan() <-chan common.FileProcess {
	return fileProcessChan
}

func ReportFileProcess(ctx context.Context, startPos, endPos int64, status int32) {
	if config.SysConfig.IsCluster() {
		if v := ctx.Value(consts.KeyProcessId); v != nil {
			processId := v.(int64)
			zap.S().Infof("processId:%d, startPos:%d, endPos:%d, status:%d", processId, startPos, endPos, status)
			select {
			case fileProcessChan <- common.FileProcess{
				ProcessId: processId,
				StartPos:  startPos,
				EndPos:    endPos,
				Status:    status,
			}:
			case <-time.After(time.Second):
				zap.S().Warnf("ReportFileProcess timeout...")
			}
		}
	}
}
