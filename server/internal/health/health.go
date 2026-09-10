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
	"sync"
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

// --- 健康检查超时策略 ---
//
// 引入动机：检查被分为两类，耗时特性完全不同，不能共用一个超时值。
//
//	1. 快检查（PG Ping / ES Ping / job 统计 / active profile）只是轻量查询或连通性探测，
//	   5s 足够，且需要快速失败以免拖慢 /readyz 响应。
//	2. Provider 检查（Embedding / Reranker）会发起真实推理请求：
//	   EmbeddingChecker.Available 触发真实 embedding 调用，
//	   RerankerChecker.Available 触发真实 rerank 推理（provider.go 内部调用 Rerank）。
//	   推理在 provider 冷启动、模型加载或长文本场景下很容易超过 5s。
//
// 线上事故：两类检查曾共用写死的 5s，provider 只是"慢"就被判为
// "context deadline exceeded"，日志持续输出
// "reranker API 调用失败，将降级为 RRF 结果 ... context deadline exceeded"，
// /readyz 长期把 Reranker 报成 degraded，加之 docker healthcheck 每 15s 打一次 /readyz 造成刷屏。
// 因此 Provider 检查使用独立可配置的超时（默认 30s，由 HEALTH_PROVIDER_CHECK_TIMEOUT_SECONDS 配置）。
//
// 注意：该超时是健康检查自身的 context deadline，与 Web 管理界面配置的 provider timeout_seconds
// 相互独立——两者同时存在时以更短者为准。
const (
	// defaultCheckTimeout 是快检查（PG/ES/job 统计/profile）的单次超时。
	// 引入动机：这些检查只做连通性或单表查询，保持 5s 以便 /readyz 快速返回。
	defaultCheckTimeout = 5 * time.Second

	// DefaultProviderCheckTimeout 是 Provider 检查（embedding/reranker）的默认超时。
	// 引入动机：这两个检查会发起真实推理请求，5s 会误报 degraded（见本节线上事故说明）。
	// 生产可经 HEALTH_PROVIDER_CHECK_TIMEOUT_SECONDS 覆盖。
	DefaultProviderCheckTimeout = 30 * time.Second
)

// SnapshotCollector 采集健康快照。
// 引入动机：将各依赖检查器组合在一起，按依赖类型使用相应超时采集健康状态。
// 不使用 XxxService/XxxManager 命名。
type SnapshotCollector struct {
	pg          PGChecker
	es          ESChecker
	embedding   EmbeddingChecker
	reranker    RerankerChecker
	jobStats    JobStatsQuery
	profileRepo ProfileQuery
	// checkTimeout 是快检查（PG/ES/job 统计/profile）的超时时间。
	// 引入动机：避免某个依赖检查阻塞整个 readyz 响应。
	// 这些检查只做轻量查询，保持固定的 5s。
	checkTimeout time.Duration
	// providerCheckTimeout 是 Provider 检查（embedding/reranker）的超时时间。
	// 引入动机：这两个检查会发起真实推理请求，必须与快检查解耦，
	// 否则慢 provider 会被误判为 degraded（线上事故根因）。
	providerCheckTimeout time.Duration
}

// SnapshotCollectorOption 是 NewSnapshotCollector 的构造选项。
// 引入动机：依赖参数已达 6 个，超时又分快检查与 Provider 两类；
// 用选项覆盖超时可保持既有调用点不变，也便于测试注入毫秒级超时。
type SnapshotCollectorOption func(*SnapshotCollector)

// WithProviderCheckTimeout 覆盖 Provider 检查（embedding/reranker）的超时。
// 引入动机：生产由配置项 HEALTH_PROVIDER_CHECK_TIMEOUT_SECONDS 提供该值（默认 30s），
// 测试需要注入较短的超时以缩短用例耗时。
// 作用：仅影响 embedding/reranker 检查，快检查仍使用 defaultCheckTimeout。
// 传入非正数时 panic（fail fast：超时配置错误必须立即暴露，不允许静默回退）。
func WithProviderCheckTimeout(d time.Duration) SnapshotCollectorOption {
	return func(c *SnapshotCollector) {
		if d <= 0 {
			panic(fmt.Sprintf("health: Provider 健康检查超时必须为正数，实际为 %v", d))
		}
		c.providerCheckTimeout = d
	}
}

