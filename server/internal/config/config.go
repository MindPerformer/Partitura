// Package config 负责从环境变量加载服务运行所需的全部配置。
//
// 引入动机：Phase 1 需要一个统一的配置入口，供 DB 连接、日志、服务监听等模块复用。
// 设计原则：配置缺失或非法时必须返回带上下文的错误，不得静默给出不安全默认值。
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是服务运行的全部配置集合。
// 每个字段对应一个环境变量，由 Load 从环境变量解析填充。
type Config struct {
	// PostgreSQL 数据库连接配置
	DBHost     string
	DBPort     int
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string

	// 服务基础运行配置
	HTTPAddr        string
	LogLevel        string
	ShutdownTimeout time.Duration
	// PublicOrigin 是对外服务的可信 public origin，例如 https://app.example.com。
	// 引入动机：device authorization 的 verification_url 只能使用明确配置的可信来源，
	// 禁止从请求 Host 推导，避免 Host header 注入。
	PublicOrigin string

	// 认证模块配置
	// 引入动机：auth 模块需要 session/token 有效期、cookie 属性、Argon2id 参数等配置。
	// 这些配置通过环境变量加载，未设置时使用安全默认值。
	SessionDuration           int64  // 秒，默认 86400（24h）
	DeviceAccessTokenDuration int64  // 秒，默认 900（15min）
	DeviceRefreshTokenDuration int64 // 秒，默认 2592000（30d）
	CookieSecure              bool   // 默认 true，开发环境可设 false
	CookieDomain              string // 默认空
	CookiePath                string // 默认 "/"
	CookieName                string // 默认 "session"
	CSRFCookieName            string // 默认 "csrf"
	CSRFHeaderName            string // 默认 "X-CSRF-Token"
	Argon2Memory              uint32 // KiB，默认 65536（64MiB）
	Argon2Iterations          uint32 // 默认 3
	Argon2Parallelism         uint8  // 默认 2
	Argon2SaltLength          uint32 // 字节，默认 16
	Argon2KeyLength           uint32 // 字节，默认 32

	// --- Phase 3 搜索配置 ---
	// 引入动机：design/01-SEARCH.md 要求 ES、Embedding Provider、Reranker Provider 全部可配置。
	// 不写死 provider/base URL/API key/model/dimensions。

	// Elasticsearch 配置
	ESURL string // ES 节点 URL，如 http://localhost:9200

	// Embedding Provider 配置
	// 引入动机：design/01-SEARCH.md §Embedding Provider 要求配置 base_url, api_key, model, dimensions 等。
	EmbeddingBaseURL  string
	EmbeddingAPIKey   string
	EmbeddingModel    string
	EmbeddingDimensions int
	EmbeddingTimeoutSeconds int
	EmbeddingBatchSize      int
	EmbeddingQueryInstruction string
	EmbeddingDocInstruction   string

	// Reranker Provider 配置
	// 引入动机：design/01-SEARCH.md §Reranker Provider 要求配置 base_url, api_key, model, timeout 等。
	RerankerBaseURL       string
	RerankerAPIKey        string
	RerankerModel         string
	RerankerTimeoutSeconds int
	RerankerMaxCandidates  int

	// Job Worker 配置
	// 引入动机：design/05-OPERATIONS.md §Background Jobs 要求 PG-backed queue + worker。
	JobWorkerEnabled     bool
	JobWorkerPollInterval int64 // 秒

	// Scheduler 配置
	// 引入动机：design/05-OPERATIONS.md §Search Maintenance 要求周期性维护调度。
	SchedulerEnabled     bool
	SchedulerTickInterval int64 // 秒

	// 健康检查配置
	// 引入动机：readyz 中 embedding/reranker 两个检查会发起真实推理请求
	// （embedding 真实调用 / rerank 真实推理），而 PG/ES/job 统计/profile 只是轻量查询。
	// 线上事故：这两类检查共用写死的 5s 超时，provider 冷启动或模型推理稍慢就会被判为
	// context deadline exceeded，/readyz 长期把 Reranker 标成 degraded，且 docker healthcheck
	// 每 15s 打一次 /readyz 造成日志刷屏。
	// 因此为 provider 健康检查提供独立可配置的超时，与快检查的 5s 解耦。
	ProviderHealthCheckTimeout time.Duration

	// 根加密密钥
	// 引入动机：计划要求由部署方通过 MASTER_ENCRYPTION_KEY 环境变量提供
	// base64 编码的 32 字节根密钥，用于 AES-256-GCM 加密 Provider API Key。
	// 可为空——表示尚未配置，此时若数据库有密文则 fail-fast。
	MasterEncryptionKey string
}

