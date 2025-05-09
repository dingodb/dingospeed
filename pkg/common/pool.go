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

package common

import (
	"context"
	"errors"
	"sync"
)

// Task 定义任务类型
type Task interface {
	DoTask()
	OutResult()
	GetResponseChan() chan []byte
}

// Pool 协程池结构体
type Pool struct {
	taskChan chan Task
	wg       sync.WaitGroup
	size     int
}

// NewPool 创建新协程池
func NewPool(size int) *Pool {
	p := &Pool{
		taskChan: make(chan Task),
		size:     size,
	}

	for i := 0; i < size; i++ {
		p.wg.Add(1)
		go p.worker()
	}

	return p
}

// worker 工作协程
func (p *Pool) worker() {
	defer p.wg.Done()
	for {
		select {
		case task, ok := <-p.taskChan:
			if !ok {
				return
			}
			task.DoTask()
		}
	}
}

// Submit 提交任务
func (p *Pool) Submit(ctx context.Context, task Task) error {
	select {
	case p.taskChan <- task:
		return nil
	case <-ctx.Done():
		return errors.New("submit task fail")
	}
}

// Close 关闭协程池（安全关闭）
func (p *Pool) Close() {
	close(p.taskChan)
	p.wg.Wait()
}
