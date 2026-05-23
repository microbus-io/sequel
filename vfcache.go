/*
Copyright (c) 2025-2026 Microbus LLC and various contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sequel

import (
	"container/list"
	"sync"
)

type vfCacheKey struct {
	driver string
	query  string
}

type vfCacheValue struct {
	expanded string
	err      error
}

type vfCacheEntry struct {
	key vfCacheKey
	val vfCacheValue
}

// vfCache is a small concurrency-safe LRU cache for expanded virtual-function queries.
type vfCache struct {
	mu       sync.Mutex
	capacity int
	items    map[vfCacheKey]*list.Element
	order    *list.List
}

func newVFCache(capacity int) *vfCache {
	if capacity < 0 {
		capacity = 0
	}
	return &vfCache{
		capacity: capacity,
		items:    make(map[vfCacheKey]*list.Element, capacity),
		order:    list.New(),
	}
}

func (c *vfCache) get(key vfCacheKey) (vfCacheValue, bool) {
	if c.capacity == 0 {
		return vfCacheValue{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*vfCacheEntry).val, true
	}
	return vfCacheValue{}, false
}

func (c *vfCache) put(key vfCacheKey, val vfCacheValue) {
	if c.capacity == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		el.Value.(*vfCacheEntry).val = val
		return
	}
	el := c.order.PushFront(&vfCacheEntry{key: key, val: val})
	c.items[key] = el
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*vfCacheEntry).key)
	}
}

func (c *vfCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[vfCacheKey]*list.Element, c.capacity)
	c.order.Init()
}

func (c *vfCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