// Load 从环境变量读取并验证全部配置。
// 任何必填项缺失或格式非法时返回错误，错误信息包含具体的环境变量名和原因。
func Load() (*Config, error) {
	var cfg Config
	var errs []string

	// --- PostgreSQL 连接配置 ---
	cfg.DBHost = os.Getenv("DB_HOST")
	if cfg.DBHost == "" {
		errs = append(errs, "DB_HOST 未设置：PostgreSQL 主机地址为必填项")
	}

	cfg.DBPort, _ = strconv.Atoi(os.Getenv("DB_PORT"))
	if os.Getenv("DB_PORT") == "" {
		errs = append(errs, "DB_PORT 未设置：PostgreSQL 端口为必填项")
	} else if cfg.DBPort <= 0 || cfg.DBPort > 65535 {
		errs = append(errs, fmt.Sprintf("DB_PORT 非法值 %d：端口范围应为 1-65535", cfg.DBPort))
	}

	cfg.DBUser = os.Getenv("DB_USER")
	if cfg.DBUser == "" {
		errs = append(errs, "DB_USER 未设置：PostgreSQL 用户名为必填项")
	}

	cfg.DBPassword = os.Getenv("DB_PASSWORD")
	if cfg.DBPassword == "" {
		errs = append(errs, "DB_PASSWORD 未设置：PostgreSQL 密码为必填项")
	}

	cfg.DBName = os.Getenv("DB_NAME")
	if cfg.DBName == "" {
		errs = append(errs, "DB_NAME 未设置：PostgreSQL 数据库名为必填项")
	}

	cfg.DBSSLMode = os.Getenv("DB_SSLMODE")
	if cfg.DBSSLMode == "" {
		errs = append(errs, "DB_SSLMODE 未设置：PostgreSQL SSL 模式为必填项，必须显式指定合法值 (disable/allow/prefer/require/verify-ca/verify-full)")
	} else if !isValidSSLMode(cfg.DBSSLMode) {
		errs = append(errs, fmt.Sprintf("DB_SSLMODE 非法值 %q：合法值为 disable/allow/prefer/require/verify-ca/verify-full", cfg.DBSSLMode))
	}

	// --- 服务基础运行配置 ---
	cfg.HTTPAddr = os.Getenv("HTTP_ADDR")
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}

	cfg.LogLevel = os.Getenv("LOG_LEVEL")
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	} else if !isValidLogLevel(cfg.LogLevel) {
		errs = append(errs, fmt.Sprintf("LOG_LEVEL 非法值 %q：合法值为 debug/info/warn/error", cfg.LogLevel))
	}

	shutdownTimeoutStr := os.Getenv("SHUTDOWN_TIMEOUT")
	if shutdownTimeoutStr == "" {
		cfg.ShutdownTimeout = 30 * time.Second
	} else {
		d, err := time.ParseDuration(shutdownTimeoutStr)
		if err != nil {
			errs = append(errs, fmt.Sprintf("SHUTDOWN_TIMEOUT 非法值 %q：应为有效时长如 30s、1m", shutdownTimeoutStr))
		} else if d <= 0 {
			errs = append(errs, fmt.Sprintf("SHUTDOWN_TIMEOUT 非法值 %q：必须为正数", shutdownTimeoutStr))
	} else {
		cfg.ShutdownTimeout = d
	}
}

