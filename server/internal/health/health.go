// Package health 实现进程存活和就绪检查逻辑。
//
// 引入动机：design/05-OPERATIONS.md §Health 要求：
//   - /healthz：进程存活端点，不因 ES/Provider 降级而失败
//   - /readyz：结构化报告 PG、ES、Embedding、Reranker、pending/failed jobs、active profile
//
// 设计原则：
//   - 不创建 XxxService/XxxManager/XxxController 超级对象
//   - 依赖通过构造函数注入，不使用全局变量或反射
//   - PG 失败为非 success；ES/Provider 失败为 degraded 但文档服务仍 ready
//   - 错误信息安全可观测，不泄露 API key/DSN
package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"partitura/server/internal/es"
	"partitura/server/internal/profile"
	"partitura/server/internal/search/embedding"
	"partitura/server/internal/search/reranker"
)

// PGChecker 定义 PostgreSQL 连通性检查接口。
// 引入动机：readyz 需要检查 PG 是否可用，使用接口便于测试注入。
type PGChecker interface {
	PingContext(ctx context.Context) error
}

// ESChecker 定义 Elasticsearch 连通性检查接口。
// 引入动机：readyz 需要检查 ES 是否可用。
// 复用 es.Client 的 Ping 方法。
type ESChecker interface {
	Ping(ctx context.Context) error
}

// EmbeddingChecker 定义 Embedding Provider 可用性检查接口。
// 引入动机：readyz 需要检查 Embedding Provider 是否可用。
// 复用 embedding.Provider 的 Available 方法。
type EmbeddingChecker interface {
	Available(ctx context.Context) bool
}

// RerankerChecker 定义 Reranker Provider 可用性检查接口。
// 引入动机：readyz 需要检查 Reranker Provider 是否可用。
// 复用 reranker.Provider 的 Available 方法。
type RerankerChecker interface {
	Available(ctx context.Context) bool
}

// JobStatsQuery 定义查询 job 统计信息的接口。
// 引入动机：readyz 需要报告 pending 和 failed/dead job 计数。
type JobStatsQuery interface {
	// CountByStatus 返回指定状态的 job 数量。
	CountByStatus(ctx context.Context, status string) (int, error)
}

// ProfileQuery 定义查询 active profile 的接口。
// 引入动机：readyz 需要报告当前 active search profile。
type ProfileQuery interface {
	// GetActiveProfileID 返回当前 active profile 的 ID 和名称，无 active profile 时返回空字符串。
	GetActiveProfileID(ctx context.Context) (id string, name string, err error)
}

// Snapshot 是某一时刻的健康快照。
// 引入动机：readyz 需要并行采集各依赖状态后汇总返回。
// 每个字段记录对应依赖的检查结果。
type Snapshot struct {
	// PostgreSQL 连通性状态。
	// 引入动机：PG 是业务真相源，失败时服务不可用。
	PostgreSQL ComponentStatus `json:"postgresql"`
	// Elasticsearch 连通性状态。
	// 引入动机：ES 是可重建索引，失败时搜索降级但文档服务仍可用。
	Elasticsearch ComponentStatus `json:"elasticsearch"`
	// Embedding Provider 可用性状态。
	// 引入动机：Embedding 不可用时 lexical 搜索仍可用。
	Embedding ComponentStatus `json:"embedding"`
	// Reranker Provider 可用性状态。
	// 引入动机：Reranker 不可用时返回 RRF 结果。
	Reranker ComponentStatus `json:"reranker"`
	// Pending job 计数。
	// 引入动机：运维需要监控积压任务。
	PendingJobs int `json:"pending_jobs"`
	// Failed/dead job 计数。
	// 引入动机：运维需要监控失败任务。
	FailedJobs int `json:"failed_jobs"`
	// Active search profile 信息。
	// 引入动机：运维需要知道当前生效的搜索配置。
	ActiveProfile ActiveProfileInfo `json:"active_profile"`
}

// ComponentStatus 是单个依赖组件的检查结果。
// 引入动机：统一描述各依赖的可用性状态和错误信息。
type ComponentStatus struct {
	// Status 是组件状态："healthy"、"degraded"、"unavailable"。
	Status string `json:"status"`
	// Error 是检查失败时的安全错误描述（不包含敏感信息）。
	// 引入动机：提供可观测的错误信息，不泄露 API key/DSN。
	Error string `json:"error,omitempty"`
}

// ActiveProfileInfo 是当前 active profile 的摘要信息。
// 引入动机：readyz 需要报告当前搜索配置。
type ActiveProfileInfo struct {
	// ID 是 profile 的 UUID，无 active profile 时为空。
	ID string `json:"id"`
	// Name 是 profile 名称。
	Name string `json:"name"`
}

