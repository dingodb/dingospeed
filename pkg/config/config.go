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
	"fmt"
	"os"
	"time"

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
	HfScheme    string `json:"hfScheme" yaml:"hfScheme"`
	HfLfsNetLoc string `json:"hfLfsNetLoc" yaml:"hfLfsNetLoc"`
}

type Download struct {
	RetryChannelNum        int   `json:"retryChannelNum" yaml:"retryChannelNum"`
	GoroutineMaxNumPerFile int   `json:"goroutineMaxNumPerFile" yaml:"goroutineMaxNumPerFile"`
	WaitNextBlockTime      int   `json:"waitNextBlockTime" yaml:"waitNextBlockTime"`
	BlockSize              int64 `json:"blockSize" yaml:"blockSize"`
	ReqTimeout             int64 `json:"reqTimeout" yaml:"reqTimeout"`
	RespChunkSize          int64 `json:"respChunkSize" yaml:"respChunkSize"`
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
	SysConfig = &c // 设置全局配置变量
	return &c, nil
}