// --- 公开服务 origin 配置 ---
// 引入动机：device authorization 的 verification_url 不能从请求 Host 推导，
// 必须显式配置可信 origin。生产缺失时 device start fail closed。
loadPublicOrigin(&cfg, &errs)

if len(errs) > 0 {
		return nil, fmt.Errorf("配置加载失败，共 %d 项问题:\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}

	// --- 认证模块配置 ---
	// 未设置的环境变量使用安全默认值，已设置的必须通过验证。
	loadAuthConfig(&cfg, &errs)

	if len(errs) > 0 {
		return nil, fmt.Errorf("配置加载失败，共 %d 项问题:\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}

	// --- Phase 3 搜索配置 ---
	// 引入动机：design/01-SEARCH.md 要求 ES、Embedding、Reranker 全部可配置。
	// 不写死 provider/base URL/API key/model/dimensions。
	// 这些配置项缺失时不产生错误（搜索降级），但已设置的非法值产生错误。
	loadSearchConfig(&cfg, &errs)

	if len(errs) > 0 {
		return nil, fmt.Errorf("配置加载失败，共 %d 项问题:\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}

	// --- 健康检查配置 ---
	// 引入动机：readyz 中 embedding/reranker 检查会发起真实推理请求，需要独立于快检查的超时。
	loadHealthCheckConfig(&cfg, &errs)

	if len(errs) > 0 {
		return nil, fmt.Errorf("配置加载失败，共 %d 项问题:\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}

	return &cfg, nil
}

// DSN 返回 PostgreSQL 连接字符串，供 lib/pq 或 pgx 驱动使用。
// 格式：host=... port=... user=... password=... dbname=... sslmode=...
func (c *Config) DSN() string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSSLMode)
}

// validSSLModes 是 PostgreSQL 支持的合法 SSL 模式集合。
// 引入动机：DB_SSLMODE 必须显式设置为合法值，禁止缺失时静默默认 disable。
var validSSLModes = map[string]bool{
	"disable":    true,
	"allow":      true,
	"prefer":     true,
	"require":    true,
	"verify-ca":  true,
	"verify-full": true,
}

// isValidSSLMode 判断给定值是否为 PostgreSQL 支持的合法 SSL 模式。
func isValidSSLMode(mode string) bool {
	return validSSLModes[mode]
}

// validLogLevels 是系统允许的合法日志级别集合，对应 slog 标准级别。
// 引入动机：LOG_LEVEL 非法值不应被静默降级为 info，而应在配置加载阶段 fail-fast。
var validLogLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

// isValidLogLevel 判断给定值是否为合法的日志级别。
func isValidLogLevel(level string) bool {
	return validLogLevels[level]
}

// loadPublicOrigin 从 PUBLIC_ORIGIN 环境变量加载可信的公开服务 origin。
// 引入动机：避免 device authorization 的 verification_url 被 Host header 注入操纵。
// 仅允许 http/https absolute origin，且不得包含 userinfo、path、query 或 fragment。
func loadPublicOrigin(cfg *Config, errs *[]string) {
	raw := os.Getenv("PUBLIC_ORIGIN")
	if raw == "" {
		cfg.PublicOrigin = ""
		return
	}

	u, err := url.Parse(raw)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("PUBLIC_ORIGIN 非法值 %q：无法解析 (%v)", raw, err))
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		*errs = append(*errs, fmt.Sprintf("PUBLIC_ORIGIN 非法值 %q：scheme 必须为 http 或 https", raw))
		return
	}
	if u.Host == "" {
		*errs = append(*errs, fmt.Sprintf("PUBLIC_ORIGIN 非法值 %q：必须包含 host", raw))
		return
	}
	if u.User != nil {
		*errs = append(*errs, fmt.Sprintf("PUBLIC_ORIGIN 非法值 %q：不得包含 userinfo", raw))
		return
	}
	if u.Path != "" && u.Path != "/" {
		*errs = append(*errs, fmt.Sprintf("PUBLIC_ORIGIN 非法值 %q：必须为 origin，不得包含路径", raw))
		return
	}
	if u.RawQuery != "" {
		*errs = append(*errs, fmt.Sprintf("PUBLIC_ORIGIN 非法值 %q：不得包含 query 参数", raw))
		return
	}
	if u.Fragment != "" {
		*errs = append(*errs, fmt.Sprintf("PUBLIC_ORIGIN 非法值 %q：不得包含 fragment", raw))
		return
	}

	cfg.PublicOrigin = strings.TrimRight(raw, "/")
}

// loadAuthConfig 从环境变量加载认证模块配置。
// 引入动机：auth 模块需要多个配置项（session/token 有效期、cookie 属性、Argon2id 参数），
// 集中在此函数加载和验证，保持 Load 函数简洁。
// 未设置的环境变量使用安全默认值；已设置的非法值产生错误。
func loadAuthConfig(cfg *Config, errs *[]string) {
	// Session 有效期（秒）
	if v := os.Getenv("SESSION_DURATION"); v != "" {
		d, err := strconv.ParseInt(v, 10, 64)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("SESSION_DURATION 非法值 %q：应为正整数（秒）", v))
		} else {
			cfg.SessionDuration = d
		}
	} else {
		cfg.SessionDuration = 86400 // 24 小时
	}

	// Device access token 有效期（秒）
	if v := os.Getenv("DEVICE_ACCESS_TOKEN_DURATION"); v != "" {
		d, err := strconv.ParseInt(v, 10, 64)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("DEVICE_ACCESS_TOKEN_DURATION 非法值 %q：应为正整数（秒）", v))
		} else {
			cfg.DeviceAccessTokenDuration = d
		}
	} else {
		cfg.DeviceAccessTokenDuration = 900 // 15 分钟
	}

	// Device refresh token 有效期（秒）
	if v := os.Getenv("DEVICE_REFRESH_TOKEN_DURATION"); v != "" {
		d, err := strconv.ParseInt(v, 10, 64)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("DEVICE_REFRESH_TOKEN_DURATION 非法值 %q：应为正整数（秒）", v))
		} else {
			cfg.DeviceRefreshTokenDuration = d
		}
	} else {
		cfg.DeviceRefreshTokenDuration = 2592000 // 30 天
	}

	// Cookie Secure 标志
	if v := os.Getenv("COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			*errs = append(*errs, fmt.Sprintf("COOKIE_SECURE 非法值 %q：应为 true/false", v))
		} else {
			cfg.CookieSecure = b
		}
	} else {
		cfg.CookieSecure = true
	}

	// Cookie Domain
	cfg.CookieDomain = os.Getenv("COOKIE_DOMAIN")

	// Cookie Path
	if v := os.Getenv("COOKIE_PATH"); v != "" {
		cfg.CookiePath = v
	} else {
		cfg.CookiePath = "/"
	}

	// Cookie Name
	if v := os.Getenv("COOKIE_NAME"); v != "" {
		cfg.CookieName = v
	} else {
		cfg.CookieName = "session"
	}

	// CSRF Cookie Name
	if v := os.Getenv("CSRF_COOKIE_NAME"); v != "" {
		cfg.CSRFCookieName = v
	} else {
		cfg.CSRFCookieName = "csrf"
	}

	// CSRF Header Name
	if v := os.Getenv("CSRF_HEADER_NAME"); v != "" {
		cfg.CSRFHeaderName = v
	} else {
		cfg.CSRFHeaderName = "X-CSRF-Token"
	}

	// Argon2id 参数
	if v := os.Getenv("ARGON2_MEMORY"); v != "" {
		d, err := strconv.ParseUint(v, 10, 32)
		if err != nil || d == 0 {
			*errs = append(*errs, fmt.Sprintf("ARGON2_MEMORY 非法值 %q：应为正整数（KiB）", v))
		} else {
			cfg.Argon2Memory = uint32(d)
		}
	} else {
		cfg.Argon2Memory = 64 * 1024 // 64 MiB
	}

	if v := os.Getenv("ARGON2_ITERATIONS"); v != "" {
		d, err := strconv.ParseUint(v, 10, 32)
		if err != nil || d == 0 {
			*errs = append(*errs, fmt.Sprintf("ARGON2_ITERATIONS 非法值 %q：应为正整数", v))
		} else {
			cfg.Argon2Iterations = uint32(d)
		}
	} else {
		cfg.Argon2Iterations = 3
	}

	if v := os.Getenv("ARGON2_PARALLELISM"); v != "" {
		d, err := strconv.ParseUint(v, 10, 8)
		if err != nil || d == 0 {
			*errs = append(*errs, fmt.Sprintf("ARGON2_PARALLELISM 非法值 %q：应为正整数", v))
		} else {
			cfg.Argon2Parallelism = uint8(d)
		}
	} else {
		cfg.Argon2Parallelism = 2
	}

	if v := os.Getenv("ARGON2_SALT_LENGTH"); v != "" {
		d, err := strconv.ParseUint(v, 10, 32)
		if err != nil || d == 0 {
			*errs = append(*errs, fmt.Sprintf("ARGON2_SALT_LENGTH 非法值 %q：应为正整数（字节）", v))
		} else {
			cfg.Argon2SaltLength = uint32(d)
		}
	} else {
		cfg.Argon2SaltLength = 16
	}

	if v := os.Getenv("ARGON2_KEY_LENGTH"); v != "" {
		d, err := strconv.ParseUint(v, 10, 32)
		if err != nil || d == 0 {
			*errs = append(*errs, fmt.Sprintf("ARGON2_KEY_LENGTH 非法值 %q：应为正整数（字节）", v))
		} else {
			cfg.Argon2KeyLength = uint32(d)
		}
	} else {
		cfg.Argon2KeyLength = 32
	}
}