// SnapshotCollector 采集健康快照。
// 引入动机：将各依赖检查器组合在一起，并行采集健康状态。
// 不使用 XxxService/XxxManager 命名。
type SnapshotCollector struct {
	pg          PGChecker
	es          ESChecker
	embedding   EmbeddingChecker
	reranker    RerankerChecker
	jobStats    JobStatsQuery
	profileRepo ProfileQuery
	// checkTimeout 是单次检查的超时时间。
	// 引入动机：避免某个依赖检查阻塞整个 readyz 响应。
	checkTimeout time.Duration
}

// NewSnapshotCollector 创建健康快照采集器。
// 引入动机：main.go 需要注入所有依赖以构造 readyz handler。
// 所有参数可为 nil，表示该依赖未配置（状态为 unavailable/degraded）。
func NewSnapshotCollector(pg PGChecker, esClient ESChecker, emb EmbeddingChecker, rr RerankerChecker, jobStats JobStatsQuery, profileRepo ProfileQuery) *SnapshotCollector {
	return &SnapshotCollector{
		pg:          pg,
		es:          esClient,
		embedding:   emb,
		reranker:    rr,
		jobStats:    jobStats,
		profileRepo: profileRepo,
		checkTimeout: 5 * time.Second,
	}
}

// Collect 采集当前健康快照。
// 引入动机：readyz handler 调用此方法获取各依赖状态。
// 各依赖检查使用独立超时 context，避免互相阻塞。
func (c *SnapshotCollector) Collect(ctx context.Context) Snapshot {
	snap := Snapshot{}

	// PostgreSQL 检查（PG 失败 = 服务不可用）
	if c.pg == nil {
		snap.PostgreSQL = ComponentStatus{Status: "unavailable", Error: "PG 连接未配置"}
	} else {
		pgCtx, cancel := context.WithTimeout(ctx, c.checkTimeout)
		if err := c.pg.PingContext(pgCtx); err != nil {
			snap.PostgreSQL = ComponentStatus{Status: "unavailable", Error: safeError(err)}
		} else {
			snap.PostgreSQL = ComponentStatus{Status: "healthy"}
		}
		cancel()
	}

	// Elasticsearch 检查（ES 失败 = degraded）
	if c.es == nil {
		snap.Elasticsearch = ComponentStatus{Status: "unavailable", Error: "ES 客户端未配置"}
	} else {
		esCtx, cancel := context.WithTimeout(ctx, c.checkTimeout)
		if err := c.es.Ping(esCtx); err != nil {
			snap.Elasticsearch = ComponentStatus{Status: "degraded", Error: safeError(err)}
		} else {
			snap.Elasticsearch = ComponentStatus{Status: "healthy"}
		}
		cancel()
	}

	// Embedding Provider 检查（不可用 = degraded）
	if c.embedding == nil {
		snap.Embedding = ComponentStatus{Status: "unavailable", Error: "Embedding Provider 未配置"}
	} else {
		embCtx, cancel := context.WithTimeout(ctx, c.checkTimeout)
		if c.embedding.Available(embCtx) {
			snap.Embedding = ComponentStatus{Status: "healthy"}
		} else {
			snap.Embedding = ComponentStatus{Status: "degraded", Error: "Embedding Provider 不可用"}
		}
		cancel()
	}

	// Reranker Provider 检查（不可用 = degraded）
	if c.reranker == nil {
		snap.Reranker = ComponentStatus{Status: "unavailable", Error: "Reranker Provider 未配置"}
	} else {
		rrCtx, cancel := context.WithTimeout(ctx, c.checkTimeout)
		if c.reranker.Available(rrCtx) {
			snap.Reranker = ComponentStatus{Status: "healthy"}
		} else {
			snap.Reranker = ComponentStatus{Status: "degraded", Error: "Reranker Provider 不可用"}
		}
		cancel()
	}

	// Job 统计
	if c.jobStats != nil {
		jobCtx, cancel := context.WithTimeout(ctx, c.checkTimeout)
		if pending, err := c.jobStats.CountByStatus(jobCtx, "pending"); err != nil {
			slog.Warn("readyz 查询 pending job 计数失败", "error", err)
		} else {
			snap.PendingJobs = pending
		}
		if failed, err := c.jobStats.CountByStatus(jobCtx, "dead"); err != nil {
			slog.Warn("readyz 查询 dead job 计数失败", "error", err)
		} else {
			snap.FailedJobs = failed
		}
		cancel()
	}

	// Active profile
	if c.profileRepo != nil {
		profCtx, cancel := context.WithTimeout(ctx, c.checkTimeout)
		id, name, err := c.profileRepo.GetActiveProfileID(profCtx)
		if err != nil {
			slog.Warn("readyz 查询 active profile 失败", "error", err)
		} else {
			snap.ActiveProfile = ActiveProfileInfo{ID: id, Name: name}
		}
		cancel()
	}

	return snap
}

