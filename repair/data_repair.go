package main

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"dingospeed/pkg/common"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/util"

	"github.com/bytedance/sonic"
	"github.com/labstack/gommon/log"
	"go.uber.org/zap"
)

var (
	repoPathParam string
	repoTypeParam string
	orgParam      string
	repoParam     string
)

type CommitHfSha struct {
	Sha      string `json:"sha"`
	Siblings []struct {
		Rfilename string `json:"rfilename"`
	} `json:"siblings"`
}

func init() {
	flag.StringVar(&repoPathParam, "repoPath", "./repos", "仓库路径")
	flag.StringVar(&repoTypeParam, "repoType", "models", "类型")
	flag.StringVar(&orgParam, "org", "", "组织")
	flag.StringVar(&repoParam, "repo", "", "仓库")
	flag.Parse()
}

func main() {
	fmt.Println("starting data repair....")
	if repoPathParam == "" || repoTypeParam == "" {
		log.Errorf("repoPath,repoType不能为空")
		return
	}
	if orgParam != "" && repoParam != "" {
		repoRepair(repoPathParam, repoTypeParam, orgParam, repoParam)
	} else {
		if orgParam != "" && repoParam == "" {
			orgRepair(repoPathParam, repoTypeParam, orgParam)
		} else if orgParam == "" && repoParam == "" {
			typePath := fmt.Sprintf("%s/api/%s", repoPathParam, repoTypeParam)
			// 读取目录内容
			orgEntries, err := os.ReadDir(typePath)
			if err != nil {
				fmt.Printf("读取目录失败: %v\n", err)
				return
			}
			for _, entry := range orgEntries {
				if entry.IsDir() {
					orgRepair(repoPathParam, repoTypeParam, entry.Name())
				}
			}
		}
	}
	headsPath := fmt.Sprintf("%s/heads", repoPathParam)
	if exist := util.FileExists(headsPath); exist {
		err := os.RemoveAll(headsPath)
		if err != nil {
			fmt.Printf("删除目录失败: %v\n", err)
			return
		}
	}
}

func orgRepair(repoPath, repoType, org string) {
	orgPath := fmt.Sprintf("%s/api/%s/%s", repoPath, repoType, org)
	// 读取目录内容
	repoEntries, err := os.ReadDir(orgPath)
	if err != nil {
		fmt.Printf("读取目录失败: %v\n", err)
		return
	}
	for _, entry := range repoEntries {
		if entry.IsDir() {
			repoRepair(repoPath, repoType, org, entry.Name())
		}
	}
}

func repoRepair(repoPath, repoType, org, repo string) {
	if repo == "" {
		panic("repo is null")
	}
	filePath := fmt.Sprintf("%s/files/%s/%s/%s", repoPath, repoType, org, repo)
	if exist := util.FileExists(filePath); !exist {
		return
	}
	fileBlobs := fmt.Sprintf("%s/blobs", filePath)
	if exist := util.FileExists(fileBlobs); exist {
		// log.Errorf(fmt.Sprintf("该仓库已完成修复：%s", fileBlobs))
		// return
	}
	metaGetPath := fmt.Sprintf("%s/api/%s/%s/%s/revision/main/meta_get.json", repoPath, repoType, org, repo)
	if exist := util.FileExists(metaGetPath); !exist {
		// log.Errorf(fmt.Sprintf("该%s/%s不存在meta_get文件，无法修复.", org, repo))
		// return
	}
	log.Infof("start repair：%s/%s/%s", repoType, org, repo)
	cacheContent, err := ReadCacheRequest(metaGetPath)
	if err != nil {
		return
	}
	var sha CommitHfSha
	if err = sonic.Unmarshal(cacheContent.OriginContent, &sha); err != nil {
		zap.S().Errorf("unmarshal error.%v", err)
		return
	}
	remoteReqFilePathMap := make(map[string]*common.PathsInfo, 0)
	for _, item := range sha.Siblings {
		remoteReqFilePathMap[item.Rfilename] = nil
	}
	getPathInfoOid(remoteReqFilePathMap, repoType, fmt.Sprintf("%s/%s", org, repo), sha.Sha)
	for _, item := range sha.Siblings {
		fileName := item.Rfilename
		pathInfo, ok := remoteReqFilePathMap[fileName]
		if !ok {
			continue
		}
		if err = updatePathInfo(repoPath, repoType, org, repo, sha.Sha, fileName, pathInfo); err != nil {
			continue
		}
		var etag string
		if pathInfo.Lfs.Oid != "" {
			etag = pathInfo.Lfs.Oid
		} else {
			etag = pathInfo.Oid
		}
		filePath = fmt.Sprintf("%s/files/%s/%s/%s/resolve/%s/%s", repoPath, repoType, org, repo, sha.Sha, fileName)
		if exist := util.FileExists(filePath); !exist {
			log.Warnf(fmt.Sprintf("该文件%s不存在.", filePath))
			continue
		}
		newBlobsFilePath := fmt.Sprintf("%s/files/%s/%s/%s/blobs/%s", repoPath, repoType, org, repo, etag)
		util.ReName(filePath, newBlobsFilePath)
		err = util.CreateSymlinkIfNotExists(newBlobsFilePath, filePath)
		if err != nil {
			log.Errorf("CreateSymlinkIfNotExists err.%v", err)
			continue
		}
	}
	// 删除其他版本
	resolvePath := fmt.Sprintf("%s/files/%s/%s/%s/resolve", repoPath, repoType, org, repo)
	entries, err := os.ReadDir(resolvePath)
	if err != nil {
		fmt.Printf("读取目录失败: %v\n", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if entry.Name() != sha.Sha {
				err := os.RemoveAll(fmt.Sprintf("%s/%s", resolvePath, entry.Name()))
				if err != nil {
					fmt.Printf("删除目录失败: %v\n", err)
					continue
				}
			}
		}
	}
	log.Infof("end repair：%s/%s/%s", repoType, org, repo)
}

