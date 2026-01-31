package usagerecord

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type cacheEntry struct {
	expiresAt time.Time
	value     any
}

type queryCache struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]cacheEntry

	sf singleflight.Group
}

func newQueryCache(ttl time.Duration) *queryCache {
	if ttl <= 0 {
		return nil
	}
	return &queryCache{
		ttl:     ttl,
		entries: make(map[string]cacheEntry),
	}
}

func (c *queryCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = make(map[string]cacheEntry)
	c.mu.Unlock()
}

func (c *queryCache) get(key string, fn func() (any, error)) (any, error) {
	if c == nil || c.ttl <= 0 {
		return fn()
	}

	now := time.Now()

	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && now.Before(entry.expiresAt) {
		value := entry.value
		c.mu.Unlock()
		return value, nil
	}
	c.mu.Unlock()

	value, err, _ := c.sf.Do(key, func() (any, error) {
		now := time.Now()
		c.mu.Lock()
		if entry, ok := c.entries[key]; ok && now.Before(entry.expiresAt) {
			value := entry.value
			c.mu.Unlock()
			return value, nil
		}
		c.mu.Unlock()

		v, err := fn()
		if err != nil {
			return nil, err
		}

		c.mu.Lock()
		c.entries[key] = cacheEntry{
			expiresAt: time.Now().Add(c.ttl),
			value:     v,
		}
		c.mu.Unlock()
		return v, nil
	})
	if err != nil {
		return nil, err
	}
	return value, nil
}
