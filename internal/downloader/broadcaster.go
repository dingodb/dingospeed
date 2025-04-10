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

package downloader

import (
	"context"
	"fmt"
	"sync"
)

type Broadcaster struct {
	once      sync.Once
	ctx       context.Context
	msgChan   chan bool
	listeners []chan bool
	mu        sync.RWMutex
	cancel    context.CancelFunc
}

// 每个下载请求需要注册广播实例

func NewBroadcaster(ctx context.Context) *Broadcaster {
	ctx, cancel := context.WithCancel(ctx)
	b := &Broadcaster{
		msgChan:   make(chan bool, 1),
		listeners: make([]chan bool, 0),
		ctx:       ctx,
		cancel:    cancel,
	}
	return b
}

func (b *Broadcaster) AddListener() chan bool {
	b.once.Do(func() {
		go func() {
			for {
				select {
				case state, ok := <-b.msgChan:
					if !ok {
						return
					}
					b.mu.RLock()
					// 复制监听器列表以避免并发修改问题
					listenersCopy := make([]chan bool, len(b.listeners))
					copy(listenersCopy, b.listeners)
					b.mu.RUnlock()
					for _, listener := range listenersCopy {
						select {
						case listener <- state:
						case <-b.ctx.Done():
							return
						}
					}
				case <-b.ctx.Done():
					return
				}
			}
		}()
	})
	listener := make(chan bool)
	b.mu.Lock()
	b.listeners = append(b.listeners, listener)
	b.mu.Unlock()
	return listener
}

func (b *Broadcaster) SendMsg(msg bool) {
	b.mu.RLock()
	if len(b.listeners) == 0 {
		return
	}
	b.mu.RUnlock()
	select {
	case b.msgChan <- msg:
	case <-b.ctx.Done():
	}
}

func (b *Broadcaster) Close() {
	close(b.msgChan)
	b.cancel()
	b.mu.RLock()
	for _, listener := range b.listeners {
		close(listener)
	}
	b.mu.RUnlock()
}

func worker(ctx context.Context, listener chan bool, id int) {
	for {
		select {
		case newState := <-listener:
			fmt.Printf("Worker %d: Received new state %v\n", id, newState)
		// default:
		//	fmt.Printf("Worker %d: Waiting for state change...\n", id)
		//	time.Sleep(500 * time.Millisecond)
		case <-ctx.Done():
			return
		}
	}
}

//
// func main() {
// 	// ctx, cancel := context.WithCancel(context.Background())
// 	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
//
// 	b := NewBroadcaster(ctx)
// 	var wg sync.WaitGroup
//
// 	for i := 0; i < 3; i++ {
// 		wg.Add(1)
// 		listener := b.addListener()
// 		go func(i int) {
// 			defer wg.Done()
// 			worker(ctx, listener, i)
// 		}(i)
// 	}
// 	time.Sleep(1 * time.Second)
// 	b.sendMsg(false)
// 	b.sendMsg(true)
// 	time.Sleep(1 * time.Second)
// 	fmt.Println("执行取消")
// 	cancel()
// 	time.Sleep(2 * time.Second)
// 	wg.Wait()
// }
