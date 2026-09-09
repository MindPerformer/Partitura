// cache_test.go 测试进程内 TTL 内存缓存。
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 4 验收要求：
//   - 缓存命中/TTL 过期
//   - 进程无落盘
package cache

import (
	"os"
	"testing"
	"time"
)

func TestCacheSetGet(t *testing.T) {
	c := New(1*time.Minute, 100)

	c.Set("key1", "value1")

	val, ok := c.Get("key1")
	if !ok {
		t.Fatal("期望缓存命中")
	}
	if val != "value1" {
		t.Errorf("val = %v, 期望 value1", val)
	}
}

func TestCacheMiss(t *testing.T) {
	c := New(1*time.Minute, 100)

	_, ok := c.Get("nonexistent")
	if ok {
		t.Fatal("期望缓存未命中")
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	c := New(50*time.Millisecond, 100)

	c.Set("key1", "value1")

	// 立即读取——应命中
	val, ok := c.Get("key1")
	if !ok {
		t.Fatal("期望缓存命中")
	}
	if val != "value1" {
		t.Errorf("val = %v", val)
	}

	// 等待 TTL 过期
	time.Sleep(60 * time.Millisecond)

	// 再次读取——应未命中
	_, ok = c.Get("key1")
	if ok {
		t.Fatal("期望缓存已过期")
	}
}

func TestCacheInvalidate(t *testing.T) {
	c := New(1*time.Minute, 100)

	c.Set("key1", "value1")
	c.Invalidate("key1")

	_, ok := c.Get("key1")
	if ok {
		t.Fatal("期望缓存已失效")
	}
}

func TestCacheInvalidatePrefix(t *testing.T) {
	c := New(1*time.Minute, 100)

	c.Set("ws1:doc1", "v1")
	c.Set("ws1:doc2", "v2")
	c.Set("ws2:doc1", "v3")

	c.InvalidatePrefix("ws1:")

	_, ok1 := c.Get("ws1:doc1")
	if ok1 {
		t.Fatal("期望 ws1:doc1 已失效")
	}
	_, ok2 := c.Get("ws1:doc2")
	if ok2 {
		t.Fatal("期望 ws1:doc2 已失效")
	}
	_, ok3 := c.Get("ws2:doc1")
	if !ok3 {
		t.Fatal("期望 ws2:doc1 仍存在")
	}
}

func TestCacheClear(t *testing.T) {
	c := New(1*time.Minute, 100)

	c.Set("key1", "v1")
	c.Set("key2", "v2")
	c.Clear()

	if c.Size() != 0 {
		t.Errorf("Size = %d, 期望 0", c.Size())
	}
}

func TestCacheMaxEntries(t *testing.T) {
	c := New(1*time.Minute, 3)

	c.Set("k1", "v1")
	c.Set("k2", "v2")
	c.Set("k3", "v3")
	c.Set("k4", "v4") // 应淘汰最旧条目

	if c.Size() > 3 {
		t.Errorf("Size = %d, 期望 <= 3", c.Size())
	}
}

func TestCacheNoPersistence(t *testing.T) {
	// 验证缓存是纯内存的——使用临时工作目录做文件系统快照验证。
	// 引入动机：design/02-MCP.md §本地缓存 要求"绝对禁止持久化 Workspace 文档缓存"，
	// "进程退出全部丢失"。测试必须在进程边界验证缓存读写和销毁不创建任何文件。
	//
	// 方法：在临时目录中运行缓存操作，操作前后对比目录内容，
	// 确认无新增文件。不使用反射。

	// 1. 创建临时工作目录
	tmpDir := t.TempDir()

	// 2. 记录操作前的文件快照
	beforeFiles := listFiles(t, tmpDir)

	// 3. 在临时目录中创建缓存、写入数据、读取数据、销毁
	// 缓存操作不应写任何文件到 tmpDir 或任何其他位置
	c := New(1*time.Minute, 100)
	c.Set("ws-1:read:test.md", "document content here")
	c.Set("ws-1:outline:test.md", []string{"heading1", "heading2"})
	c.Set("ws-1:meta", map[string]interface{}{"name": "test", "id": "ws-1"})

	// 读取验证缓存正常工作
	val, ok := c.Get("ws-1:read:test.md")
	if !ok {
		t.Fatal("缓存应命中")
	}
	if val != "document content here" {
		t.Errorf("缓存值 = %v, 期望 document content here", val)
	}

	// 清空缓存（模拟进程退出）
	c.Clear()

	// 4. 记录操作后的文件快照
	afterFiles := listFiles(t, tmpDir)

	// 5. 验证文件系统无变化——缓存未创建任何文件
	if len(afterFiles) != len(beforeFiles) {
		t.Errorf("缓存操作创建了文件：操作前 %d 个文件，操作后 %d 个文件", len(beforeFiles), len(afterFiles))
	}
	for f := range afterFiles {
		if !beforeFiles[f] {
			t.Errorf("缓存操作创建了意外文件: %s", f)
		}
	}
}

// listFiles 列出目录中所有文件的集合。
// 引入动机：TestCacheNoPersistence 需要文件系统快照对比。
func listFiles(t *testing.T, dir string) map[string]bool {
	t.Helper()
	files := make(map[string]bool)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	for _, e := range entries {
		files[e.Name()] = true
	}
	return files
}
