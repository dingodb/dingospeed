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

	p.wg.Add(size)
	for i := 0; i < size; i++ {
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
