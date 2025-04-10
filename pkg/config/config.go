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

	myerr "dingo-hfmirror/pkg/error"

	"github.com/go-playground/validator/v10"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var SysConfig *Config

type Config struct {
	Server   ServerConfig `json:"server" yaml:"server"`
	Download Download     `json:"download" yaml:"download"`
	Log      LogConfig    `json:"log" yaml:"log"`
}

type ServerConfig struct {
	Host        string `json:"host" yaml:"host"`
	Mode        string `json:"mode" yaml:"mode"`
	Port        int    `json:"port" yaml:"port"`
	Online      bool   `json:"online" yaml:"online"`
	Repos       string `json:"repos" yaml:"repos"`
	HfNetLoc    string `json:"hfNetLoc" yaml:"hfNetLoc"`
	HfScheme    string `json:"hfScheme" yaml:"hfScheme" validate:"oneof=https http"`
	HfLfsNetLoc string `json:"hfLfsNetLoc" yaml:"hfLfsNetLoc"`
}

type Download struct {
	RetryChannelNum         int   `json:"retryChannelNum" yaml:"retryChannelNum"`
	GoroutineMaxNumPerFile  int   `json:"goroutineMaxNumPerFile" yaml:"goroutineMaxNumPerFile" validate:"min=1,max=8"`
	BlockSize               int64 `json:"blockSize" yaml:"blockSize" validate:"min=1048576,max=134217728"`
	ReqTimeout              int64 `json:"reqTimeout" yaml:"reqTimeout"`
	RespChunkSize           int64 `json:"respChunkSize" yaml:"respChunkSize" validate:"min=4096,max=8388608"`
	RemoteFileRangeSize     int64 `json:"remoteFileRangeSize" yaml:"remoteFileRangeSize" validate:"min=0,max=1073741824"`
	RemoteFileRangeWaitTime int64 `json:"remoteFileRangeWaitTime" yaml:"remoteFileRangeWaitTime" validate:"min=1,max=10"`
	RemoteFileBufferSize    int64 `json:"remoteFileBufferSize" yaml:"remoteFileBufferSize" validate:"min=0,max=134217728"`
	PrefetchBlocks          int64 `json:"prefetchBlocks" yaml:"prefetchBlocks" validate:"min=8,max=32"` // 读取块数据，预先缓存的块数据数量

}

type LogConfig struct {
	MaxSize    int `json:"maxSize" yaml:"maxSize"`
	MaxBackups int `json:"maxBackups" yaml:"maxBackups"`
	MaxAge     int `json:"maxAge" yaml:"maxAge"`
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

func (c *Config) GetHfScheme() string {
	return c.Server.HfScheme
}

func (c *Config) GetHfLfsNetLoc() string {
	return c.Server.HfLfsNetLoc
}

func (c *Config) GetReqTimeOut() time.Duration {
	return time.Duration(c.Download.ReqTimeout) * time.Second
}

func (c *Config) GetRemoteFileRangeWaitTime() time.Duration {
	return time.Duration(c.Download.RemoteFileRangeWaitTime) * time.Second
}

func (c *Config) SetDefaults() {
	if c.Server.Port == 0 {
		c.Server.Port = 8090
	}
	if c.Server.Host == "" {
		c.Server.Host = "localhost"
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
	if c.Download.PrefetchBlocks == 0 {
		c.Download.PrefetchBlocks = 16
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
	return &c, nil
}
