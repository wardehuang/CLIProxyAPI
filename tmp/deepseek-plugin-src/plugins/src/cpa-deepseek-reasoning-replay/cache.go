package main

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

type reasoningCacheEntry struct {
	key       string
	content   string
	expiresAt time.Time
}

type reasoningCache struct {
	mu         sync.Mutex
	maxEntries int
	ttl        time.Duration
	items      map[string]*list.Element
	order      *list.List
}

var globalReasoningCache = newReasoningCache(4096, time.Hour)

func newReasoningCache(maxEntries int, ttl time.Duration) *reasoningCache {
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &reasoningCache{
		maxEntries: maxEntries,
		ttl:        ttl,
		items:      make(map[string]*list.Element),
		order:      list.New(),
	}
}

func resetReasoningCache(maxEntries int, ttl time.Duration) {
	globalReasoningCache = newReasoningCache(maxEntries, ttl)
	resetStreamAccumulators()
}

func cacheReasoningContent(key, content string) {
	key = strings.TrimSpace(key)
	content = strings.TrimSpace(content)
	if key == "" || content == "" {
		return
	}
	globalReasoningCache.put(key, content)
}

func lookupReasoningContent(key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	return globalReasoningCache.get(key)
}

func (c *reasoningCache) put(key, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if element, ok := c.items[key]; ok {
		entry := element.Value.(*reasoningCacheEntry)
		entry.content = content
		entry.expiresAt = now.Add(c.ttl)
		c.order.MoveToFront(element)
		return
	}
	c.evictExpiredLocked(now)
	for c.order.Len() >= c.maxEntries {
		c.evictOldestLocked()
	}
	entry := &reasoningCacheEntry{
		key:       key,
		content:   content,
		expiresAt: now.Add(c.ttl),
	}
	c.items[key] = c.order.PushFront(entry)
}

func (c *reasoningCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return "", false
	}
	entry := element.Value.(*reasoningCacheEntry)
	if time.Now().After(entry.expiresAt) {
		c.removeElementLocked(element)
		return "", false
	}
	c.order.MoveToFront(element)
	return entry.content, true
}

func (c *reasoningCache) evictExpiredLocked(now time.Time) {
	for element := c.order.Back(); element != nil; {
		previous := element.Prev()
		entry := element.Value.(*reasoningCacheEntry)
		if now.After(entry.expiresAt) {
			c.removeElementLocked(element)
		}
		element = previous
	}
}

func (c *reasoningCache) evictOldestLocked() {
	element := c.order.Back()
	if element == nil {
		return
	}
	c.removeElementLocked(element)
}

func (c *reasoningCache) removeElementLocked(element *list.Element) {
	entry := element.Value.(*reasoningCacheEntry)
	delete(c.items, entry.key)
	c.order.Remove(element)
}

func (c *reasoningCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
