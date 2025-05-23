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

package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"dingospeed/internal/model"
	myerr "dingospeed/pkg/error"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/gommon/log"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var SysConfig *Config
var SystemInfo *model.SystemInfo

type Config struct {
	Server           ServerConfig     `json:"server" yaml:"server"`
	Download         Download         `json:"download" yaml:"download"`
	Cache            Cache            `json:"cache" yaml:"cache"`
	Log              LogConfig        `json:"log" yaml:"log"`
	Retry            Retry            `json:"retry" yaml:"retry"`
	TokenBucketLimit TokenBucketLimit `json:"tokenBucketLimit" yaml:"tokenBucketLimit"`
	DiskClean        DiskClean        `json:"diskClean" yaml:"diskClean"`
}

type ServerConfig struct {
	Mode      string `json:"mode" yaml:"mode"`
	Host      string `json:"host" yaml:"host"`
	Port      int    `json:"port" yaml:"port"`
	PProf     bool   `json:"pprof" yaml:"pprof"`
	PProfPort int    `json:"pprofPort" yaml:"pprofPort"`
	Metrics   bool   `json:"metrics" yaml:"metrics"`
	Online    bool   `json:"online" yaml:"online"`
	Repos     string `json:"repos" yaml:"repos"`
	HfNetLoc  string `json:"hfNetLoc" yaml:"hfNetLoc"`
	HfScheme  string `json:"hfScheme" yaml:"hfScheme" validate:"oneof=https http"`
}

type Download struct {
	RetryChannelNum         int   `json:"retryChannelNum" yaml:"retryChannelNum"`
	GoroutineMaxNumPerFile  int   `json:"goroutineMaxNumPerFile" yaml:"goroutineMaxNumPerFile" validate:"min=1,max=8"`
	BlockSize               int64 `json:"blockSize" yaml:"blockSize" validate:"min=1048576,max=134217728"`
	ReqTimeout              int64 `json:"reqTimeout" yaml:"reqTimeout"`
	RespChunkSize           int64 `json:"respChunkSize" yaml:"respChunkSize" validate:"min=4096,max=8388608"`
	RespChanSize            int64 `json:"respChanSize" yaml:"respChanSize"`
	RemoteFileRangeSize     int64 `json:"remoteFileRangeSize" yaml:"remoteFileRangeSize" validate:"min=0,max=1073741824"`
	RemoteFileRangeWaitTime int64 `json:"remoteFileRangeWaitTime" yaml:"remoteFileRangeWaitTime" validate:"min=1,max=10"`
	RemoteFileBufferSize    int64 `json:"remoteFileBufferSize" yaml:"remoteFileBufferSize" validate:"min=0,max=134217728"`
}

type Cache struct {
	Enabled                     bool    `json:"enabled" yaml:"enabled"`
	Type                        int     `json:"type" yaml:"type"`
	CollectTimePeriod           int     `json:"collectTimePeriod" yaml:"collectTimePeriod" validate:"min=1,max=600"` // 周期采集内存使用量，单位秒
	PrefetchMemoryUsedThreshold float64 `json:"prefetchMemoryUsedThreshold" yaml:"prefetchMemoryUsedThreshold" validate:"min=50,max=99"`
	PrefetchBlocks              int64   `json:"prefetchBlocks" yaml:"prefetchBlocks" validate:"min=1,max=32"`      // 读取块数据，预先缓存的块数据数量
	PrefetchBlockTTL            int64   `json:"prefetchBlockTTL" yaml:"prefetchBlockTTL" validate:"min=1,max=120"` // 读取块数据，预先缓存的块数据数量
}

type Retry struct {
	Delay    int  `json:"delay" yaml:"delay" validate:"min=0,max=60"`
	Attempts uint `json:"attempts" yaml:"attempts" validate:"min=1,max=5"`
}

type LogConfig struct {
	MaxSize    int `json:"maxSize" yaml:"maxSize"`
	MaxBackups int `json:"maxBackups" yaml:"maxBackups"`
	MaxAge     int `json:"maxAge" yaml:"maxAge"`
}

type TokenBucketLimit struct {
	Capacity        int `json:"capacity" yaml:"capacity"`
	Rate            int `json:"rate" yaml:"rate"`
	HandlerCapacity int `json:"handlerCapacity" yaml:"handlerCapacity"`
}

