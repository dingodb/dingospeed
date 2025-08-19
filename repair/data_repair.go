package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"dingospeed/pkg/common"
	"dingospeed/pkg/consts"
	myerr "dingospeed/pkg/error"
	"dingospeed/pkg/util"

	"github.com/avast/retry-go"
	"github.com/bytedance/sonic"
	"github.com/labstack/gommon/log"
	"go.uber.org/zap"
)

var (
	repoPathParam string
	repoTypeParam string
	orgParam      string
	repoParam     string
	token         string
	hfUrl         string
	batchSize     int

	type_meta     = "meta"
	type_pathInfo = "pathInfo"
)

type CommitHfSha struct {
	Sha      string `json:"sha"`
	Siblings []struct {
		Rfilename string `json:"rfilename"`
	} `json:"siblings"`
}

func main() {
	flag.StringVar(&repoPathParam, "repoPath", "./repos", "仓库路径")
	flag.StringVar(&repoTypeParam, "repoType", "models", "类型")
	flag.StringVar(&orgParam, "org", "", "组织")
	flag.StringVar(&repoParam, "repo", "", "仓库")
	flag.StringVar(&token, "token", "", "token") //
	flag.StringVar(&hfUrl, "hfUrl", "hf-mirror.com", "hfUrl")
	flag.IntVar(&batchSize, "batchSize", 100, "batchSize")
	flag.Parse()
	if repoPathParam == "" || repoTypeParam == "" {
		log.Errorf("repoPath,repoType不能为空")
		return
	}
	if exist := util.FileExists(repoPathParam); !exist {
		log.Errorf("repoPath:%s目录不存在", repoPathParam)
		return
	}
	if repoTypeParam != "models" && repoTypeParam != "datasets" {
		log.Errorf("repoType的可选值为models或datasets。")
		return
	}
	typePath := fmt.Sprintf("%s/api/%s", repoPathParam, repoTypeParam)
	if exist := util.FileExists(typePath); !exist {
		log.Errorf("不存在类型为%s的缓存数据", repoTypeParam)
		return
	}

	if orgParam != "" && repoParam != "" {
		repoRepair(repoPathParam, repoTypeParam, orgParam, repoParam)
	} else {
		if orgParam != "" && repoParam == "" {
			orgRepair(repoPathParam, repoTypeParam, orgParam)
		} else if orgParam == "" && repoParam == "" {
			// 读取目录内容
			orgEntries, err := os.ReadDir(typePath)
			if err != nil {
				log.Warnf("读取目录失败: %v\n", err)
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
			log.Warnf("删除目录失败: %v\n", err)
			return
		}
	}
}

func orgRepair(repoPath, repoType, org string) {
	orgPath := fmt.Sprintf("%s/api/%s/%s", repoPath, repoType, org)
	// 读取目录内容
	repoEntries, err := os.ReadDir(orgPath)
	if err != nil {
		log.Warnf("读取目录失败: %v\n", err)
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
		log.Warnf("不存在org:%s, repo:%s的缓存数据", org, repo)
		return
	}
	metaGetPath := fmt.Sprintf("%s/api/%s/%s/%s/revision/main/meta_get.json", repoPath, repoType, org, repo)
	if exist := util.FileExists(metaGetPath); exist {
		cacheContent, err := ReadCacheRequest(metaGetPath)
		if err != nil {
			return
		}
		var commitHfSha CommitHfSha
		if err = sonic.Unmarshal(cacheContent.OriginContent, &commitHfSha); err != nil {
			log.Warnf("unmarshal commitHfSha %s/%s error.%v", org, repo, err)
			// metaget解析失败，采用迭代pathsInfo处理
			iteratePathsInfoDir(repoPath, repoType, org, repo)
			return
		}
		fileOrderMap := make(map[string]interface{}, 0)
		for _, item := range commitHfSha.Siblings {
			fileOrderMap[item.Rfilename] = nil
		}
		// meta_get读取正确，获取最新的main sha值，得到其pathsInfo
		if err = iteratePathsInfoDirSha(fileOrderMap, repoPath, repoType, org, repo, commitHfSha.Sha, type_meta); err != nil {
			log.Errorf("iteratePathsInfoDirSha error.%v", err)
			return
		}
	} else {
		iteratePathsInfoDir(repoPath, repoType, org, repo)
	}
}

func iteratePathsInfoDir(repoPath, repoType, org, repo string) {
	pathsInfoDir := fmt.Sprintf("%s/api/%s/%s/%s/paths-info", repoPath, repoType, org, repo)
	// pathsInfo文件不存在，不予处理
	if b := util.FileExists(pathsInfoDir); !b {
		log.Warnf("pathsInfoDir is not exitst.%s", pathsInfoDir)
		return
	}
	if shas, err := util.ReadDir(pathsInfoDir); err != nil {
		log.Warnf("ReadDir %s/%s , %s error.%v", org, repo, pathsInfoDir, err)
		return
	} else {
		for _, item := range shas {
			metaResp, err := remoteRequestMeta(repoType, org, repo, item, token)
			if err != nil {
				continue
			}
			var sha CommitHfSha
			if err = sonic.Unmarshal(metaResp.Body, &sha); err != nil {
				zap.S().Errorf("unmarshal content:%s, error:%v", string(metaResp.Body), err)
				continue
			}
			fileOrderMap := make(map[string]interface{}, 0)
			for _, sibling := range sha.Siblings {
				fileOrderMap[sibling.Rfilename] = nil
			}
			if err = iteratePathsInfoDirSha(fileOrderMap, repoPath, repoType, org, repo, item, type_pathInfo); err != nil {
				log.Errorf("iteratePathsInfoDirSha error.%v", err)
				continue
			}
		}
	}
}

func iteratePathsInfoDirSha(fileOrderMap map[string]interface{}, repoPath, repoType, org, repo, sha, strategyPathInfo string) error {
	pathsInfoDir := fmt.Sprintf("%s/api/%s/%s/%s/paths-info", repoPath, repoType, org, repo)
	shaDir := fmt.Sprintf("%s/%s", pathsInfoDir, sha)
	// pathsInfo文件不存在，不予处理
	if b := util.FileExists(shaDir); !b {
		log.Infof("shaDir is not exist.%s", shaDir)
		return nil
	}
	if fileNames, err := util.TraverseDir(shaDir, shaDir); err != nil {
		log.Errorf("ReadDir error.%v", err)
		return err
	} else {
		newFileNames := make([]string, 0)
		for _, fileName := range fileNames {
			if _, ok := fileOrderMap[fileName]; ok {
				newFileNames = append(newFileNames, fileName)
			}
		}
		if len(newFileNames) == 0 {
			log.Infof("there are no files to repair.%s", shaDir)
			return nil
		}
		err = repoRepairProcess(repoPath, repoType, org, repo, sha, newFileNames, strategyPathInfo)
		if err != nil {
			log.Errorf("repoRepairProcess error.%v", err)
			return err
		}
	}
	return nil
}

func repoRepairProcess(repoPath, repoType, org, repo, sha string, fileNames []string, strategy string) error {
	fileBlobs := fmt.Sprintf("%s/files/%s/%s/%s/blobs", repoPath, repoType, org, repo)
	blobExist := util.FileExists(fileBlobs)
	shaPath := fmt.Sprintf("%s/files/%s/%s/%s/resolve/%s", repoPath, repoType, org, repo, sha)
	if shaPathExist := util.FileExists(shaPath); !shaPathExist {
		log.Warnf("shaPath file not exist.%s", shaPath)
		return nil
	}
	repairLockPath := fmt.Sprintf("%s/%s", shaPath, "lock")
	lockExist := util.FileExists(repairLockPath)
	if strategy == type_meta {
		if blobExist && !lockExist {
			log.Infof(fmt.Sprintf("该仓库已完成修复：%s", fileBlobs))
			return nil
		}
	}
	log.Infof("start repair：%s/%s/%s/%s", repoType, org, repo, sha)
	if err := util.CreateFile(repairLockPath); err != nil {
		return err
	}
	blobsDir := fmt.Sprintf("%s/files/%s/%s/%s/blobs/", repoPath, repoType, org, repo)
	if err := util.MakeDirs(blobsDir); err != nil {
		log.Errorf("MakeDirs err.%v", err)
		return err
	}
	var (
		i              = 0
		pieceFileNames = make([]string, 0)
	)
	for _, k := range fileNames {
		pieceFileNames = append(pieceFileNames, k)
		i++
		if i%batchSize == 0 {
			if err := repoRepairProcessPiece(pieceFileNames, repoPath, repoType, org, repo, sha); err != nil {
				var t myerr.Error
				if errors.As(err, &t) {
					if t.StatusCode() == http.StatusNotFound {
						// 若查询模型官网已不存在，则删除该模型数据
						deleteOrgRepo(repoPath, repoType, org, repo)
						return nil
					}
				}
				return err
			}
			i = i / batchSize
			pieceFileNames = make([]string, 0)
		}
	}
	if i > 0 {
		if err := repoRepairProcessPiece(pieceFileNames, repoPath, repoType, org, repo, sha); err != nil {
			var t myerr.Error
			if errors.As(err, &t) {
				if t.StatusCode() == http.StatusNotFound {
					// 若查询模型官网已不存在，则删除该模型数据
					deleteOrgRepo(repoPath, repoType, org, repo)
					return nil
				}
			}
			return err
		}
	}
	if strategy == type_meta {
		// 删除其他版本
		deleteOtherVersion(repoPath, repoType, org, repo, sha)
	}
	if err := util.DeleteFile(repairLockPath); err != nil {
		return err
	}
	log.Infof("end repair：%s/%s/%s/%s", repoType, org, repo, sha)
	return nil
}

func repoRepairProcessPiece(fileNames []string, repoPath, repoType, org, repo, sha string) error {
	todoRepairFileNames := make([]string, 0)
	// 先判断哪些文件已被修复
	for _, fileName := range fileNames {
		filePath := fmt.Sprintf("%s/files/%s/%s/%s/resolve/%s/%s", repoPath, repoType, org, repo, sha, fileName)
		if exist := util.FileExists(filePath); exist {
			if b, err := util.IsSymlink(filePath); err != nil {
				todoRepairFileNames = append(todoRepairFileNames, fileName)
			} else {
				if !b {
					todoRepairFileNames = append(todoRepairFileNames, fileName)
				}
			}
		} else {
			// 文件不存在，也需要更新pathsInfo
			todoRepairFileNames = append(todoRepairFileNames, fileName)
		}
	}
	if len(todoRepairFileNames) == 0 {
		return nil
	}
	remoteReqFilePathMap, err := batchGetPathInfo(todoRepairFileNames, repoType, fmt.Sprintf("%s/%s", org, repo), sha)
	if err != nil {
		return err
	}
	blobsDir := fmt.Sprintf("%s/files/%s/%s/%s/blobs/", repoPath, repoType, org, repo)
	for _, fileName := range todoRepairFileNames {
		pathInfo := remoteReqFilePathMap[fileName]
		if pathInfo == nil {
			log.Warnf("pathInfo is nil, %s/%s, sha:%s, fileName:%s", org, repo, sha, fileName)
			continue
		}
		if err = updatePathInfo(repoPath, repoType, org, repo, sha, fileName, pathInfo); err != nil {
			continue
		}
		filePath := fmt.Sprintf("%s/files/%s/%s/%s/resolve/%s/%s", repoPath, repoType, org, repo, sha, fileName)
		if exist := util.FileExists(filePath); exist {
			var etag string
			if pathInfo.Lfs.Oid != "" {
				etag = pathInfo.Lfs.Oid
			} else {
				etag = pathInfo.Oid
			}
			newBlobsFilePath := fmt.Sprintf("%s/%s", blobsDir, etag)
			util.ReName(filePath, newBlobsFilePath)
			err = util.CreateSymlinkIfNotExists(newBlobsFilePath, filePath)
			if err != nil {
				log.Errorf("CreateSymlinkIfNotExists err.%v", err)
				continue
			}
		}
	}
	return nil
}

func deleteOrgRepo(repoPath, repoType, org, repo string) {
	repoFilesPath := fmt.Sprintf("%s/files/%s/%s/%s", repoPath, repoType, org, repo)
	repoApiPath := fmt.Sprintf("%s/api/%s/%s/%s", repoPath, repoType, org, repo)
	err := os.RemoveAll(repoFilesPath)
	if err != nil {
		log.Errorf("删除files目录%s失败: %v", repoFilesPath, err)
	}
	err = os.RemoveAll(repoApiPath)
	if err != nil {
		log.Errorf("删除api目录%s失败: %v", repoApiPath, err)
	}
	log.Infof("delete repo data %s/%s", org, repo)
}

func deleteOtherVersion(repoPath, repoType, org, repo, sha string) {
	resolvePath := fmt.Sprintf("%s/files/%s/%s/%s/resolve", repoPath, repoType, org, repo)
	revisionPath := fmt.Sprintf("%s/api/%s/%s/%s/revision", repoPath, repoType, org, repo)
	pathInfoPath := fmt.Sprintf("%s/api/%s/%s/%s/paths-info", repoPath, repoType, org, repo)
	entries, err := os.ReadDir(resolvePath)
	if err != nil {
		log.Errorf("读取目录失败: %v", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if entry.Name() != sha {
				rpp := fmt.Sprintf("%s/%s", resolvePath, entry.Name())
				err = os.RemoveAll(rpp)
				if err != nil {
					log.Errorf("删除resolve目录失败: %v", err)
				}
				revisionP := fmt.Sprintf("%s/%s", revisionPath, entry.Name())
				err = os.RemoveAll(revisionP)
				if err != nil {
					log.Errorf("删除revision目录失败: %v", err)
				}
				ppth := fmt.Sprintf("%s/%s", pathInfoPath, entry.Name())
				err = os.RemoveAll(ppth)
				if err != nil {
					log.Errorf("删除pathinfo目录失败: %v", err)
				}
			}
		}
	}
}

func updatePathInfo(repoPath, repoType, org, repo, commit, fileName string, pathInfo *common.PathsInfo) error {
	pathInfoPath := fmt.Sprintf("%s/api/%s/%s/%s/paths-info/%s/%s/paths-info_post.json", repoPath, repoType, org, repo, commit, fileName)
	if exist := util.FileExists(pathInfoPath); exist {
		cacheContent, err := ReadCacheRequest(pathInfoPath)
		if err != nil {
			log.Errorf(fmt.Sprintf("read file:%s err", pathInfoPath))
			return err
		}
		pathsInfos := make([]*common.PathsInfo, 0)
		pathsInfos = append(pathsInfos, pathInfo)
		b, err := sonic.Marshal(pathsInfos)
		if err != nil {
			log.Errorf("pathsInfo Unmarshal err.%v", err)
			return err
		}
		if err = WriteCacheRequest(pathInfoPath, cacheContent.StatusCode, cacheContent.Headers, b); err != nil {
			log.Errorf("WriteCacheRequest err.%s,%v", pathInfoPath, err)
			return err
		}
	}
	return nil
}

func WriteCacheRequest(apiPath string, statusCode int, headers map[string]string, content []byte) error {
	cacheContent := common.CacheContent{
		Version:    consts.VersionSnapshot,
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

func batchGetPathInfo(filePaths []string, repoType, orgRepo, commit string) (map[string]*common.PathsInfo, error) {
	pathsInfoUrl := fmt.Sprintf("%s/api/%s/%s/paths-info/%s", getDomainUrl(), repoType, orgRepo, commit)
	response, err := pathsInfoProxy(pathsInfoUrl, token, filePaths)
	if err != nil {
		log.Errorf("req %s err.%v", pathsInfoUrl, err)
		return nil, myerr.Wrap("batchGetPathInfo err", err)
	}
	if response.StatusCode != http.StatusOK {
		log.Errorf("response orgRepo:%s, StatusCode err:%d", orgRepo, response.StatusCode)
		return nil, myerr.NewAppendCode(response.StatusCode, "request fail")
	}
	remoteRespPathsInfos := make([]common.PathsInfo, 0)
	err = sonic.Unmarshal(response.Body, &remoteRespPathsInfos)
	if err != nil {
		log.Errorf("req %s remoteRespPathsInfos Unmarshal err.%v", pathsInfoUrl, err)
		return nil, myerr.Wrap("Unmarshal err", err)
	}
	remoteReqFilePathMap := make(map[string]*common.PathsInfo, 0)
	for _, item := range remoteRespPathsInfos {
		localPathInfo := item
		remoteReqFilePathMap[localPathInfo.Path] = &localPathInfo
	}
	return remoteReqFilePathMap, nil
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
		headers["authorization"] = fmt.Sprintf("Bearer %s", authorization)
	}
	return RetryRequest(func() (*common.Response, error) {
		return Post(targetUrl, "application/json", jsonData, headers)
	})
}

func remoteRequestMeta(repoType, org, repo, commit, authorization string) (*common.Response, error) {
	orgRepo := util.GetOrgRepo(org, repo)
	var reqUrl string
	if commit == "" {
		reqUrl = fmt.Sprintf("%s/api/%s/%s", getDomainUrl(), repoType, orgRepo)
	} else {
		reqUrl = fmt.Sprintf("%s/api/%s/%s/revision/%s", getDomainUrl(), repoType, orgRepo, commit)
	}
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = fmt.Sprintf("Bearer %s", authorization)
	}
	return RetryRequest(func() (*common.Response, error) {
		return util.Get(reqUrl, headers)
	})
}

func RetryRequest(f func() (*common.Response, error)) (*common.Response, error) {
	var resp *common.Response
	err := retry.Do(
		func() error {
			var err error
			resp, err = f()
			return err
		},
		retry.Delay(1*time.Second),
		retry.Attempts(5),
		retry.DelayType(retry.FixedDelay),
	)
	return resp, err
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

func getDomainUrl() string {
	return fmt.Sprintf("https://%s", hfUrl)
}
