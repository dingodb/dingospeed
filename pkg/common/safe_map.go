package common

import "sync"

type SafeMap[K comparable, V any] struct {
	m  map[K]V
	mu sync.RWMutex
}

func NewSafeMap[K comparable, V any]() *SafeMap[K, V] {
	return &SafeMap[K, V]{
		m: make(map[K]V),
	}
}

func (sm *SafeMap[K, V]) Set(key K, value V) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m[key] = value
}

func (sm *SafeMap[K, V]) Get(key K) (V, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	value, exists := sm.m[key]
	return value, exists
}

func (sm *SafeMap[K, V]) Delete(key K) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.m, key)
}

func (sm *SafeMap[K, V]) Len() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.m)
}
