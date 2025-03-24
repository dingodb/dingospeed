//  Copyright (c) 2025 dingodb.com, Inc. All Rights Reserved
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http:www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package downloader

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"dingo-hfmirror/pkg/common"
	"dingo-hfmirror/pkg/consts"
	"dingo-hfmirror/pkg/util"
)

// Downloader 下载器
type Downloader struct {
	common.FileMetadata                     // 文件元数据
	waitGoroutine       sync.WaitGroup      // 同步goroutine
	DownloadDir         string              // 下载文件保存目录
	RetryChannel        chan common.Segment // 重传channel通道
	MaxGtChannel        chan struct{}       // 限制上传的goroutine的数量通道
	StartTime           int64               // 下载开始时间
	DownloadUrl         string              // 文件下载路径
}
type FileInfo struct {
	FileName string
	FileSize int64
}

func GetDownLoader(fileInfo FileInfo, downloadDir string) *Downloader {
	// 构建下载文件路径
	downloadingFile := getDownloadMetaFile(path.Join(downloadDir, fileInfo.FileName))
	dloader := &Downloader{
		DownloadDir:  downloadDir,
		RetryChannel: make(chan common.Segment, consts.DownloadRetryChannelNum),
		MaxGtChannel: make(chan struct{}, consts.DpGoroutineMaxNumPerFile),
		StartTime:    time.Now().Unix(),
	}
	// 检查下载文件是否存在，若存在表示是上次未下载完成的文件
	if util.IsFile(downloadingFile) {
		file, err := os.Open(downloadingFile)
		if err != nil {
			fmt.Println("获取文件状态失败")
			return nil
		}
		var metadata common.FileMetadata
		filedata := gob.NewDecoder(file)
		err = filedata.Decode(&metadata)
		if err != nil {
			fmt.Println("格式化文件数据失败")
		}
		dloader.FileMetadata = metadata
		// 计算还需下载的分片
		// sliceseq, err := dloader.calNeededSlice()
		// if err != nil {
		//	os.Remove(downloadingFile)
		//	return nil
		// }
		return dloader
	} else {
		// 没有downloading文件，重新下载
		return NewDownLoader(fileInfo, downloadDir)
	}
}

// NewDownLoader 新建一个下载器
func NewDownLoader(fileInfo FileInfo, downloadDir string) *Downloader {
	var metadata common.FileMetadata
	metadata.Fid = util.UUID()
	metadata.Filesize = fileInfo.FileSize
	metadata.Filename = fileInfo.FileName
	count, segments := util.SplitFileToSegment(fileInfo.FileSize, int64(consts.BlockSize))
	metadata.SliceNum = count
	metadata.Segments = segments

	// 创建下载分片保存路径文件夹
	dSliceDir := getSliceDir(path.Join(downloadDir, fileInfo.FileName), metadata.Fid)
	err := os.MkdirAll(dSliceDir, 0766)
	if err != nil {
		fmt.Println("创建下载分片目录失败", dSliceDir, err)
		return nil
	}

	metadataPath := getDownloadMetaFile(path.Join(downloadDir, fileInfo.FileName))
	err = util.StoreMetadata(metadataPath, &metadata)
	if err != nil {
		return nil
	}

	return &Downloader{
		DownloadDir:  downloadDir,
		FileMetadata: metadata,
		RetryChannel: make(chan common.Segment, consts.DownloadRetryChannelNum),
		MaxGtChannel: make(chan struct{}, consts.DpGoroutineMaxNumPerFile),
		StartTime:    time.Now().Unix(),
	}
}

// 获取上传元数据文件路径
func getDownloadMetaFile(filePath string) string {
	paths, fileName := filepath.Split(filePath)
	return path.Join(paths, "."+fileName+".downloading")
}

func getSliceDir(filePath string, fid string) string {
	paths, _ := filepath.Split(filePath)
	return path.Join(paths, fid)
}

// 计算还需下载的分片序号
func (d *Downloader) calNeededSlice() ([]*common.Segment, error) {
	// initialSegments := d.Segments
	//
	// // 获取已下载的文件片序号
	// storeSeq := make(map[string]bool)
	// files, _ := ioutil.ReadDir(getSliceDir(path.Join(d.DownloadDir, d.Filename), d.Fid))
	// for _, file := range files {
	//	_, err := strconv.Atoi(file.Name())
	//	if err != nil {
	//		fmt.Println("文件片有错", err, file.Name())
	//		continue
	//	}
	//	storeSeq[file.Name()] = true
	// }
	//
	// i := 0
	// for ; i < d.SliceNum && len(storeSeq) > 0; i++ {
	//	indexStr := strconv.Itoa(i)
	//	if _, ok := storeSeq[indexStr]; ok {
	//		delete(storeSeq, indexStr)
	//	} else {
	//		seq.Slices = append(seq.Slices, i)
	//	}
	// }
	//
	// // -1指代slices的最大数字序号到最后一片都没有收到
	// if i < d.SliceNum {
	//	seq.Slices = append(seq.Slices, i)
	//	i += 1
	//	if i < d.SliceNum {
	//		seq.Slices = append(seq.Slices, -1)
	//	}
	// }
	//
	// fmt.Printf("%s还需重新下载的片\n", d.Filename)
	// fmt.Println(seq.Slices)
	// return &seq, nil
	return nil, nil
}