func updatePathInfo(repoPath, repoType, org, repo, commit, fileName string, pathInfo *common.PathsInfo) error {
	pathInfoPath := fmt.Sprintf("%s/api/%s/%s/%s/paths-info/%s/%s/paths-info_post.json", repoPath, repoType, org, repo, commit, fileName)
	if exist := util.FileExists(pathInfoPath); !exist {
		return myerr.New("file is not exist")
	}
	cacheContent, err := ReadCacheRequest(pathInfoPath)
	if err != nil {
		log.Errorf(fmt.Sprintf("read file:%s err", pathInfoPath))
		return err
	}
	pathsInfos := make([]*common.PathsInfo, 0)
	pathsInfos = append(pathsInfos, pathInfo)
	b, err := sonic.Marshal(pathsInfos)
	if err != nil {
		zap.S().Errorf("pathsInfo Unmarshal err.%v", err)
		return err
	}
	if err = WriteCacheRequest(pathInfoPath, cacheContent.StatusCode, cacheContent.Headers, b); err != nil {
		zap.S().Errorf("WriteCacheRequest err.%s,%v", pathInfoPath, err)
		return err
	}
	return nil
}

func WriteCacheRequest(apiPath string, statusCode int, headers map[string]string, content []byte) error {
	cacheContent := common.CacheContent{
		StatusCode: statusCode,
		Headers:    headers,
		Content:    hex.EncodeToString(content),
	}
	return util.WriteDataToFile(apiPath, cacheContent)
}

func ReadCacheRequest(apiPath string) (*common.CacheContent, error) {
	cacheContent := common.CacheContent{}
	bytes, err := util.ReadFileToBytes(apiPath)
	if err != nil {
		return nil, myerr.Wrap("ReadFileToBytes err.", err)
	}
	if err = sonic.Unmarshal(bytes, &cacheContent); err != nil {
		return nil, err
	}
	decodeByte, err := hex.DecodeString(cacheContent.Content)
	if err != nil {
		return nil, myerr.Wrap("DecodeString err.", err)
	}
	cacheContent.OriginContent = decodeByte
	return &cacheContent, nil
}

func getPathInfoOid(remoteReqFilePathMap map[string]*common.PathsInfo, repoType, orgRepo, commit string) {
	filePaths := make([]string, 0)
	for k := range remoteReqFilePathMap {
		filePaths = append(filePaths, k)
	}
	pathsInfoUrl := fmt.Sprintf("%s/api/%s/%s/paths-info/%s", "https://hf-mirror.com", repoType, orgRepo, commit)
	response, err := pathsInfoProxy(pathsInfoUrl, "", filePaths)
	if err != nil {
		zap.S().Errorf("req %s err.%v", pathsInfoUrl, err)
		return
	}
	if response.StatusCode != http.StatusOK {
		zap.S().Errorf("response.StatusCode err:%d", response.StatusCode)
		return
	}
	remoteRespPathsInfos := make([]common.PathsInfo, 0)
	err = sonic.Unmarshal(response.Body, &remoteRespPathsInfos)
	if err != nil {
		zap.S().Errorf("req %s remoteRespPathsInfos Unmarshal err.%v", pathsInfoUrl, err)
		return
	}
	for _, item := range remoteRespPathsInfos {
		// 对单个文件pathsInfo做存储
		if _, ok := remoteReqFilePathMap[item.Path]; ok {
			remoteReqFilePathMap[item.Path] = &item
		}
	}
}

func pathsInfoProxy(targetUrl, authorization string, filePaths []string) (*common.Response, error) {
	data := map[string]interface{}{
		"paths": filePaths,
	}
	jsonData, err := sonic.Marshal(data)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	return Post(targetUrl, "application/json", jsonData, headers)
}

// Post 方法用于发送带请求头的 POST 请求
func Post(url string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	respHeaders := make(map[string]interface{})
	for key, value := range resp.Header {
		respHeaders[key] = value
	}
	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       body,
	}, nil
}