// loadHealthCheckConfig 从环境变量加载健康检查配置。
// 引入动机：/readyz 中 embedding/reranker 检查会发起真实推理请求，
// 其耗时特性与 PG/ES/job/profile 等轻量检查完全不同，需要独立可配置的超时。
func loadHealthCheckConfig(cfg *Config, errs *[]string) {
	// Provider 健康检查超时（秒）
	// 引入动机：embedding/reranker 检查会发起真实推理请求，冷启动或模型推理耗时远超
	// 快检查所需的 5s；写死 5s 会把"只是慢"的 provider 误报为 degraded（线上事故）。
	if v := os.Getenv("HEALTH_PROVIDER_CHECK_TIMEOUT_SECONDS"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("HEALTH_PROVIDER_CHECK_TIMEOUT_SECONDS 非法值 %q：应为正整数（秒）", v))
		} else {
			cfg.ProviderHealthCheckTimeout = time.Duration(d) * time.Second
		}
	} else {
		cfg.ProviderHealthCheckTimeout = 30 * time.Second
	}
}

// loadSearchConfig 从环境变量加载 Phase 3 搜索配置。
// 引入动机：design/01-SEARCH.md 要求 ES、Embedding Provider、Reranker Provider 全部可配置。
// 这些配置项缺失时不产生错误（搜索降级为 unavailable），但已设置的非法值产生错误。
// 不写死 provider/base URL/API key/model/dimensions。
func loadSearchConfig(cfg *Config, errs *[]string) {
	// Elasticsearch URL（可选，缺失时搜索返回 degraded）
	cfg.ESURL = os.Getenv("ES_URL")

	// Embedding Provider 配置
	cfg.EmbeddingBaseURL = os.Getenv("EMBEDDING_BASE_URL")
	cfg.EmbeddingAPIKey = os.Getenv("EMBEDDING_API_KEY")
	cfg.EmbeddingModel = os.Getenv("EMBEDDING_MODEL")
	if v := os.Getenv("EMBEDDING_DIMENSIONS"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("EMBEDDING_DIMENSIONS 非法值 %q：应为正整数", v))
		} else {
			cfg.EmbeddingDimensions = d
		}
	} else {
		cfg.EmbeddingDimensions = 1024
	}
	if v := os.Getenv("EMBEDDING_TIMEOUT_SECONDS"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("EMBEDDING_TIMEOUT_SECONDS 非法值 %q：应为正整数", v))
		} else {
			cfg.EmbeddingTimeoutSeconds = d
		}
	} else {
		cfg.EmbeddingTimeoutSeconds = 30
	}
	if v := os.Getenv("EMBEDDING_BATCH_SIZE"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("EMBEDDING_BATCH_SIZE 非法值 %q：应为正整数", v))
		} else {
			cfg.EmbeddingBatchSize = d
		}
	} else {
		cfg.EmbeddingBatchSize = 32
	}
	cfg.EmbeddingQueryInstruction = os.Getenv("EMBEDDING_QUERY_INSTRUCTION")
	cfg.EmbeddingDocInstruction = os.Getenv("EMBEDDING_DOCUMENT_INSTRUCTION")

	// Reranker Provider 配置
	cfg.RerankerBaseURL = os.Getenv("RERANKER_BASE_URL")
	cfg.RerankerAPIKey = os.Getenv("RERANKER_API_KEY")
	cfg.RerankerModel = os.Getenv("RERANKER_MODEL")
	if v := os.Getenv("RERANKER_TIMEOUT_SECONDS"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("RERANKER_TIMEOUT_SECONDS 非法值 %q：应为正整数", v))
		} else {
			cfg.RerankerTimeoutSeconds = d
		}
	} else {
		cfg.RerankerTimeoutSeconds = 10
	}
	if v := os.Getenv("RERANKER_MAX_CANDIDATES"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("RERANKER_MAX_CANDIDATES 非法值 %q：应为正整数", v))
		} else {
			cfg.RerankerMaxCandidates = d
		}
	} else {
		cfg.RerankerMaxCandidates = 20
	}

	// Job Worker 配置
	if v := os.Getenv("JOB_WORKER_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			*errs = append(*errs, fmt.Sprintf("JOB_WORKER_ENABLED 非法值 %q：应为 true/false", v))
		} else {
			cfg.JobWorkerEnabled = b
		}
	}
	if v := os.Getenv("JOB_WORKER_POLL_INTERVAL"); v != "" {
		d, err := strconv.ParseInt(v, 10, 64)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("JOB_WORKER_POLL_INTERVAL 非法值 %q：应为正整数（秒）", v))
		} else {
			cfg.JobWorkerPollInterval = d
		}
	} else {
		cfg.JobWorkerPollInterval = 5
	}

	// Scheduler 配置
	if v := os.Getenv("SCHEDULER_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			*errs = append(*errs, fmt.Sprintf("SCHEDULER_ENABLED 非法值 %q：应为 true/false", v))
		} else {
			cfg.SchedulerEnabled = b
		}
	}
	if v := os.Getenv("SCHEDULER_TICK_INTERVAL"); v != "" {
		d, err := strconv.ParseInt(v, 10, 64)
		if err != nil || d <= 0 {
			*errs = append(*errs, fmt.Sprintf("SCHEDULER_TICK_INTERVAL 非法值 %q：应为正整数（秒）", v))
		} else {
			cfg.SchedulerTickInterval = d
		}
	} else {
		cfg.SchedulerTickInterval = 60
	}

	// 根加密密钥（可选，但数据库有密文时必须存在且合法）
	// 引入动机：计划要求由部署方提供 base64 编码的 32 字节根密钥。
	// 此处仅读取原始值，格式校验在启动阶段由 crypto.ParseRootKey 执行。
	cfg.MasterEncryptionKey = os.Getenv("MASTER_ENCRYPTION_KEY")
}
