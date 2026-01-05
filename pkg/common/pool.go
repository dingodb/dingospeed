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
	"sync"
	"time"

	"dingospeed/pkg/consts"
	myerr "dingospeed/pkg/error"
)

// Task 定义任务类型
type Task interface {
	GetTaskNo() int
	DoTask()
	GetCancelFun() context.CancelFunc
}

type DownloadTask interface {
	Task
	OutResult()
	GetResponseChan() chan []byte
	SetTaskSize(taskSize int)
}

// Pool 协程池结构体
type Pool struct {
	taskChan chan Task
	taskMap  *SafeMap[int, Task]
	persist  bool
	size     int
	wg       sync.WaitGroup
	mu       sync.Mutex
}

// NewPool 创建新协程池
func NewPool(size int, persist bool) *Pool {
	p := &Pool{
		taskChan: make(chan Task),
		persist:  persist,
		size:     size,
	}
	if persist {
		p.taskMap = NewSafeMap[int, Task]()
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
			if p.persist {
				p.taskMap.Delete(task.GetTaskNo())
			}
		}
	}
}

func (p *Pool) exist(taskNo int) bool {
	return p.taskMap.Exist(taskNo)
}

func (p *Pool) GetTask(taskNo int) (Task, bool) {
	return p.taskMap.Get(taskNo)
}

// Submit 提交任务
func (p *Pool) Submit(ctx context.Context, task Task) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.persist && p.exist(task.GetTaskNo()) {
		return nil
	}
	select {
	case p.taskChan <- task:
		if p.persist {
			p.taskMap.Set(task.GetTaskNo(), task)
		}
		return nil
	case <-ctx.Done():
		return nil
	}
}

func (p *Pool) SubmitForTimeout(ctx context.Context, task Task) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.persist && p.exist(task.GetTaskNo()) {
		return nil
	}
	select {
	case p.taskChan <- task:
		if p.persist {
			p.taskMap.Set(task.GetTaskNo(), task)
		}
		return nil
	case <-time.After(3 * time.Second):
		return myerr.New(consts.TaskMoreErrMsg)
	case <-ctx.Done():
		return nil
	}
}

// Close 关闭协程池（安全关闭）
func (p *Pool) Close() {
	close(p.taskChan)
	p.wg.Wait()
	if p.persist {
		p.taskMap.DeleteAll()
	}
}
