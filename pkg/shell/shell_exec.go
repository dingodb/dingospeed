package util

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/labstack/gommon/log"
)

func CommandExec(commandName string, args ...string) (*exec.Cmd, error) {
	fmt.Printf("commandName %v,args %v\n", commandName, args)
	cmd := exec.Command(commandName, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Start()
	return cmd, err
}

func CommandWait(cmd *exec.Cmd) (int, error) {
	err := cmd.Wait()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			return exitError.ExitCode(), err
		} else {
			return -1, err
		}
	}
	return 0, nil
}

func ExecForWait(commandName string, args ...string) (int, error) {
	fmt.Printf("commandName %v,args %v\n", commandName, args)
	// 创建一个 exec.Command 对象
	cmd := exec.Command(commandName, args...)
	// 设置标准输出和标准错误
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// 启动命令并不阻塞
	err := cmd.Start()
	if err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	err = cmd.Wait()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			// 获取退出状态
			exitCode := exitError.ExitCode()
			// 根据退出状态做出不同的处理
			switch exitCode {
			case 0:
			case 1: // 进程号不存在
				return pid, nil
			default:
				log.Errorf("命令执行失败，未知错误代码：%d", exitCode)
			}
			return 0, err
		} else {
			// 其他错误类型
			log.Error("命令执行错误:", err)
		}
		return 0, err
	}
	return pid, nil
}

func ExecOutLogFile(commandName, logPath string, args ...string) (int, error) {
	// 打开或创建日志文件
	logFile, err := os.Create(logPath)
	if err != nil {
		fmt.Println("无法创建日志文件:", err)
		return 0, err
	}
	defer logFile.Close()

	// 创建一个 exec.Command 对象
	log.Info(fmt.Sprintf("当前执行命令为 %s %v", commandName, args))
	cmd := exec.Command(commandName, args...)

	// 将命令输出重定向到日志文件
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	err = cmd.Start()
	if err != nil {
		log.Errorf("启动命令失败:%v", err)
		return 0, err
	}
	pid := cmd.Process.Pid
	return pid, nil
}
