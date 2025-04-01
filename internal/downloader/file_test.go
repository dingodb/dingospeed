package downloader

import (
	"fmt"
	"testing"

	"go.uber.org/zap"
)

func TestFileWrite(t *testing.T) {
	var dingFile *DingCache
	var err error
	savePath := "cachefile"
	fileSize := int64(8388608)
	blockSize := int64(8388608)
	if dingFile, err = NewDingCache(savePath, blockSize); err != nil {
		zap.S().Errorf("NewDingCache err.%v", err)
		return
	}
	if err = dingFile.Resize(fileSize); err != nil {
		zap.S().Errorf("Resize err.%v", err)
		return
	}
}

func TestFileWrite2(t *testing.T) {

	h := NewDingCacheHeader(1, 1, 1)
	fmt.Println(string(h.MagicNumber[:]))
	fmt.Println(string(h.MagicNumber[:]))

	fmt.Println(string(h.MagicNumber[:]))

}
