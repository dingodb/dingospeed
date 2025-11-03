package util

import (
	"fmt"
	"testing"
)

func TestExecForWait(t *testing.T) {
	pid, err := ExecForWait("/Users/zhaoli/projects/code/zetyun/go/dingospeed/config/shell/hf-cache.sh", "model", "Qwen/Qwen3-0.6B", "/Users/zhaoli/Downloads", "http://localhost:8091")
	if err != nil {
		fmt.Errorf("err:%v", err)
		return
	}
	fmt.Println(pid)
}

func TestExecOutLogFile(t *testing.T) {
	pid, err := ExecOutLogFile("/Users/zhaoli/projects/code/zetyun/go/dingospeed/config/shell/hf-cache.sh", "./log", "model", "Qwen/Qwen3-0.6B", "/Users/zhaoli/Downloads", "http://localhost:8091")
	if err != nil {
		fmt.Errorf("err:%v", err)
		return
	}
	fmt.Println(pid)
}
