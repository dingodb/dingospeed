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

package util

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"io"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	myerr "dingo-hfmirror/pkg/error"

	"github.com/google/uuid"
)

func Max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func Min(a, b int) int {
	if a > b {
		return b
	}
	return a
}

func Itoa(a interface{}) string {
	switch at := a.(type) {
	case int, int8, int16, int64, int32:
		return strconv.FormatInt(reflect.ValueOf(a).Int(), 10)
	case uint, uint8, uint16, uint32, uint64:
		return strconv.FormatInt(int64(reflect.ValueOf(a).Uint()), 10)
	case float32, float64:
		return strconv.FormatFloat(reflect.ValueOf(a).Float(), 'f', -1, 64)
	case string:
		return at
	}
	return ""
}

func Atoi(a string) int {
	if a == "" {
		return 0
	}
	r, e := strconv.Atoi(a)
	if e == nil {
		return r
	}
	return 0
}

func Atoi64(a string) int64 {
	b, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0
	}
	return b
}

// 转Int
func AnyToInt(value interface{}) int {
	if value == nil {
		return 0
	}
	switch val := value.(type) {
	case int:
		return val
	case int8:
		return int(val)
	case int16:
		return int(val)
	case int32:
		return int(val)
	case int64:
		return int(val)
	case uint:
		return int(val)
	case uint8:
		return int(val)
	case uint16:
		return int(val)
	case uint32:
		return int(val)
	case uint64:
		return int(val)
	case *string:
		v, err := strconv.Atoi(*val)
		if err != nil {
			return 0
		}
		return v
	case string:
		v, err := strconv.Atoi(val)
		if err != nil {
			return 0
		}
		return v
	case float32:
		return int(val)
	case float64:
		return int(val)
	case bool:
		if val {
			return 1
		} else {
			return 0
		}
	case json.Number:
		v, _ := val.Int64()
		return int(v)
	}

	return 0
}

func HashCode(s string) uint32 {
	return crc32.ChecksumIEEE([]byte(s))
}

// BindJSONWithDisallowUnknownFields 验证传入的请求是否合理
func BindJSONWithDisallowUnknownFields(req *http.Request, obj interface{}) error {
	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(obj)
	if err != nil {
		return err
	}
	return nil
}

func TimeToInt64(time time.Time) int64 {
	if time.Unix() > 0 {
		return time.Unix()
	}
	return 0
}

// 秒级时间戳转time
func UnixSecondToTime(second int64) time.Time {
	return time.Unix(second, 0)
}

// 毫秒级时间戳转time
func UnixMilliToTime(milli int64) time.Time {
	return time.Unix(milli/1000, (milli%1000)*(1000*1000))
}

// 纳秒级时间戳转time
func UnixNanoToTime(nano int64) time.Time {
	return time.Unix(nano/(1000*1000*1000), nano%(1000*1000*1000))
}

// 转义百分号
func EscapePercent(s string) string {
	return strings.ReplaceAll(s, "%", "\\%")
}

func StringSliceToInt64Slice(s []string) []int {
	r := make([]int, 0, len(s))
	for _, v := range s {
		r = append(r, Atoi(v))
	}
	return r
}

func SetValWhenFloatIsNaNOrInf(val float64) float64 {
	if math.IsNaN(val) {
		return 0.00
	}
	if math.IsInf(val, 0) {
		return 100.00
	}
	return val
}

func ReadText(reader io.ReaderAt, i int64) string {
	buffer := make([]byte, i)
	n, _ := reader.ReadAt(buffer, i)
	return string(buffer[:n])
}

func CalculatePercentage(a, b float32) (float32, error) {
	if b == 0 {
		return 0, myerr.New("division by zero is not allowed")
	}
	percentage := (a / b) * 100
	roundedPercentage := float32(int(percentage*100+0.5)) / 100
	return roundedPercentage, nil
}

func UUID() string {
	return uuid.New().String()
}

func Md5(str string) string {
	hash := md5.Sum([]byte(str))
	hashString := hex.EncodeToString(hash[:])
	return hashString
}
