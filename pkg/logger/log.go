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

package log

import (
	"os"
	"path/filepath"
	"time"

	"dingo-hfmirror/pkg/config"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

func InitLogger() {
	logMode := zapcore.InfoLevel
	if config.SysConfig.Server.Mode == "debug" {
		logMode = zapcore.DebugLevel
	}
	core := zapcore.NewCore(getEncoder(), zapcore.NewMultiWriteSyncer(getWriteSyncer(), zapcore.AddSync(os.Stdout)), logMode)
	logger := zap.New(core, zap.AddCaller())
	zap.ReplaceGlobals(logger)
}

func getEncoder() zapcore.Encoder {
	encoder := zap.NewProductionEncoderConfig()
	encoder.TimeKey = "time"
	encoder.EncodeLevel = zapcore.CapitalLevelEncoder
	encoder.EncodeTime = func(t time.Time, encoder zapcore.PrimitiveArrayEncoder) {
		encoder.AppendString(t.Local().Format(time.DateTime))
	}
	encoder.CallerKey = "caller"
	encoder.EncodeCaller = zapcore.ShortCallerEncoder
	return zapcore.NewJSONEncoder(encoder)
}

func getWriteSyncer() zapcore.WriteSyncer {
	stSeparator := string(filepath.Separator)
	stRootDir, _ := os.Getwd()
	stLogFilePath := stRootDir + stSeparator + "log" + stSeparator + time.Now().Format(time.DateOnly) + ".log"

	l := &lumberjack.Logger{
		Filename:   stLogFilePath,
		MaxSize:    config.SysConfig.Log.MaxSize, // megabytes
		MaxBackups: config.SysConfig.Log.MaxBackups,
		MaxAge:     config.SysConfig.Log.MaxAge, // days
		Compress:   true,                        // disabled by default
	}
	return zapcore.AddSync(l)
}
