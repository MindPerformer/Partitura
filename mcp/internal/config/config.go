// Package config 实现 MCP 客户端的 TOML 配置加载。
//
// 引入动机：design/02-MCP.md §MCP 配置 要求使用 TOML 配置文件保存 server URL、
// cache TTL 等非敏感配置。认证凭证（access token / refresh token）禁止出现在配置文件中，
// 必须存储在操作系统 credential store。
//
// 安全原则：
//   - 配置文件不得包含任何 token 或敏感凭据
//   - 配置文件路径和权限受控
//   - 解析失败时 fail-fast，不使用默认值静默运行
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Config 是 MCP 客户端的运行时配置。
// 引入动机：design/02-MCP.md §MCP 配置 示例定义了 server、cache_ttl、cache_max_entries、
// default_search_mode、search_result_limit 等配置项。
type Config struct {
	// Server 是 Knowledge Server 的 HTTPS base URL（如 "https://knowledge.company.com"）。
	// 引入动机：MCP client 需要知道 server 地址才能发起 REST 请求。
	Server string `toml:"server"`

	// CacheTTL 是进程内 memory cache 的 TTL（时间字符串，如 "30m"）。
	// 引入动机：design/02-MCP.md §本地缓存 要求默认 TTL=30 分钟，可配置。
	CacheTTL string `toml:"cache_ttl"`

	// CacheMaxEntries 是 memory cache 的最大条目数。
	// 引入动机：防止缓存无限增长。
	CacheMaxEntries int `toml:"cache_max_entries"`

	// DefaultSearchMode 是 knowledge_search 的默认搜索模式。
	// 引入动机：design/02-MCP.md §MCP 配置 示例包含 default_search_mode = "hybrid"。
	DefaultSearchMode string `toml:"default_search_mode"`

	// SearchResultLimit 是 knowledge_search 的默认结果数量上限。
	// 引入动机：design/02-MCP.md §MCP 配置 示例包含 search_result_limit = 10。
	SearchResultLimit int `toml:"search_result_limit"`

	// ActiveWorkspace 是上次选择的 workspace ID，用于进程重启后恢复状态。
	// 引入动机：提升用户体验，但不作为安全依据——每次工具调用仍验证 workspace 权限。
	ActiveWorkspace string `toml:"active_workspace"`

	// AllowInsecureTLS 允许跳过 TLS 证书验证。
	// 引入动机：仅用于测试环境（fake HTTPS server 使用自签证书）。
	// 生产环境必须为 false。design/02-MCP.md 要求 HTTPS server 通信。
	AllowInsecureTLS bool `toml:"allow_insecure_tls"`
}

// DefaultConfig 返回设计文档定义的默认配置。
// 引入动机：当配置文件缺失或某些字段未设置时，使用安全默认值。
func DefaultConfig() Config {
	return Config{
		CacheTTL:           "30m",
		CacheMaxEntries:    200,
		DefaultSearchMode:  "hybrid",
		SearchResultLimit:  10,
		AllowInsecureTLS:   false,
	}
}

// ParseCacheTTL 将配置中的 cache_ttl 字符串解析为 time.Duration。
// 引入动机：cache 模块需要 time.Duration 而非字符串。
func (c *Config) ParseCacheTTL() (time.Duration, error) {
	if c.CacheTTL == "" {
		return 30 * time.Minute, nil
	}
	d, err := time.ParseDuration(c.CacheTTL)
	if err != nil {
		return 0, fmt.Errorf("解析 cache_ttl: %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("cache_ttl 必须为正数")
	}
	return d, nil
}

// configPath 返回配置文件的标准路径。
// 引入动机：design/02-MCP.md 要求配置文件路径受控。
// 使用用户主目录下的 .knowledge-mcp/config.toml。
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录: %w", err)
	}
	return filepath.Join(home, ".knowledge-mcp", "config.toml"), nil
}

// Load 从默认路径加载配置文件。
// 引入动机：MCP 进程启动时需要加载配置。
//
// 行为：
//   - 配置文件不存在时返回 DefaultConfig（不报错，首次运行是正常情况）
//   - 配置文件存在但解析失败时 fail-fast
//   - 配置文件存在且解析成功时覆盖默认值
func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, fmt.Errorf("获取配置文件路径: %w", err)
	}

	return LoadFromPath(path)
}

// LoadFromPath 从指定路径加载配置文件。
// 引入动机：测试需要指定自定义配置文件路径。
func LoadFromPath(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 配置文件不存在——返回默认配置
			return &cfg, nil
		}
		return nil, fmt.Errorf("读取配置文件: %w", err)
	}

	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件: %w", err)
	}

	if cfg.Server == "" {
		return nil, fmt.Errorf("配置缺少 server URL")
	}

	return &cfg, nil
}

// Save 将配置写入默认路径。
// 引入动机：login 命令成功后需要保存 server URL 和 active workspace。
// 安全：只保存非敏感配置，不保存任何 token。
func Save(cfg *Config) error {
	dir, err := configPath()
	if err != nil {
		return err
	}
	return SaveToPath(cfg, dir)
}

// SaveToPath 将配置写入指定路径。
// 引入动机：测试需要指定自定义配置文件路径以验证序列化内容。
// 安全：只保存非敏感配置，不保存任何 token。
func SaveToPath(cfg *Config, path string) error {
	configDir := filepath.Dir(path)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("创建配置目录: %w", err)
	}

	// 序列化为 TOML
	var buf struct {
		Server            string `toml:"server"`
		CacheTTL          string `toml:"cache_ttl"`
		CacheMaxEntries   int    `toml:"cache_max_entries"`
		DefaultSearchMode string `toml:"default_search_mode"`
		SearchResultLimit int    `toml:"search_result_limit"`
		ActiveWorkspace   string `toml:"active_workspace"`
		AllowInsecureTLS  bool   `toml:"allow_insecure_tls"`
	}
	buf.Server = cfg.Server
	buf.CacheTTL = cfg.CacheTTL
	buf.CacheMaxEntries = cfg.CacheMaxEntries
	buf.DefaultSearchMode = cfg.DefaultSearchMode
	buf.SearchResultLimit = cfg.SearchResultLimit
	buf.ActiveWorkspace = cfg.ActiveWorkspace
	buf.AllowInsecureTLS = cfg.AllowInsecureTLS

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建配置文件: %w", err)
	}
	defer f.Close()

	encoder := toml.NewEncoder(f)
	if err := encoder.Encode(&buf); err != nil {
		return fmt.Errorf("写入配置文件: %w", err)
	}

	return nil
}
