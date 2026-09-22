package task

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"dingospeed/internal/dao"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/hfprojection"
	"dingospeed/pkg/repository"

	"go.uber.org/zap"
)

type MountCacheTask struct {
	CacheTask
	Authorization string
}

func (m *MountCacheTask) DoTask() {
	orgRepo := fmt.Sprintf("%s/%s", m.Job.Namespace, m.Job.Repo)
	key := repository.RepoKey{Namespace: m.Job.Namespace, RepoType: m.Job.Datatype, Repo: m.Job.Repo}
	if key.Namespace != repository.HuggingFace {
		revision := "main"
		if key.Namespace == repository.ModelScope {
			revision = "master"
		}
		if err := m.mountViaRepositoryAPI(key, revision); err != nil {
			m.SchedulerDao.ExecUpdateRepositoryMountStatus(m.TaskNo, m.StopStatus(), err.Error())
		} else {
			m.SchedulerDao.ExecUpdateRepositoryMountStatus(m.TaskNo, consts.RunningStatusJobComplete, "")
		}
		return
	}
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
	localModelDir := filepath.Join(mountDir, m.Job.Datatype, filepath.FromSlash(key.Repo))

	logDir := filepath.Join(config.SysConfig.Server.Repos, "mount_download_logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		zap.S().Errorf("mkdir err: %v", err)
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
	hfEndpoint := fmt.Sprintf("http://%s:%d/huggingface", config.SysConfig.Server.Host, config.SysConfig.Server.Port)
	upstream, upErr := dao.UpstreamRepo(m.Job.Datatype, orgRepo)
	if upErr != nil {
		m.SchedulerDao.ExecUpdateRepositoryMountStatus(m.TaskNo, m.StopStatus(), upErr.Error())
		return
	}
	token := getToken(m.Authorization)
	var cmd *exec.Cmd
	if token != "" {
		cmd = exec.Command("hf", "download", "--repo-type",
			repoType, upstream, "--local-dir", localModelDir, "--token", token)
	} else {
		cmd = exec.Command("hf", "download", "--repo-type",
			repoType, upstream, "--local-dir", localModelDir)
	}
	cmd.Env = append(os.Environ(), fmt.Sprintf("HF_ENDPOINT=%s", hfEndpoint))
	cmd.Stdout = logF
	cmd.Stderr = logF
	go func() {
		<-m.Ctx.Done()
		if cmd.Process != nil {
			if err = cmd.Process.Kill(); err != nil {
				zap.S().Errorf("kill process fail (%s, pid: %d): %v", orgRepo, cmd.Process.Pid, err)
			} else {
				zap.S().Infof("cancel process (%s, pid: %d)", orgRepo, cmd.Process.Pid)
			}
		}
	}()
	if err = cmd.Run(); err != nil {
		zap.S().Infof("command fail.%d", m.TaskNo)
		errMsg := err.Error()
		lines, err := getLastNLines(logFile, 80)
		if err != nil {
			errMsg = err.Error()
		} else {
			if len(lines) > 0 {
				errMsg = strings.Join(lines, "\n")
			}
		}
		m.SchedulerDao.ExecUpdateRepositoryMountStatus(m.TaskNo, m.StopStatus(), errMsg)
	} else {
		zap.S().Infof("command success.%d", m.TaskNo)
		hfprojection.DefaultIndex.Invalidate(config.SysConfig.Repos(), m.Job.Datatype)
		m.SchedulerDao.ExecUpdateRepositoryMountStatus(m.TaskNo, consts.RunningStatusJobComplete, "")
	}
}

func getLastNLines(filePath string, n int) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open file failed, %s, %w", filePath, err)
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan file failed, %s, %w", filePath, err)
	}
	startIdx := len(lines) - n
	if startIdx < 0 {
		startIdx = 0 // 不足 N 行时取全部
	}
	return lines[startIdx:], nil
}

func getToken(authorization string) string {
	split := strings.Split(authorization, " ")
	if len(split) == 2 {
		return split[1]
	}
	return ""
}
