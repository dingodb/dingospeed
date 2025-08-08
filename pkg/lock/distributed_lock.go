package lock

import (
	"context"
	"time"

	"github.com/go-redsync/redsync/v4"
)

type DistributedLock struct {
	mutex         *redsync.Mutex
	stopRenewal   chan struct{} // 停止续期信号
	renewalFailed chan struct{} // 续期失败信号
	expiry        time.Duration // 锁过期时间 单位秒
}

func NewDistributedLock(rs *redsync.Redsync, name string, expiry time.Duration) *DistributedLock {
	mutex := rs.NewMutex(name, redsync.WithExpiry(time.Second*expiry))
	dl := &DistributedLock{
		mutex:         mutex,
		stopRenewal:   make(chan struct{}),
		renewalFailed: make(chan struct{}),
		expiry:        expiry,
	}
	return dl
}

func (dl *DistributedLock) Lock(ctx context.Context) error {
	// 尝试获取锁
	if err := dl.mutex.LockContext(ctx); err != nil {
		return err
	}

	// 启动自动续期
	go dl.renewLoop(ctx)
	return nil
}

// Unlock 释放分布式锁
func (dl *DistributedLock) Unlock(ctx context.Context) error {
	// 停止自动续期
	close(dl.stopRenewal)

	// 释放锁
	_, err := dl.mutex.UnlockContext(ctx)
	return err
}

func (dl *DistributedLock) exponentialBackoff(retry int) time.Duration {
	base := time.Second
	maxDelay := 5 * time.Second
	delay := base * (1 << retry)
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

// renewLoop 自动续期循环
func (dl *DistributedLock) renewLoop(ctx context.Context) {
	ticker := time.NewTicker(dl.expiry / 2)
	defer ticker.Stop()

	const maxRetries = 3
	retryCount := 0

	for {
		select {
		case <-ctx.Done(): // 上下文取消
			return
		case <-ticker.C: // 定时
			// 执行续期操作
			if _, err := dl.mutex.ExtendContext(ctx); err != nil {
				// 重试
				if retryCount++; retryCount >= maxRetries {
					dl.renewalFailed <- struct{}{}
					return
				}
				time.Sleep(dl.exponentialBackoff(retryCount))
			} else {
				retryCount = 0 // 重置计数器
			}
		case <-dl.stopRenewal: // 释放锁
			return
		}
	}
}
