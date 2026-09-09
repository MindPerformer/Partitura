// Package cache 实现进程内 TTL 内存缓存。
//
// 引入动机：design/02-MCP.md §本地缓存 要求：
//   - 绝对禁止持久化 Workspace 文档缓存
//   - 只允许进程内 memory cache
//   - TTL = 30 分钟（可配置）
//   - 进程退出全部丢失
//   - 可缓存 document content / revision / content hash / section outline / workspace metadata
//   - 缓存只用于性能和本地 patch
//   - 服务器永远是真相源
//   - 写请求必须携带 expected_revision + expected_hash
//   - 缓存不能替代并发控制
package cache

import (
	"sync"
	"time"
)

// entry 是缓存中的一个条目。
// 引入动机：需要存储值和过期时间。
type entry struct {
	value     interface{}
	expiresAt time.Time
}

// Cache 是进程内 TTL 内存缓存。
// 引入动机：MCP 工具读取文档时可以缓存结果减少 server 请求。
// 进程退出后缓存全部丢失，不持久化到文件系统。
type Cache struct {
	mu         sync.RWMutex
	entries    map[string]entry
	ttl        time.Duration
	maxEntries int
}

// New 创建新的内存缓存实例。
// 引入动机：MCP 进程启动时创建缓存，进程退出时缓存消失。
//
// 参数：
//   - ttl：缓存条目的存活时间
//   - maxEntries：最大条目数，超过时清理最旧条目
func New(ttl time.Duration, maxEntries int) *Cache {
	return &Cache{
		entries:    make(map[string]entry),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

// Get 从缓存中读取值。
// 引入动机：document read/outline/section 等工具先查缓存再请求 server。
// 如果缓存命中且未过期，返回值和 true；否则返回 nil 和 false。
func (c *Cache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok {
		return nil, false
	}

	if time.Now().After(e.expiresAt) {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return nil, false
	}

	return e.value, true
}

// Set 将值写入缓存。
// 引入动机：从 server 获取文档后缓存结果。
// 如果缓存条目数超过 maxEntries，清理最旧的条目。
func (c *Cache) Set(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 清理过期条目
	if len(c.entries) >= c.maxEntries {
		c.evictOldest()
	}

	c.entries[key] = entry{
		value:     value,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Invalidate 使指定 key 的缓存条目失效。
// 引入动机：文档写入成功后应清除该文档的缓存，确保下次读取获取最新内容。
func (c *Cache) Invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// InvalidatePrefix 使所有以指定前缀开头的缓存条目失效。
// 引入动机：workspace 切换时应清除前一个 workspace 的所有缓存。
func (c *Cache) InvalidatePrefix(prefix string) {
	c.mu.Lock()
	for k := range c.entries {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}

// Clear 清空所有缓存条目。
// 引入动机：logout 时清除全部缓存。
func (c *Cache) Clear() {
	c.mu.Lock()
	c.entries = make(map[string]entry)
	c.mu.Unlock()
}

// Size 返回当前缓存条目数。
// 引入动机：测试需要验证缓存条目数。
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// evictOldest 清理最旧的缓存条目。
// 引入动机：缓存达到 maxEntries 时需要淘汰策略。
func (c *Cache) evictOldest() {
	var oldestKey string
	var oldestExpiry time.Time
	first := true

	for k, e := range c.entries {
		if first {
			oldestKey = k
			oldestExpiry = e.expiresAt
			first = false
			continue
		}
		if e.expiresAt.Before(oldestExpiry) {
			oldestKey = k
			oldestExpiry = e.expiresAt
		}
	}

	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}
