package task

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"

	"go.uber.org/zap"
)

type MountCacheTask struct {
	CacheTask
}

func (m *MountCacheTask) DoTask() {
	orgRepo := fmt.Sprintf("%s/%s", m.Job.Org, m.Job.Repo)
	var repoType string
	if m.Job.Datatype == consts.RepoTypeModel.Value() {
		repoType = "model"
	} else if m.Job.Datatype == consts.RepoTypeDataset.Value() {
		repoType = "dataset"
	} else {
		zap.S().Errorf("repotype err.%s", repoType)
		return
	}
	modelDirName := filepath.Base(orgRepo)
	mountDir := config.SysConfig.Cache.MountModelDir
	localModelDir := filepath.Join(mountDir, m.Job.Datatype, orgRepo)

	logDir := filepath.Join(config.SysConfig.Server.Repos, "mount_download_logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		zap.S().Errorf("创建目录失败: %v", err)
		return
	}
	logFileName := fmt.Sprintf("%s_%s.log", modelDirName, time.Now().Format("20060102_150405"))
	logFile := filepath.Join(logDir, logFileName)
	logF, err := os.Create(logFile)
	if err != nil {
		zap.S().Errorf("create err.%v", err)
		return
	}
	defer logF.Close()
	hfEndpoint := fmt.Sprintf("http://%s:%d", config.SysConfig.Server.Host, config.SysConfig.Server.Port)
	cmd := exec.Command("huggingface-cli", "download", "--resume-download", "--repo-type",
		repoType, orgRepo, "--local-dir", localModelDir)
	cmd.Env = append(os.Environ(), fmt.Sprintf("HF_ENDPOINT=%s", hfEndpoint))
	cmd.Stdout = logF
	cmd.Stderr = logF
	go func() {
		<-m.Ctx.Done()
		if cmd.Process != nil {
			if err = cmd.Process.Kill(); err != nil {
				zap.S().Warnf("终止子进程失败 (pid: %d): %v", cmd.Process.Pid, err)
			} else {
				zap.S().Infof("已通过 context 取消终止子进程 (pid: %d)", cmd.Process.Pid)
			}
		}
	}()
	if err := cmd.Run(); err != nil {
		zap.S().Errorf("下载失败（错误摘要）：%v", err)
		m.SchedulerDao.ExecUpdateRepositoryMountStatus(m.TaskNo, consts.StatusCacheJobBreak, err.Error())
	} else {
		m.SchedulerDao.ExecUpdateRepositoryMountStatus(m.TaskNo, consts.StatusCacheJobComplete, "")
	}
}
