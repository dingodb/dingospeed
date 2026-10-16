package task

import (
	"bufio"
	"context"
	"fmt"
	"io"
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
	args := []string{"download", "--repo-type", repoType, upstream, "--local-dir", localModelDir}
	if token != "" {
		args = append(args, "--token", token)
	}
	newCmd := func() *exec.Cmd {
		cmd := exec.CommandContext(m.Ctx, "hf", args...)
		cmd.Env = append(os.Environ(), fmt.Sprintf("HF_ENDPOINT=%s", hfEndpoint))
		cmd.Stdout = logF
		cmd.Stderr = logF
		cmd.Cancel = func() error {
			zap.S().Infof("cancel process (%s, pid: %d)", orgRepo, cmd.Process.Pid)
			return cmd.Process.Kill()
		}
		return cmd
	}
	if err = runMountWithRetry(m.Ctx, orgRepo, newCmd, logF); err != nil {
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
		hfprojection.DefaultIndex.InvalidateRepository(config.SysConfig.Repos(), m.Job.Datatype, key.Repo)
		m.SchedulerDao.ExecUpdateRepositoryMountStatus(m.TaskNo, consts.RunningStatusJobComplete, "")
	}
}

// MountRetryBackoff spaces out reruns of a failed `hf download`; empty disables
// retries. A rerun skips files already present in the target directory and
// resumes partial ones, so one transient error (a DNS lookup, a dropped
// connection) no longer fails a copy that has been running for hours.
var MountRetryBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

func runMountWithRetry(ctx context.Context, orgRepo string, newCmd func() *exec.Cmd, logW io.Writer) error {
	for attempt := 0; ; attempt++ {
		err := newCmd().Run()
		if err == nil || ctx.Err() != nil || attempt >= len(MountRetryBackoff) {
			return err
		}
		zap.S().Warnf("mount %s failed, retry %d/%d in %s: %v",
			orgRepo, attempt+1, len(MountRetryBackoff), MountRetryBackoff[attempt], err)
		fmt.Fprintf(logW, "\n--- attempt %d failed: %v; retry in %s ---\n", attempt+1, err, MountRetryBackoff[attempt])
		select {
		case <-ctx.Done():
			return err
		case <-time.After(MountRetryBackoff[attempt]):
		}
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
