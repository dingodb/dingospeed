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
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// DecompressData 对压缩的数据进行解压缩
func DecompressData(rawData []byte, contentEncoding string) ([]byte, error) {
	fmt.Printf("decompress_data parameters:\n- content_encoding: %s\n", contentEncoding)
	if contentEncoding == "" {
		return rawData, nil
	}
	algorithms := strings.Split(contentEncoding, ",")
	finalData := rawData
	for _, algo := range algorithms {
		algo = strings.TrimSpace(strings.ToLower(algo))
		var err error
		switch algo {
		case "gzip":
			finalData, err = decompressGzip(finalData)
			if err != nil {
				return nil, fmt.Errorf("error decompressing gzip data: %w", err)
			}
		case "deflate":
			finalData, err = decompressDeflate(finalData)
			if err != nil {
				return nil, fmt.Errorf("error decompressing deflate data: %w", err)
			}
		case "br":
			finalData, err = decompressBrotli(finalData)
			if err != nil {
				return nil, fmt.Errorf("error decompressing Brotli data: %w", err)
			}
		case "zstd":
			finalData, err = decompressZstd(finalData)
			if err != nil {
				return nil, fmt.Errorf("error decompressing Zstandard data: %w", err)
			}
		case "compress":
			return nil, fmt.Errorf("unsupported decompression algorithm: %s", algo)
		default:
			return nil, fmt.Errorf("unsupported compression algorithm: %s", algo)
		}
	}
	return finalData, nil
}

// decompressGzip 解压缩 gzip 数据
func decompressGzip(data []byte) ([]byte, error) {
	buf := bytes.NewBuffer(data)
	gzr, err := gzip.NewReader(buf)
	if err != nil {
		return nil, err
	}
	defer gzr.Close()
	return io.ReadAll(gzr)
}

// decompressDeflate 解压缩 deflate 数据
func decompressDeflate(data []byte) ([]byte, error) {
	buf := bytes.NewBuffer(data)
	r, err := zlib.NewReader(buf)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// decompressBrotli 解压缩 Brotli 数据
func decompressBrotli(data []byte) ([]byte, error) {
	buf := bytes.NewBuffer(data)
	br := brotli.NewReader(buf)
	return io.ReadAll(br)
}

// decompressZstd 解压缩 Zstandard 数据
func decompressZstd(data []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	decompressed, err := decoder.DecodeAll(data, nil)
	if err != nil {
		return nil, err
	}
	return decompressed, nil
}
