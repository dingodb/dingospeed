package main

import (
	"testing"

	"dingospeed/pkg/common"
)

func TestGetPathInfoOid(t *testing.T) {
	remoteReqFilePathMap := make(map[string]*common.PathsInfo, 0)
	remoteReqFilePathMap["README.md"] = nil
}

// func TestGetPathInfoFile(t *testing.T) {
// 	rootDir := "/Users/zhaoli/projects/code/zetyun/go/dingospeed/repos/api/models/Qwen/Qwen3-32B/paths-info/d47b0d4ae4b48fde975756bf360a63a9cca8d470" // 当前目录，可替换为其他路径
// 	dirPaths, err := util.TraverseDir(rootDir, rootDir)
//
// 	if err != nil {
// 		fmt.Printf("遍历目录时出错: %v\n", err)
// 		return
// 	}
//
// 	// 输出所有目录路径
// 	for _, path := range dirPaths {
// 		fmt.Println(path)
// 	}
// }