// IsReady 判断快照是否表示服务就绪。
// 引入动机：readyz 需要返回 HTTP 状态码，PG 不可用时 503，其余降级仍 200。
func (s *Snapshot) IsReady() bool {
	return s.PostgreSQL.Status == "healthy"
}

// safeError 将错误转换为安全描述，不泄露 DSN/API key 等敏感信息。
//
// 引入动机：design 要求 health 响应不泄露敏感数据。
//
// 策略：白名单优先的稳健脱敏。
//   1. 先检查是否命中已知敏感模式（黑名单兜底）——命中则返回通用消息。
//   2. 再检查是否匹配已知安全模式（白名单）——命中则返回截断后的原始消息。
//   3. 既不敏感也不在白名单中的错误，返回通用消息，确保未知敏感信息不泄漏。
//
// 可观测性保留：原始错误通过 slog.Warn 记录到服务日志，HTTP 响应仅暴露安全描述。
func safeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()

	// --- 第一步：黑名单兜底 ---
	// 已知敏感模式命中时，立即脱敏。
	// 引入动机：即使白名单遗漏，黑名单仍能拦截常见敏感模式。
	for _, pattern := range sensitivePatterns {
		if containsCI(msg, pattern) {
			slog.Warn("health 检测到敏感错误信息，已脱敏", "original_length", len(msg))
			return "连接失败（详情见服务日志）"
		}
	}

	// --- 第二步：白名单放行 ---
	// 仅当错误匹配已知安全模式时，才将原始消息（截断）暴露到 HTTP 响应。
	// 引入动机：保证只有明确安全的错误信息才对外可见，未知模式默认脱敏。
	for _, pattern := range safeErrorPatterns {
		if containsCI(msg, pattern) {
			return truncateError(msg)
		}
	}

	// --- 第三步：未知错误默认脱敏 ---
	// 引入动机：未知错误可能包含未预见的敏感信息，默认不暴露。
	slog.Warn("health 遇到未分类错误，已脱敏", "original_length", len(msg))
	return "连接失败（详情见服务日志）"
}

// sensitivePatterns 是已知可能包含敏感信息的错误模式（黑名单）。
// 引入动机：作为白名单的兜底保护，拦截常见敏感模式。
var sensitivePatterns = []string{
	"password=",
	"password@",
	"apikey=",
	"api_key=",
	"api-key=",
	"bearer ",
	"authorization:",
	"authorization=",
	"dsn=",
	"dsn:",
	"user=",
	"credential",
	"secret",
	"token=",
	"token:",
	"access_key",
	"accesskey",
	"private_key",
	"privatekey",
	"connection string",
	"connstr",
	"://", // URL 可能包含凭据，如 postgres://user:pass@host
}

// safeErrorPatterns 是已知安全的错误模式（白名单）。
// 引入动机：只有匹配这些模式的错误信息才允许出现在 HTTP 响应中。
// 这些模式都是连接级/网络级错误，不包含敏感数据。
var safeErrorPatterns = []string{
	"connection refused",
	"connection reset",
	"connect: connection refused",
	"context deadline exceeded",
	"context canceled",
	"deadline exceeded",
	"i/o timeout",
	"timeout",
	"no such host",
	"network is unreachable",
	"host is down",
	"connection closed",
	"EOF",
	"broken pipe",
	"server not available",
	"未配置",
	"为 nil",
	"不可用",
}

// truncateError 将错误消息截断到安全长度。
// 引入动机：防止过长错误消息在 HTTP 响应中暴露意外信息。
func truncateError(msg string) string {
	if len(msg) > 200 {
		return msg[:200] + "..."
	}
	return msg
}