type DiskClean struct {
	Enabled            bool   `json:"enabled" yaml:"enabled"`
	CacheSizeLimit     int64  `json:"cacheSizeLimit" yaml:"cacheSizeLimit"`
	CacheCleanStrategy string `json:"cacheCleanStrategy" yaml:"cacheCleanStrategy"`
	CollectTimePeriod  int    `json:"collectTimePeriod" yaml:"collectTimePeriod" validate:"min=1,max=600"` // 周期采集内存使用量，单位秒
}

func (c *Config) GetHFURLBase() string {
	return fmt.Sprintf("%s://%s", c.GetHfScheme(), c.GetHfNetLoc())
}

func (c *Config) Online() bool {
	return c.Server.Online
}

func (c *Config) Repos() string {
	return c.Server.Repos
}

func (c *Config) GetHost() string {
	return c.Server.Host
}

func (c *Config) GetHfNetLoc() string {
	return c.Server.HfNetLoc
}

func (c *Config) GetCapacity() int {
	return c.TokenBucketLimit.Capacity
}

func (c *Config) GetRate() int {
	return c.TokenBucketLimit.Rate
}

func (c *Config) GetHfScheme() string {
	return c.Server.HfScheme
}

func (c *Config) GetReqTimeOut() time.Duration {
	return time.Duration(c.Download.ReqTimeout) * time.Second
}

func (c *Config) GetCollectTimePeriod() time.Duration {
	return time.Duration(c.Cache.CollectTimePeriod) * time.Second
}

func (c *Config) GetPrefetchMemoryUsedThreshold() float64 {
	return c.Cache.PrefetchMemoryUsedThreshold
}

func (c *Config) GetRemoteFileRangeWaitTime() time.Duration {
	return time.Duration(c.Download.RemoteFileRangeWaitTime) * time.Millisecond
}

func (c *Config) GetPrefetchBlocks() int64 {
	return c.Cache.PrefetchBlocks
}

func (c *Config) GetPrefetchBlockTTL() time.Duration {
	return time.Duration(c.Cache.PrefetchBlockTTL) * time.Second
}

func (c *Config) GetDiskCollectTimePeriod() time.Duration {
	return time.Duration(c.DiskClean.CollectTimePeriod) * time.Hour
}

func (c *Config) EnableMetric() bool {
	return c.Server.Metrics
}

func (c *Config) CacheCleanStrategy() string {
	return c.DiskClean.CacheCleanStrategy
}

func (c *Config) SetDefaults() {
	if c.Server.Port == 0 {
		c.Server.Port = 8090
	}
	if c.Server.Host == "" {
		c.Server.Host = "localhost"
	}
	if c.Server.PProfPort == 0 {
		c.Server.PProfPort = 6060
	}
	if c.Download.GoroutineMaxNumPerFile == 0 {
		c.Download.GoroutineMaxNumPerFile = 8
	}
	if c.Download.BlockSize == 0 {
		c.Download.BlockSize = 8388608
	}
	if c.Download.RespChunkSize == 0 {
		c.Download.RespChunkSize = 8192
	}
	if c.Download.RemoteFileRangeWaitTime == 0 {
		c.Download.RemoteFileRangeWaitTime = 1
	}
	if c.Cache.PrefetchBlocks == 0 {
		c.Cache.PrefetchBlocks = 16
	}
	if c.Cache.PrefetchBlockTTL == 0 {
		c.Cache.PrefetchBlockTTL = 30
	}
	if c.Cache.CollectTimePeriod == 0 {
		c.Cache.CollectTimePeriod = 5
	}
	if c.Download.RespChanSize == 0 {
		c.Download.RespChanSize = 30
	}
	if c.DiskClean.CollectTimePeriod == 0 {
		c.DiskClean.CollectTimePeriod = 1
	}
}

func Scan(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var c Config
	err = yaml.Unmarshal(b, &c)
	if err != nil {
		return nil, err
	}
	c.SetDefaults()

	if c.Download.RemoteFileRangeSize%c.Download.BlockSize != 0 {
		return nil, myerr.New("RemoteFileRangeSize must be a multiple of BlockSize")
	}

	validate := validator.New()
	err = validate.Struct(&c)
	if err != nil {
		var invalidValidationError *validator.InvalidValidationError
		if errors.As(err, &invalidValidationError) {
			zap.S().Errorf("Invalid validation error: %v\n", err)
		}
		return nil, err
	}
	SysConfig = &c // 设置全局配置变量

	marshal, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	log.Info(string(marshal))
	SystemInfo = &model.SystemInfo{}
	return &c, nil
}