// DownloadFile 单个文件的下载
func (d *Downloader) DownloadFile() error {
	if !util.IsDir(d.DownloadDir) {
		fmt.Printf("指定下载路径：%s 不存在\n", d.DownloadDir)
		return errors.New("指定下载路径不存在")
	}

	client := http.Client{}
	req, err := http.NewRequest(http.MethodGet, d.DownloadUrl, nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	filePath := path.Join(d.DownloadDir, d.Filename)
	f, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		fmt.Printf(err.Error())
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	if err != nil {
		return err
	}
	fmt.Printf("%s 文件下载成功，保存路径：%s\n", d.Filename, filePath)
	return nil
}

// DownloadFileBySlice 切片方式下载文件
func (d *Downloader) DownloadFileBySlice() error {
	// 启动重下载goroutine
	go d.retryDownloadSlice()
	metadata := &d.FileMetadata
	for _, segment := range metadata.Segments {
		tmp := segment
		d.waitGoroutine.Add(1)
		go d.downloadSlice(tmp)
	}

	// 等待各个分片都下载完成了
	fmt.Printf("%s等待分片下载完成\n", d.Filename)
	d.waitGoroutine.Wait()
	fmt.Printf("%s分片都已下载完成\n", d.Filename)
	return nil
}

// 重下载失败的分片
func (d *Downloader) retryDownloadSlice() {
	for segment := range d.RetryChannel {
		// 检查下载是否超时了
		if time.Now().Unix()-d.StartTime > consts.DownloadTimeout {
			fmt.Println("下载超时，请重试")
			d.waitGoroutine.Done()
		}

		fmt.Printf("重下载文件分片，文件名:%s, 分片序号:%d\n", d.Filename, segment)
		go d.downloadSlice(&segment)
	}
}

// 下载分片
func (d *Downloader) downloadSlice(segment *common.Segment) error {
	d.MaxGtChannel <- struct{}{}
	defer func() {
		<-d.MaxGtChannel
	}()
	client := http.Client{}
	req, err := http.NewRequest(http.MethodGet, d.DownloadUrl, nil)
	if err != nil {
		return err
	}
	req.Header.Add("range", fmt.Sprintf("bytes=%d-%d", segment.Start, segment.End))
	resp, err := client.Do(req)
	if err != nil {
		d.RetryChannel <- *segment
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		d.RetryChannel <- *segment
		errMsg, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return errors.New(string(errMsg))
	}

	filePath := path.Join(getSliceDir(path.Join(d.DownloadDir, d.Filename), d.Fid), strconv.Itoa(segment.Index))
	sliceFile, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		fmt.Println(err)
		d.RetryChannel <- *segment
		return err
	}

	_, err = io.Copy(sliceFile, resp.Body)
	if err != nil {
		fmt.Printf("文件%s的%d分片拷贝失败，失败原因:%s\n", d.Filename, segment.Index, err.Error())
		d.RetryChannel <- *segment
		return err
	}
	sliceFile.Close()
	d.waitGoroutine.Done()
	return nil
}

// MergeDownloadFiles 合并分片文件为一个文件
func (d *Downloader) MergeDownloadFiles() error {
	fmt.Println("开始合并文件", d.Filename)
	targetFile := path.Join(d.DownloadDir, d.Filename)
	realFile, err := os.OpenFile(targetFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		fmt.Println(err)
		return err
	}
	sliceDir := getSliceDir(path.Join(d.DownloadDir, d.Filename), d.Fid)
	// 计算md5值，这里要注意，一定要按分片顺序计算，不要使用读目录文件的方式，返回的文件顺序是无保证的
	// md5hash := md5.New()
	defer os.Remove(getDownloadMetaFile(targetFile))
	defer os.RemoveAll(sliceDir)
	for i := 0; i < d.SliceNum; i++ {
		sliceFilePath := path.Join(sliceDir, strconv.Itoa(i))
		sliceFile, err := os.Open(sliceFilePath)
		if err != nil {
			fmt.Printf("读取文件%s失败, err: %s\n", sliceFilePath, err)
			return err
		}
		// io.Copy(md5hash, sliceFile)

		// 偏移量需要重新进行调整
		sliceFile.Seek(0, 0)
		io.Copy(realFile, sliceFile)

		sliceFile.Close()
	}

	// 校验md5值
	// calMd5 := hex.EncodeToString(md5hash.Sum(nil))
	// if calMd5 != d.Md5sum {
	//	fmt.Printf("%s文件校验失败，请重新下载, 原始md5: %s, 计算的md5: %s\n", d.Filename, d.Md5sum, calMd5)
	//	return errors.New("文件校验失败")
	// }
	realFile.Close()
	fmt.Printf("%s文件下载成功，保存路径：%s\n", d.Filename, targetFile)
	return nil
}