// containsCI 是不区分大小写的子串检查。
func containsCI(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			c1 := s[i+j]
			c2 := substr[j]
			if c1 >= 'A' && c1 <= 'Z' {
				c1 += 32
			}
			if c2 >= 'A' && c2 <= 'Z' {
				c2 += 32
			}
			if c1 != c2 {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// HealthzHandler 处理 GET /healthz 请求。
// 引入动机：design/05-OPERATIONS.md §Health 要求进程存活端点。
// 不因 ES/Provider 降级而失败，仅报告进程可响应。
func HealthzHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

// ReadyHandler 处理 GET /readyz 请求。
// 引入动机：design/05-OPERATIONS.md §Health 要求就绪端点。
// PG 不可用时返回 503；ES/Provider 降级时返回 200 但标注 degraded。
func ReadyHandler(collector *SnapshotCollector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		snap := collector.Collect(r.Context())

		status := "ready"
		if !snap.IsReady() {
			status = "not_ready"
		} else if snap.Elasticsearch.Status != "healthy" || snap.Embedding.Status != "healthy" || snap.Reranker.Status != "healthy" {
			status = "degraded"
		}

		resp := struct {
			Status   string   `json:"status"`
			Snapshot Snapshot `json:"checks"`
		}{
			Status:   status,
			Snapshot: snap,
		}

		w.Header().Set("Content-Type", "application/json")
		if snap.IsReady() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// --- 适配器实现 ---
// 引入动机：main.go 已有 *sql.DB、es.Client、embedding.Provider、reranker.Provider、
// job.PGRepository、profile.PGRepository 等具体类型。需要适配为 health 包定义的接口。
// 适配器在 main.go 中组装，避免 health 包依赖具体实现包。

// PGPingAdapter 将 *sql.DB 适配为 PGChecker。
// 引入动机：health 包定义 PGChecker 接口，*sql.DB 的 PingContext 方法天然匹配。
type PGPingAdapter struct {
	DB *sql.DB
}

// PingContext 检查 PG 连通性。
func (a *PGPingAdapter) PingContext(ctx context.Context) error {
	if a.DB == nil {
		return fmt.Errorf("数据库连接为 nil")
	}
	return a.DB.PingContext(ctx)
}

// ESPingAdapter 将 es.Client 适配为 ESChecker。
// 引入动机：health 包定义 ESChecker 接口，es.Client 的 Ping 方法天然匹配。
type ESPingAdapter struct {
	Client es.Client
}

// Ping 检查 ES 连通性。
func (a *ESPingAdapter) Ping(ctx context.Context) error {
	if a.Client == nil {
		return fmt.Errorf("ES 客户端为 nil")
	}
	return a.Client.Ping(ctx)
}

// EmbeddingCheckAdapter 将 embedding.Provider 适配为 EmbeddingChecker。
// 引入动机：health 包定义 EmbeddingChecker 接口，embedding.Provider 的 Available 方法天然匹配。
// 使用函数引用而非静态实例，使健康检查能反映 Provider Registry 的热更新——
// 管理员保存新 Provider 后，健康检查立即使用新实例，无需重启。
type EmbeddingCheckAdapter struct {
	// ProviderFn 返回当前 Embedding Provider 实例（可能为 nil）。
	// 引入动机：支持从 Provider Registry 动态获取当前实例。
	ProviderFn func() embedding.Provider
}

// Available 检查 Embedding Provider 是否可用。
func (a *EmbeddingCheckAdapter) Available(ctx context.Context) bool {
	if a.ProviderFn == nil {
		return false
	}
	p := a.ProviderFn()
	if p == nil {
		return false
	}
	return p.Available(ctx)
}

// RerankerCheckAdapter 将 reranker.Provider 适配为 RerankerChecker。
// 引入动机：health 包定义 RerankerChecker 接口，reranker.Provider 的 Available 方法天然匹配。
// 使用函数引用而非静态实例，使健康检查能反映 Provider Registry 的热更新——
// 管理员保存新 Provider 后，健康检查立即使用新实例，无需重启。
type RerankerCheckAdapter struct {
	// ProviderFn 返回当前 Reranker Provider 实例（可能为 nil）。
	// 引入动机：支持从 Provider Registry 动态获取当前实例。
	ProviderFn func() reranker.Provider
}

// Available 检查 Reranker Provider 是否可用。
func (a *RerankerCheckAdapter) Available(ctx context.Context) bool {
	if a.ProviderFn == nil {
		return false
	}
	p := a.ProviderFn()
	if p == nil {
		return false
	}
	return p.Available(ctx)
}

// ProfileQueryAdapter 将 profile.PGRepository 适配为 ProfileQuery。
// 引入动机：health 包定义 ProfileQuery 接口，需要从 profile.PGRepository 获取 active profile。
type ProfileQueryAdapter struct {
	Repo profile.Repository
}

// GetActiveProfileID 返回当前 active profile 的 ID 和名称。
func (a *ProfileQueryAdapter) GetActiveProfileID(ctx context.Context) (string, string, error) {
	p, err := a.Repo.GetActiveProfile(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", nil
		}
		return "", "", err
	}
	return p.ID, p.Name, nil
}
