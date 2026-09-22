package transfersettings

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"dingospeed/pkg/config"
	"dingospeed/pkg/transferlimit"
)

type Settings struct {
	Upload      int `json:"upload"`
	HuggingFace int `json:"huggingface"`
	ModelScope  int `json:"modelscope"`
	Peer        int `json:"peer"`
	Download    int `json:"download"`
}

var initial sync.Once
var value atomic.Pointer[Settings]
var writeMu sync.Mutex
var loadErr error
var gates [5]transferlimit.Gate

func AcquireDownload(ctx context.Context) (func(), error) {
	return gates[4].Acquire(ctx, func() int { return Current().Download })
}

func AcquireUpload(ctx context.Context) (func(), error) {
	return gates[0].Acquire(ctx, func() int { return Current().Upload })
}

func settingsPath() string { return filepath.Join(config.SysConfig.Repos(), ".transfer-settings.json") }
func Current() Settings {
	initial.Do(func() {
		s := Settings{Upload: max(1, min(config.SysConfig.Upload.ConcurrentLimit, config.SysConfig.GetUploadChunkConcurrentLimit())), HuggingFace: 8, ModelScope: 8, Peer: 8, Download: 8}
		b, err := os.ReadFile(settingsPath())
		if err == nil {
			err = json.Unmarshal(b, &s)
			if err == nil {
				err = Validate(s)
			}
		}
		if err != nil && !os.IsNotExist(err) {
			loadErr = err
			s = Settings{1, 1, 1, 1, 1}
		}
		value.Store(&s)
	})
	return *value.Load()
}
func Validate(s Settings) error {
	for _, n := range []int{s.Upload, s.HuggingFace, s.ModelScope, s.Peer, s.Download} {
		if n < 1 || n > 64 {
			return fmt.Errorf("HTTP concurrency must be between 1 and 64")
		}
	}
	if s.Upload > 8 {
		return fmt.Errorf("upload concurrency must be between 1 and 8 (chunk memory budget)")
	}
	return nil
}
func LoadError() error { Current(); return loadErr }
func Save(s Settings) error {
	if err := Validate(s); err != nil {
		return err
	}
	Current()
	writeMu.Lock()
	defer writeMu.Unlock()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(settingsPath()), ".transfer-settings-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, settingsPath()); err != nil {
		return err
	}
	value.Store(&s)
	for i := range gates {
		gates[i].Wake()
	}
	return nil
}
func Do(kind string, client *http.Client, req *http.Request) (*http.Response, error) {
	index := 0
	switch kind {
	case "huggingface":
		index = 1
	case "modelscope":
		index = 2
	case "peer":
		index = 3
	case "download":
		index = 4
	}
	return gates[index].Do(client, req, func() int {
		s := Current()
		return []int{s.Upload, s.HuggingFace, s.ModelScope, s.Peer, s.Download}[index]
	})
}