// NewSnapshotCollector 创建健康快照采集器。
// 引入动机：main.go 需要注入所有依赖以构造 readyz handler。
// 所有依赖参数可为 nil，表示该依赖未配置（状态为 unavailable/degraded）。
// opts 可覆盖默认超时（快检查 defaultCheckTimeout、Provider 检查 DefaultProviderCheckTimeout）；
// 不传 opts 时全部使用默认值，main.go 传入 WithProviderCheckTimeout 接入配置。
func NewSnapshotCollector(pg PGChecker, esClient ESChecker, emb EmbeddingChecker, rr RerankerChecker, jobStats JobStatsQuery, profileRepo ProfileQuery, opts ...SnapshotCollectorOption) *SnapshotCollector {
	c := &SnapshotCollector{
		pg:                   pg,
		es:                   esClient,
		embedding:            emb,
		reranker:             rr,
		jobStats:             jobStats,
		profileRepo:          profileRepo,
		checkTimeout:         defaultCheckTimeout,
		providerCheckTimeout: DefaultProviderCheckTimeout,
	}
	for _, opt := range opts {
		if opt == nil {
			panic("health: NewSnapshotCollector 收到 nil 选项")
		}
		opt(c)
	}
	return c
}

// Collect 并发采集当前健康快照。
//
// 引入动机（为什么并发）：各依赖检查原先顺序执行，最坏耗时是各项超时之和
// （5s PG + 5s ES + 30s embedding + 30s reranker + 5s job + 5s profile = 80s）。
// 而 docker-compose 中 server 的 healthcheck timeout 只有 10s：provider 慢或挂时，
// healthcheck 命令会先被 docker 杀掉，容器被判为 unhealthy，
// 即使 /readyz 本应返回 200 + degraded；这同时污染 depends_on: service_healthy 的启动判定。
// 改为每项检查各自 goroutine 并发执行后，最坏耗时从"各项之和"降为"单项最长"（30s）。
//
// 对外语义不变：PG 失败 → 503（unavailable）；ES/embedding/reranker 失败 → 200 + degraded；
// 各 ComponentStatus 的 Status/Error 取值与字段映射保持原样。
//
// 并发安全：每个 goroutine 只写自己独占的局部变量，全部完成后经 wg.Wait() 组装 Snapshot，
// 不存在跨 goroutine 的共享写入；每个 goroutine 内部各自 defer cancel() 释放 context。
//
// 超时语义不变：PG/ES/job 统计/profile 使用 checkTimeout（5s），
// embedding/reranker 使用 providerCheckTimeout（默认 30s，因为会发起真实推理请求）。
func (c *SnapshotCollector) Collect(ctx context.Context) Snapshot {
	// 各检查结果先写入独占局部变量：goroutine 之间不共享任何可写内存，从根上避免 data race。
	var (
		pgStatus      ComponentStatus
		esStatus      ComponentStatus
		embStatus     ComponentStatus
		rrStatus      ComponentStatus
		pendingJobs   int
		failedJobs    int
		activeProfile ActiveProfileInfo
	)

	// wg 等待全部依赖检查结束；wg.Wait() 同时构成 happens-before，
	// 保证下方组装 Snapshot 时能看到各 goroutine 的写入结果。
	var wg sync.WaitGroup

	// PostgreSQL 检查（PG 失败 = 服务不可用）
	wg.Add(1)
	go func() {
		defer wg.Done()
		if c.pg == nil {
			pgStatus = ComponentStatus{Status: "unavailable", Error: "PG 连接未配置"}
			return
		}
		pgCtx, cancel := context.WithTimeout(ctx, c.checkTimeout)
		defer cancel()
		if err := c.pg.PingContext(pgCtx); err != nil {
			pgStatus = ComponentStatus{Status: "unavailable", Error: safeError(err)}
			return
		}
		pgStatus = ComponentStatus{Status: "healthy"}
	}()

	// Elasticsearch 检查（ES 失败 = degraded）
	wg.Add(1)
	go func() {
		defer wg.Done()
		if c.es == nil {
			esStatus = ComponentStatus{Status: "unavailable", Error: "ES 客户端未配置"}
			return
		}
		esCtx, cancel := context.WithTimeout(ctx, c.checkTimeout)
		defer cancel()
		if err := c.es.Ping(esCtx); err != nil {
			esStatus = ComponentStatus{Status: "degraded", Error: safeError(err)}
			return
		}
		esStatus = ComponentStatus{Status: "healthy"}
	}()

	// Embedding Provider 检查（不可用 = degraded）
	// 使用 providerCheckTimeout：Available 会发起真实 embedding 调用，5s 会误报 degraded。
	wg.Add(1)
	go func() {
		defer wg.Done()
		if c.embedding == nil {
			embStatus = ComponentStatus{Status: "unavailable", Error: "Embedding Provider 未配置"}
			return
		}
		embCtx, cancel := context.WithTimeout(ctx, c.providerCheckTimeout)
		defer cancel()
		if c.embedding.Available(embCtx) {
			embStatus = ComponentStatus{Status: "healthy"}
			return
		}
		embStatus = ComponentStatus{Status: "degraded", Error: "Embedding Provider 不可用"}
	}()

	// Reranker Provider 检查（不可用 = degraded）
	// 使用 providerCheckTimeout：Available 会发起真实 rerank 推理，5s 会误报 degraded。
	wg.Add(1)
	go func() {
		defer wg.Done()
		if c.reranker == nil {
			rrStatus = ComponentStatus{Status: "unavailable", Error: "Reranker Provider 未配置"}
			return
		}
		rrCtx, cancel := context.WithTimeout(ctx, c.providerCheckTimeout)
		defer cancel()
		if c.reranker.Available(rrCtx) {
			rrStatus = ComponentStatus{Status: "healthy"}
			return
		}
		rrStatus = ComponentStatus{Status: "degraded", Error: "Reranker Provider 不可用"}
	}()

	// Job 统计（同一个 goroutine 内串行执行两次轻量查询，只写 pendingJobs/failedJobs）
	wg.Add(1)
	go func() {
		defer wg.Done()
		if c.jobStats == nil {
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, c.checkTimeout)
		defer cancel()
		if pending, err := c.jobStats.CountByStatus(jobCtx, "pending"); err != nil {
			slog.Warn("readyz 查询 pending job 计数失败", "error", err)
		} else {
			pendingJobs = pending
		}
		if failed, err := c.jobStats.CountByStatus(jobCtx, "dead"); err != nil {
			slog.Warn("readyz 查询 dead job 计数失败", "error", err)
		} else {
			failedJobs = failed
		}
	}()

	// Active profile（只写 activeProfile）
	wg.Add(1)
	go func() {
		defer wg.Done()
		if c.profileRepo == nil {
			return
		}
		profCtx, cancel := context.WithTimeout(ctx, c.checkTimeout)
		defer cancel()
		id, name, err := c.profileRepo.GetActiveProfileID(profCtx)
		if err != nil {
			slog.Warn("readyz 查询 active profile 失败", "error", err)
			return
		}
		activeProfile = ActiveProfileInfo{ID: id, Name: name}
	}()

	wg.Wait()

	return Snapshot{
		PostgreSQL:    pgStatus,
		Elasticsearch: esStatus,
		Embedding:     embStatus,
		Reranker:      rrStatus,
		PendingJobs:   pendingJobs,
		FailedJobs:    failedJobs,
		ActiveProfile: activeProfile,
	}
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
