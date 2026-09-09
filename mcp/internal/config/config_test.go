// config_test.go 测试 TOML 配置加载。
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 4 验收要求：
//   - 配置文件无 token
//   - 解析失败 fail-fast
//   - 缺失配置文件返回默认值
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.CacheTTL != "30m" {
		t.Errorf("CacheTTL = %s, 期望 30m", cfg.CacheTTL)
	}
	if cfg.CacheMaxEntries != 200 {
		t.Errorf("CacheMaxEntries = %d, 期望 200", cfg.CacheMaxEntries)
	}
	if cfg.DefaultSearchMode != "hybrid" {
		t.Errorf("DefaultSearchMode = %s, 期望 hybrid", cfg.DefaultSearchMode)
	}
	if cfg.SearchResultLimit != 10 {
		t.Errorf("SearchResultLimit = %d, 期望 10", cfg.SearchResultLimit)
	}
	if cfg.AllowInsecureTLS != false {
		t.Error("AllowInsecureTLS 应为 false")
	}
}

func TestParseCacheTTL(t *testing.T) {
	cfg := Config{CacheTTL: "15m"}
	d, err := cfg.ParseCacheTTL()
	if err != nil {
		t.Fatalf("ParseCacheTTL 失败: %v", err)
	}
	if d.Minutes() != 15 {
		t.Errorf("TTL = %v, 期望 15m", d)
	}
}

func TestParseCacheTTLEmpty(t *testing.T) {
	cfg := Config{CacheTTL: ""}
	d, err := cfg.ParseCacheTTL()
	if err != nil {
		t.Fatalf("ParseCacheTTL 失败: %v", err)
	}
	if d.Minutes() != 30 {
		t.Errorf("TTL = %v, 期望 30m（默认）", d)
	}
}

func TestParseCacheTTLInvalid(t *testing.T) {
	cfg := Config{CacheTTL: "invalid"}
	_, err := cfg.ParseCacheTTL()
	if err == nil {
		t.Fatal("期望解析失败")
	}
}

func TestParseCacheTTLZero(t *testing.T) {
	cfg := Config{CacheTTL: "0s"}
	_, err := cfg.ParseCacheTTL()
	if err == nil {
		t.Fatal("期望 0 TTL 失败")
	}
}

func TestLoadFromPathNonexistent(t *testing.T) {
	cfg, err := LoadFromPath("/nonexistent/path/config.toml")
	if err != nil {
		t.Fatalf("缺失文件不应报错: %v", err)
	}
	// 应返回默认配置
	if cfg.CacheTTL != "30m" {
		t.Errorf("CacheTTL = %s, 期望 30m", cfg.CacheTTL)
	}
}

func TestLoadFromPathValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `
server = "https://knowledge.example.com"
cache_ttl = "45m"
cache_max_entries = 500
default_search_mode = "semantic"
search_result_limit = 20
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

	cfg, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath 失败: %v", err)
	}
	if cfg.Server != "https://knowledge.example.com" {
		t.Errorf("Server = %s", cfg.Server)
	}
	if cfg.CacheTTL != "45m" {
		t.Errorf("CacheTTL = %s, 期望 45m", cfg.CacheTTL)
	}
	if cfg.CacheMaxEntries != 500 {
		t.Errorf("CacheMaxEntries = %d, 期望 500", cfg.CacheMaxEntries)
	}
	if cfg.DefaultSearchMode != "semantic" {
		t.Errorf("DefaultSearchMode = %s, 期望 semantic", cfg.DefaultSearchMode)
	}
	if cfg.SearchResultLimit != 20 {
		t.Errorf("SearchResultLimit = %d, 期望 20", cfg.SearchResultLimit)
	}
}

func TestLoadFromPathMissingServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `
cache_ttl = "30m"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

	_, err := LoadFromPath(path)
	if err == nil {
		t.Fatal("缺少 server URL 应 fail-fast")
	}
}

func TestLoadFromPathInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `invalid = = toml`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

	_, err := LoadFromPath(path)
	if err == nil {
		t.Fatal("无效 TOML 应 fail-fast")
	}
}

func TestConfigNoTokenFields(t *testing.T) {
	// 验证 Config 序列化和保存不包含任何 token 字段。
	// 引入动机：design 要求配置文件不得包含 token。
	// 使用 t.TempDir 真实保存并验证序列化内容无 token，
	// Save 错误不得静默吞掉。

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `
server = "https://knowledge.example.com"
cache_ttl = "30m"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}

	cfg, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath 失败: %v", err)
	}

	// 保存到同一路径——验证 SaveToPath 不报错
	if err := SaveToPath(cfg, path); err != nil {
		t.Fatalf("SaveToPath 失败（错误不得静默吞掉）: %v", err)
	}

	// 读取保存后的文件内容，验证不含 token
	loadedContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取保存的配置文件失败: %v", err)
	}
	loadedStr := string(loadedContent)
	if strings.Contains(loadedStr, "token") {
		t.Errorf("配置文件包含 token 字段: %s", loadedStr)
	}
	if strings.Contains(loadedStr, "access") {
		t.Errorf("配置文件包含 access 字段: %s", loadedStr)
	}
	if strings.Contains(loadedStr, "refresh") {
		t.Errorf("配置文件包含 refresh 字段: %s", loadedStr)
	}
	// 验证 server URL 被正确保存
	if !strings.Contains(loadedStr, "https://knowledge.example.com") {
		t.Errorf("配置文件应包含 server URL: %s", loadedStr)
	}
}
