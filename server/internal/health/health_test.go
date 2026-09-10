// health_test.go 测试 health 包的 /healthz 和 /readyz 逻辑。
//
// 引入动机：plan-phase6 要求执行级 Go 单元/HTTP 测试：
//   - PG healthy/unhealthy
//   - ES/provider degraded
//   - job/profile 成功与错误
//   - healthz 与 readyz 状态码
//   - 不泄漏 secret
//
// 测试策略：直接调用 handler 并验证 HTTP 响应，不使用源码 contains 测试。
package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- Mock 实现 ---

type mockPGChecker struct {
	err error
}

func (m *mockPGChecker) PingContext(ctx context.Context) error {
	return m.err
}

type mockESChecker struct {
	err error
}

func (m *mockESChecker) Ping(ctx context.Context) error {
	return m.err
}

type mockEmbeddingChecker struct {
	available bool
}

func (m *mockEmbeddingChecker) Available(ctx context.Context) bool {
	return m.available
}

type mockRerankerChecker struct {
	available bool
}

func (m *mockRerankerChecker) Available(ctx context.Context) bool {
	return m.available
}

type mockJobStatsQuery struct {
	pendingCount int
	failedCount  int
	err          error
}

func (m *mockJobStatsQuery) CountByStatus(ctx context.Context, status string) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	if status == "pending" {
		return m.pendingCount, nil
	}
	if status == "dead" {
		return m.failedCount, nil
	}
	return 0, nil
}

type mockProfileQuery struct {
	id    string
	name  string
	err   error
}

func (m *mockProfileQuery) GetActiveProfileID(ctx context.Context) (string, string, error) {
	return m.id, m.name, m.err
}

// --- Tests ---

// TestHealthz_ReturnsOK 验证 healthz 始终返回 200。
func TestHealthz_ReturnsOK(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	HealthzHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("healthz 状态码 = %d, 期望 200", rr.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("解析 healthz 响应失败: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("healthz status = %q, 期望 \"ok\"", body["status"])
	}
}

// TestHealthz_MethodNotAllowed 验证非 GET 请求返回 405。
func TestHealthz_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rr := httptest.NewRecorder()

	HealthzHandler(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("healthz POST 状态码 = %d, 期望 405", rr.Code)
	}
}

// TestReadyz_AllHealthy 验证所有依赖健康时返回 200 和 ready 状态。
func TestReadyz_AllHealthy(t *testing.T) {
	collector := NewSnapshotCollector(
		&mockPGChecker{err: nil},
		&mockESChecker{err: nil},
		&mockEmbeddingChecker{available: true},
		&mockRerankerChecker{available: true},
		&mockJobStatsQuery{pendingCount: 3, failedCount: 1},
		&mockProfileQuery{id: "profile-1", name: "default"},
	)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	ReadyHandler(collector)(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("readyz 全部健康时状态码 = %d, 期望 200", rr.Code)
	}

	var resp struct {
		Status   string   `json:"status"`
		Snapshot Snapshot `json:"checks"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("解析 readyz 响应失败: %v", err)
	}
	if resp.Status != "ready" {
		t.Errorf("readyz status = %q, 期望 \"ready\"", resp.Status)
	}
	if resp.Snapshot.PostgreSQL.Status != "healthy" {
		t.Errorf("PG status = %q, 期望 \"healthy\"", resp.Snapshot.PostgreSQL.Status)
	}
	if resp.Snapshot.Elasticsearch.Status != "healthy" {
		t.Errorf("ES status = %q, 期望 \"healthy\"", resp.Snapshot.Elasticsearch.Status)
	}
	if resp.Snapshot.PendingJobs != 3 {
		t.Errorf("pending jobs = %d, 期望 3", resp.Snapshot.PendingJobs)
	}
	if resp.Snapshot.FailedJobs != 1 {
		t.Errorf("failed jobs = %d, 期望 1", resp.Snapshot.FailedJobs)
	}
	if resp.Snapshot.ActiveProfile.ID != "profile-1" {
		t.Errorf("active profile ID = %q, 期望 \"profile-1\"", resp.Snapshot.ActiveProfile.ID)
	}
}

// TestReadyz_PGUnavailable 验证 PG 不可用时返回 503。
func TestReadyz_PGUnavailable(t *testing.T) {
	collector := NewSnapshotCollector(
		&mockPGChecker{err: errors.New("connection refused")},
		&mockESChecker{err: nil},
		&mockEmbeddingChecker{available: true},
		&mockRerankerChecker{available: true},
		&mockJobStatsQuery{},
		&mockProfileQuery{},
	)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	ReadyHandler(collector)(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz PG 不可用时状态码 = %d, 期望 503", rr.Code)
	}

	var resp struct {
		Status   string   `json:"status"`
		Snapshot Snapshot `json:"checks"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("解析 readyz 响应失败: %v", err)
	}
	if resp.Status != "not_ready" {
		t.Errorf("readyz status = %q, 期望 \"not_ready\"", resp.Status)
	}
	if resp.Snapshot.PostgreSQL.Status != "unavailable" {
		t.Errorf("PG status = %q, 期望 \"unavailable\"", resp.Snapshot.PostgreSQL.Status)
	}
}

// TestReadyz_ESDegraded 验证 ES 不可用时返回 200 但标注 degraded。
func TestReadyz_ESDegraded(t *testing.T) {
	collector := NewSnapshotCollector(
		&mockPGChecker{err: nil},
		&mockESChecker{err: errors.New("ES connection refused")},
		&mockEmbeddingChecker{available: true},
		&mockRerankerChecker{available: true},
		&mockJobStatsQuery{},
		&mockProfileQuery{},
	)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	ReadyHandler(collector)(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("readyz ES 降级时状态码 = %d, 期望 200", rr.Code)
	}

	var resp struct {
		Status   string   `json:"status"`
		Snapshot Snapshot `json:"checks"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("解析 readyz 响应失败: %v", err)
	}
	if resp.Status != "degraded" {
		t.Errorf("readyz status = %q, 期望 \"degraded\"", resp.Status)
	}
	if resp.Snapshot.Elasticsearch.Status != "degraded" {
		t.Errorf("ES status = %q, 期望 \"degraded\"", resp.Snapshot.Elasticsearch.Status)
	}
}

// TestReadyz_EmbeddingDegraded 验证 Embedding 不可用时返回 200 但标注 degraded。
func TestReadyz_EmbeddingDegraded(t *testing.T) {
	collector := NewSnapshotCollector(
		&mockPGChecker{err: nil},
		&mockESChecker{err: nil},
		&mockEmbeddingChecker{available: false},
		&mockRerankerChecker{available: true},
		&mockJobStatsQuery{},
		&mockProfileQuery{},
	)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	ReadyHandler(collector)(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("readyz Embedding 降级时状态码 = %d, 期望 200", rr.Code)
	}

	var resp struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("解析 readyz 响应失败: %v", err)
	}
	if resp.Status != "degraded" {
		t.Errorf("readyz status = %q, 期望 \"degraded\"", resp.Status)
	}
}

// TestReadyz_NilDependencies 验证依赖为 nil 时正确报告 unavailable。
func TestReadyz_NilDependencies(t *testing.T) {
	collector := NewSnapshotCollector(nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	ReadyHandler(collector)(rr, req)

	// PG 为 nil → 不可用 → 503
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz PG 为 nil 时状态码 = %d, 期望 503", rr.Code)
	}

	var resp struct {
		Snapshot Snapshot `json:"checks"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("解析 readyz 响应失败: %v", err)
	}
	if resp.Snapshot.PostgreSQL.Status != "unavailable" {
		t.Errorf("PG 为 nil 时 status = %q, 期望 \"unavailable\"", resp.Snapshot.PostgreSQL.Status)
	}
	if resp.Snapshot.Elasticsearch.Status != "unavailable" {
		t.Errorf("ES 为 nil 时 status = %q, 期望 \"unavailable\"", resp.Snapshot.Elasticsearch.Status)
	}
}

// TestReadyz_NoSecretLeak 验证错误信息不包含 DSN 或凭据。
// 使用占位符标记而非真实敏感值，避免测试输出中出现敏感样例。
func TestReadyz_NoSecretLeak(t *testing.T) {
	// 使用 <SENSITIVE_MARK> 占位符代替真实密码/凭据
	// 测试验证脱敏后响应不含占位符，而非真实敏感值
	pgErr := errors.New("connection to host=postgres <SENSITIVE_MARK> user=admin failed")
	esErr := errors.New("ES at http://<CRED_MARK>@elasticsearch:9200 failed")

	collector := NewSnapshotCollector(
		&mockPGChecker{err: pgErr},
		&mockESChecker{err: esErr},
		&mockEmbeddingChecker{available: false},
		&mockRerankerChecker{available: false},
		&mockJobStatsQuery{},
		&mockProfileQuery{},
	)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	ReadyHandler(collector)(rr, req)

	body := rr.Body.String()
	if strings.Contains(body, "<SENSITIVE_MARK>") {
		t.Error("readyz 响应泄漏了 PG 敏感标记")
	}
	if strings.Contains(body, "<CRED_MARK>") {
		t.Error("readyz 响应泄漏了 ES 凭据标记")
	}
}

// TestReadyz_ProfileError 验证 profile 查询失败时不影响 ready 状态。
func TestReadyz_ProfileError(t *testing.T) {
	collector := NewSnapshotCollector(
		&mockPGChecker{err: nil},
		&mockESChecker{err: nil},
		&mockEmbeddingChecker{available: true},
		&mockRerankerChecker{available: true},
		&mockJobStatsQuery{},
		&mockProfileQuery{err: errors.New("profile query failed")},
	)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	ReadyHandler(collector)(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("readyz profile 查询失败时状态码 = %d, 期望 200", rr.Code)
	}

	var resp struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("解析 readyz 响应失败: %v", err)
	}
	if resp.Status != "ready" {
		t.Errorf("readyz status = %q, 期望 \"ready\"", resp.Status)
	}
}

// TestReadyz_JobStatsError 验证 job 统计查询失败时不影响 ready 状态。
func TestReadyz_JobStatsError(t *testing.T) {
	collector := NewSnapshotCollector(
		&mockPGChecker{err: nil},
		&mockESChecker{err: nil},
		&mockEmbeddingChecker{available: true},
		&mockRerankerChecker{available: true},
		&mockJobStatsQuery{err: errors.New("job stats query failed")},
		&mockProfileQuery{id: "p1", name: "default"},
	)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()

	ReadyHandler(collector)(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("readyz job 统计失败时状态码 = %d, 期望 200", rr.Code)
	}
}

// TestReadyz_MethodNotAllowed 验证非 GET 请求返回 405。
func TestReadyz_MethodNotAllowed(t *testing.T) {
	collector := NewSnapshotCollector(nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/readyz", nil)
	rr := httptest.NewRecorder()

	ReadyHandler(collector)(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("readyz POST 状态码 = %d, 期望 405", rr.Code)
	}
}

// TestSnapshot_IsReady 验证 IsReady 逻辑。
func TestSnapshot_IsReady(t *testing.T) {
	tests := []struct {
		name     string
		snapshot Snapshot
		expected bool
	}{
		{
			name:     "PG healthy",
			snapshot: Snapshot{PostgreSQL: ComponentStatus{Status: "healthy"}},
			expected: true,
		},
		{
			name:     "PG unavailable",
			snapshot: Snapshot{PostgreSQL: ComponentStatus{Status: "unavailable"}},
			expected: false,
		},
		{
			name:     "PG degraded (should not happen but test)",
			snapshot: Snapshot{PostgreSQL: ComponentStatus{Status: "degraded"}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.snapshot.IsReady(); got != tt.expected {
				t.Errorf("IsReady() = %v, 期望 %v", got, tt.expected)
			}
		})
	}
}

// TestSafeError 验证 safeError 截断过长错误信息。
func TestSafeError(t *testing.T) {
	// 使用白名单安全模式构造长错误，确保截断生效
	longMsg := "connection refused: " + strings.Repeat("x", 300)
	longErr := errors.New(longMsg)
	safe := safeError(longErr)
	if len(safe) > 203 { // 200 + "..."
		t.Errorf("safeError 未正确截断: len=%d", len(safe))
	}
	if safe == "" {
		t.Error("safeError 不应返回空字符串")
	}
}

// TestSafeError_NilError 验证 nil 错误返回空字符串。
func TestSafeError_NilError(t *testing.T) {
	safe := safeError(nil)
	if safe != "" {
		t.Errorf("safeError(nil) = %q, 期望 \"\"", safe)
	}
}

// TestSafeError_WhitelistSafePatterns 验证白名单内的安全错误模式可以透传。
func TestSafeError_WhitelistSafePatterns(t *testing.T) {
	tests := []struct {
		name    string
		errMsg  string
		wantOut string // 期望透传的错误消息子串
	}{
		{"connection refused", "connection refused", "connection refused"},
		{"context deadline exceeded", "context deadline exceeded", "context deadline exceeded"},
		{"EOF", "EOF", "EOF"},
		{"no such host", "dial tcp: lookup postgres: no such host", "no such host"},
		{"timeout", "i/o timeout", "timeout"},
		{"connection reset", "connection reset by peer", "connection reset"},
		{"中文-未配置", "PG 连接未配置", "未配置"},
		{"中文-为nil", "数据库连接为 nil", "为 nil"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			safe := safeError(errors.New(tt.errMsg))
			if !containsCI(safe, tt.wantOut) {
				t.Errorf("safeError(%q) = %q, 期望包含 %q", tt.errMsg, safe, tt.wantOut)
			}
		})
	}
}

// TestSafeError_UnknownErrorsSanitized 验证不在白名单中的未知错误被脱敏。
func TestSafeError_UnknownErrorsSanitized(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"未知错误1", "something unexpected happened in the database layer"},
		{"未知错误2", "query returned unexpected column type"},
		{"未知错误3", "driver returned non-standard response code 42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			safe := safeError(errors.New(tt.errMsg))
			if safe != "连接失败（详情见服务日志）" {
				t.Errorf("safeError(%q) = %q, 期望脱敏为通用消息", tt.errMsg, safe)
			}
		})
	}
}

// TestSafeError_SensitivePatternsBlocked 验证已知敏感模式被拦截。
// 使用占位符标记而非真实敏感值。
func TestSafeError_SensitivePatternsBlocked(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"password", "connection failed: password=<MARK>"},
		{"token", "auth failed: token=<MARK>"},
		{"authorization", "authorization: <MARK>"},
		{"url_with_creds", "failed to connect to postgres://<MARK>@host:5432"},
		{"secret", "secret=<MARK> in config"},
		{"credential", "credential=<MARK> rejected"},
		{"api_key", "api_key=<MARK> invalid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			safe := safeError(errors.New(tt.errMsg))
			if strings.Contains(safe, "<MARK>") {
				t.Errorf("safeError(%q) 泄漏了敏感标记: %q", tt.errMsg, safe)
			}
			if safe != "连接失败（详情见服务日志）" {
				t.Errorf("safeError(%q) = %q, 期望通用脱敏消息", tt.errMsg, safe)
			}
		})
	}
}

// 确保未使用的 import 不会导致编译错误
var _ = sql.ErrNoRows

// --- Provider 超时策略相关测试替身 ---
// 引入动机：线上事故是 embedding/reranker 健康检查的 5s 写死超时误报 degraded。
// 需要用可阻塞、可观测 ctx deadline 的替身真实驱动 Collect，而不是字符串包含式断言。

// deadlineRecorder 记录检查器观察到的 ctx 剩余超时。
// 引入动机：直接读取 ctx deadline 即可断言超时策略，无需真的等待 30s（或 5s）。
type deadlineRecorder struct {
	// hasDeadline 标记传入的 ctx 是否带 deadline。
	hasDeadline bool
	// remaining 是 record 调用时刻距离 deadline 的剩余时间。
	remaining time.Duration
}

// record 记录 ctx 的 deadline 剩余时间；无 deadline 时仅标记有调用发生。
func (r *deadlineRecorder) record(ctx context.Context) {
	dl, ok := ctx.Deadline()
	r.hasDeadline = ok
	if ok {
		r.remaining = time.Until(dl)
	}
}

// blockingProviderChecker 同时实现 EmbeddingChecker 与 RerankerChecker。
// 引入动机：用同一个替身模拟两类真实推理检查——记录健康检查传入的 ctx deadline，
// 并按 block 时长阻塞；阻塞期间尊重 ctx 取消，与真实 Provider 的 Available 行为一致
// （真实实现调用推理 API，ctx 到期即返回 false）。
type blockingProviderChecker struct {
	// block 模拟推理耗时的阻塞时长；<=0 表示立即返回可用。
	block time.Duration
	// rec 记录观察到的 ctx deadline。
	rec deadlineRecorder
}

// Available 模拟 Provider 可用性检查：阻塞 block 后返回可用；
// 若阻塞期间 ctx 到期则返回不可用（等价于真实推理请求超时）。
func (b *blockingProviderChecker) Available(ctx context.Context) bool {
	b.rec.record(ctx)
	if b.block <= 0 {
		return true
	}
	timer := time.NewTimer(b.block)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// blockingPGChecker 记录 PG 检查收到的 ctx deadline，并可按 block 阻塞。
// 引入动机：验证快检查（5s）语义未受 provider 超时改动影响。
type blockingPGChecker struct {
	// block 模拟 PG Ping 耗时；<=0 表示立即返回成功。
	block time.Duration
	// rec 记录观察到的 ctx deadline。
	rec deadlineRecorder
}

// PingContext 模拟 PG 连通性检查。
func (m *blockingPGChecker) PingContext(ctx context.Context) error {
	m.rec.record(ctx)
	return blockUntil(ctx, m.block)
}

// blockingESChecker 记录 ES 检查收到的 ctx deadline，并可按 block 阻塞。
// 引入动机：验证快检查（5s）语义未受 provider 超时改动影响。
type blockingESChecker struct {
	// block 模拟 ES Ping 耗时；<=0 表示立即返回成功。
	block time.Duration
	// rec 记录观察到的 ctx deadline。
	rec deadlineRecorder
}

// Ping 模拟 ES 连通性检查。
func (m *blockingESChecker) Ping(ctx context.Context) error {
	m.rec.record(ctx)
	return blockUntil(ctx, m.block)
}

// blockUntil 阻塞 d 时长；d<=0 立即返回 nil，阻塞期间 ctx 到期则返回 ctx.Err()。
func blockUntil(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// assertDeadline 断言检查器观察到的 ctx 剩余超时落在 (lowerBound, upperBound] 区间内。
// 引入动机：/readyz 的超时是相对当前时刻的 deadline，用区间断言既容忍调度抖动，
// 又能区分 5s 与 30s、200ms 这些量级不同的策略。
func assertDeadline(t *testing.T, name string, rec deadlineRecorder, lowerBound, upperBound time.Duration) {
	t.Helper()
	if !rec.hasDeadline {
		t.Fatalf("%s 检查收到的 ctx 没有 deadline", name)
	}
	if rec.remaining <= lowerBound || rec.remaining > upperBound {
		t.Errorf("%s 检查超时 = %v, 期望落在 (%v, %v]", name, rec.remaining, lowerBound, upperBound)
	}
}

// TestCollector_DefaultProviderTimeoutIs30s 验证不传选项时的默认超时策略：
// embedding/reranker 使用 DefaultProviderCheckTimeout（30s），PG/ES 仍使用 5s 快检查超时。
//
// 引入动机：线上事故根因是 Provider 检查被写死 5s。本用例直接读取各检查器收到的 ctx deadline，
// 断言 Provider 是 30s 而非 5s —— 无需真的等待 30s，可稳定、快速地回归该事故。
func TestCollector_DefaultProviderTimeoutIs30s(t *testing.T) {
	if DefaultProviderCheckTimeout != 30*time.Second {
		t.Fatalf("DefaultProviderCheckTimeout = %v, 期望 30s", DefaultProviderCheckTimeout)
	}

	pg := &blockingPGChecker{}
	es := &blockingESChecker{}
	emb := &blockingProviderChecker{}
	rr := &blockingProviderChecker{}

	snap := NewSnapshotCollector(pg, es, emb, rr, nil, nil).Collect(context.Background())

	assertDeadline(t, "PG", pg.rec, 4*time.Second, defaultCheckTimeout)
	assertDeadline(t, "ES", es.rec, 4*time.Second, defaultCheckTimeout)
	assertDeadline(t, "Embedding", emb.rec, 25*time.Second, DefaultProviderCheckTimeout)
	assertDeadline(t, "Reranker", rr.rec, 25*time.Second, DefaultProviderCheckTimeout)

	if snap.Embedding.Status != "healthy" {
		t.Errorf("默认超时下 Embedding 状态 = %q, 期望 \"healthy\"", snap.Embedding.Status)
	}
	if snap.Reranker.Status != "healthy" {
		t.Errorf("默认超时下 Reranker 状态 = %q, 期望 \"healthy\"", snap.Reranker.Status)
	}
	if !snap.IsReady() {
		t.Error("PG 健康时 IsReady 应为 true")
	}
}

// TestCollector_ProviderTimeoutInjectedRespected 验证注入的 Provider 超时真正决定
// embedding/reranker 检查的预算，且不影响 PG/ES 的 5s 快检查语义。
func TestCollector_ProviderTimeoutInjectedRespected(t *testing.T) {
	const providerTimeout = 200 * time.Millisecond

	// 子用例 1：阻塞时长小于 Provider 超时 → 检查通过，说明预算未被压到更短。
	t.Run("provider 阻塞在超时内则 healthy", func(t *testing.T) {
		emb := &blockingProviderChecker{block: 50 * time.Millisecond}
		rr := &blockingProviderChecker{block: 50 * time.Millisecond}

		snap := NewSnapshotCollector(&blockingPGChecker{}, &blockingESChecker{}, emb, rr, nil, nil,
			WithProviderCheckTimeout(providerTimeout)).Collect(context.Background())

		if snap.Embedding.Status != "healthy" || snap.Reranker.Status != "healthy" {
			t.Errorf("阻塞 50ms（< 200ms）状态 = %q/%q, 期望均 healthy",
				snap.Embedding.Status, snap.Reranker.Status)
		}
		assertDeadline(t, "Embedding", emb.rec, 150*time.Millisecond, providerTimeout)
		assertDeadline(t, "Reranker", rr.rec, 150*time.Millisecond, providerTimeout)
	})

	// 子用例 2：阻塞时长超过 Provider 超时 → 降级，且耗时等于 Provider 超时而非 5s 快检查超时。
	t.Run("provider 阻塞超过超时则 degraded 且按 provider 超时截止", func(t *testing.T) {
		// 阻塞 1s：若仍沿用 5s 快检查超时，检查会在 1s 后返回 healthy；实际应在 200ms 截止降级。
		emb := &blockingProviderChecker{block: time.Second}
		rr := &blockingProviderChecker{block: time.Second}

		start := time.Now()
		snap := NewSnapshotCollector(&blockingPGChecker{}, &blockingESChecker{}, emb, rr, nil, nil,
			WithProviderCheckTimeout(providerTimeout)).Collect(context.Background())
		elapsed := time.Since(start)

		if snap.Embedding.Status != "degraded" || snap.Reranker.Status != "degraded" {
			t.Errorf("阻塞 1s（> 200ms）状态 = %q/%q, 期望均 degraded",
				snap.Embedding.Status, snap.Reranker.Status)
		}
		if elapsed < providerTimeout {
			t.Errorf("检查耗时 = %v, 不应短于 provider 超时 %v", elapsed, providerTimeout)
		}
		if elapsed >= time.Second {
			t.Errorf("检查耗时 = %v, 说明未按 provider 超时取消失常，可能退回了快检查超时", elapsed)
		}
		if !snap.IsReady() {
			t.Error("Provider 降级不应影响 PG 就绪判定")
		}
	})

	// 子用例 3：PG/ES 阻塞超过 Provider 超时仍 healthy，证明快检查保持 5s。
	t.Run("PG/ES 阻塞超过 provider 超时仍用 5s", func(t *testing.T) {
		pg := &blockingPGChecker{block: 400 * time.Millisecond}
		es := &blockingESChecker{block: 400 * time.Millisecond}

		snap := NewSnapshotCollector(pg, es, &blockingProviderChecker{}, &blockingProviderChecker{}, nil, nil,
			WithProviderCheckTimeout(providerTimeout)).Collect(context.Background())

		if snap.PostgreSQL.Status != "healthy" || snap.Elasticsearch.Status != "healthy" {
			t.Errorf("PG/ES 阻塞 400ms（> provider 200ms）状态 = %q/%q, 期望均 healthy（快检查仍为 5s）",
				snap.PostgreSQL.Status, snap.Elasticsearch.Status)
		}
		assertDeadline(t, "PG", pg.rec, 4*time.Second, defaultCheckTimeout)
		assertDeadline(t, "ES", es.rec, 4*time.Second, defaultCheckTimeout)
	})
}

// TestCollector_SlowProviderNotDegradedUnderDefaultConfig 是本次线上事故的端到端回归测试。
//
// 引入动机：故障现象是"慢但可用"的 reranker 被判为 degraded 并刷
// "reranker API 调用失败，将降级为 RRF 结果 ... context deadline exceeded"。
// 用例让 RerankerChecker.Available 真实阻塞 6s（超过修复前的写死 5s，但远小于默认 30s），
// 断言默认配置下该检查仍为 healthy。
// 用例真实阻塞 6s，故在 go test -short 下跳过，便于本地快速回归。
// 只阻塞 Reranker：事故日志正是 reranker 的 "context deadline exceeded" 刷屏，聚焦该路径即可；
// Embedding 走的是完全相同的超时路径，已由 TestCollector_DefaultProviderTimeoutIs30s
// （deadline 断言）与 TestCollector_ProviderTimeoutInjectedRespected（超时生效）覆盖。
// 注：Collect 已并发执行，两个 Provider 同时阻塞也不会叠加耗时。
func TestCollector_SlowProviderNotDegradedUnderDefaultConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("阻塞 6s 的线上事故回归用例在 -short 模式下跳过")
	}

	const slow = 6 * time.Second
	rr := &blockingProviderChecker{block: slow}

	start := time.Now()
	snap := NewSnapshotCollector(&blockingPGChecker{}, &blockingESChecker{}, &blockingProviderChecker{}, rr, nil, nil).
		Collect(context.Background())
	elapsed := time.Since(start)

	if snap.Reranker.Status != "healthy" {
		t.Errorf("6s 慢 Reranker 状态 = %q, 期望 \"healthy\"（默认 30s 超时）；"+
			"修复前写死 5s 时此处为 degraded", snap.Reranker.Status)
	}
	if snap.Embedding.Status != "healthy" {
		t.Errorf("Embedding 状态 = %q, 期望 \"healthy\"", snap.Embedding.Status)
	}
	if elapsed < slow {
		t.Errorf("检查耗时 = %v, 期望 >= 阻塞时长 %v", elapsed, slow)
	}
	if elapsed >= DefaultProviderCheckTimeout {
		t.Errorf("检查耗时 = %v, 不应达到默认超时 %v", elapsed, DefaultProviderCheckTimeout)
	}
}

// TestWithProviderCheckTimeout_InvalidPanics 验证非法选项 fail fast，不静默回退默认值。
func TestWithProviderCheckTimeout_InvalidPanics(t *testing.T) {
	tests := []struct {
		name string
		opts []SnapshotCollectorOption
	}{
		{name: "零值", opts: []SnapshotCollectorOption{WithProviderCheckTimeout(0)}},
		{name: "负值", opts: []SnapshotCollectorOption{WithProviderCheckTimeout(-time.Second)}},
		{name: "nil 选项", opts: []SnapshotCollectorOption{nil}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("非法超时选项未 panic（fail fast 失效）")
				}
			}()
			NewSnapshotCollector(nil, nil, nil, nil, nil, nil, tt.opts...)
		})
	}
}

// --- 并发采集相关测试替身 ---
// 引入动机：Collect 由顺序改为并发后，需要真实驱动"所有检查同时阻塞"的场景，
// 证明总耗时等于最长单项而不是各项之和。以下替身可阻塞且记录所观察到的 ctx deadline。

// blockingJobStatsQuery 模拟 job 统计查询：记录 ctx deadline 并按 block 阻塞。
// 引入动机：job 统计在同一 goroutine 内串行执行两次查询，
// 只在第一次查询阻塞即可让整项检查耗时等于一次 block，便于断言"单项最长"。
type blockingJobStatsQuery struct {
	// block 模拟单次统计查询耗时；<=0 表示立即返回。
	block time.Duration
	// rec 记录观察到的 ctx deadline。
	rec deadlineRecorder
	// calls 累计查询次数，仅由 job 统计所在 goroutine 串行写入，不涉及并发共享。
	calls int
}

// CountByStatus 模拟 job 计数查询，仅第一次调用执行阻塞。
func (m *blockingJobStatsQuery) CountByStatus(ctx context.Context, status string) (int, error) {
	m.calls++
	m.rec.record(ctx)
	if m.block > 0 && m.calls == 1 {
		if err := blockUntil(ctx, m.block); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

// blockingProfileQuery 模拟 active profile 查询：记录 ctx deadline 并按 block 阻塞。
type blockingProfileQuery struct {
	// block 模拟 profile 查询耗时；<=0 表示立即返回。
	block time.Duration
	// rec 记录观察到的 ctx deadline。
	rec deadlineRecorder
}

// GetActiveProfileID 模拟 active profile 查询。
func (m *blockingProfileQuery) GetActiveProfileID(ctx context.Context) (string, string, error) {
	m.rec.record(ctx)
	if err := blockUntil(ctx, m.block); err != nil {
		return "", "", err
	}
	return "profile-1", "default", nil
}

// TestCollector_ChecksRunConcurrently 验证 Collect 的各依赖检查真正并发执行：
// 六个检查各自阻塞 200ms 时，总耗时约等于最长单项（200ms），而不是各项之和（1200ms）。
//
// 引入动机：顺序采集的最坏耗时是各项超时之和（80s），会被 docker healthcheck 的 10s timeout
// 先杀掉并误判容器 unhealthy。本用例用可观测的阻塞时长直接证明并发化生效——
// 若回退到顺序执行，总耗时将 >= 1200ms，必然超过 400ms 的上界断言。
func TestCollector_ChecksRunConcurrently(t *testing.T) {
	const (
		block = 200 * time.Millisecond
		// providerBudget 必须大于 block，否则 Provider 检查会先按超时降级，
		// 无法验证"阻塞在预算内仍 healthy"。
		providerBudget = 2 * time.Second
	)

	pg := &blockingPGChecker{block: block}
	es := &blockingESChecker{block: block}
	emb := &blockingProviderChecker{block: block}
	rr := &blockingProviderChecker{block: block}
	jobs := &blockingJobStatsQuery{block: block}
	prof := &blockingProfileQuery{block: block}

	start := time.Now()
	snap := NewSnapshotCollector(pg, es, emb, rr, jobs, prof,
		WithProviderCheckTimeout(providerBudget)).Collect(context.Background())
	elapsed := time.Since(start)

	sequentialTotal := 6 * block
	if elapsed >= 400*time.Millisecond {
		t.Errorf("Collect 耗时 = %v, 期望约等于最长单项 %v（并发执行）；"+
			"顺序执行应约为各项之和 %v", elapsed, block, sequentialTotal)
	}
	if elapsed < block {
		t.Errorf("Collect 耗时 = %v, 不应短于最长单项 %v", elapsed, block)
	}

	// 并发不应破坏各组件的状态分类：全部在预算内，应均为 healthy。
	if snap.PostgreSQL.Status != "healthy" {
		t.Errorf("PG status = %q, 期望 \"healthy\"", snap.PostgreSQL.Status)
	}
	if snap.Elasticsearch.Status != "healthy" {
		t.Errorf("ES status = %q, 期望 \"healthy\"", snap.Elasticsearch.Status)
	}
	if snap.Embedding.Status != "healthy" {
		t.Errorf("Embedding status = %q, 期望 \"healthy\"", snap.Embedding.Status)
	}
	if snap.Reranker.Status != "healthy" {
		t.Errorf("Reranker status = %q, 期望 \"healthy\"", snap.Reranker.Status)
	}
	if snap.ActiveProfile.ID != "profile-1" {
		t.Errorf("active profile ID = %q, 期望 \"profile-1\"", snap.ActiveProfile.ID)
	}
	if !snap.IsReady() {
		t.Error("全部健康时 IsReady 应为 true")
	}

	// 各检查仍必须使用自己的超时预算，不能因并发而改变超时语义。
	assertDeadline(t, "PG", pg.rec, 4*time.Second, defaultCheckTimeout)
	assertDeadline(t, "ES", es.rec, 4*time.Second, defaultCheckTimeout)
	assertDeadline(t, "Embedding", emb.rec, providerBudget-100*time.Millisecond, providerBudget)
	assertDeadline(t, "Reranker", rr.rec, providerBudget-100*time.Millisecond, providerBudget)
	assertDeadline(t, "JobStats", jobs.rec, 4*time.Second, defaultCheckTimeout)
	assertDeadline(t, "Profile", prof.rec, 4*time.Second, defaultCheckTimeout)
}

// TestCollector_ConcurrentMixedStatusClassification 验证并发采集下各组件状态仍被正确分类：
// PG 失败 → unavailable（服务不可用）；ES 失败 → degraded；慢 Embedding 按 provider 超时 → degraded；
// Reranker 可用 → healthy，且 PG 不可用优先决定 IsReady。
//
// 引入动机：并发后用时最长的检查（此处是超时的 Embedding）不得覆盖或污染其他组件的结果，
// 同时 /readyz 的对外语义（PG 失败 503、其余 degraded 仍 200）必须保持不变。
func TestCollector_ConcurrentMixedStatusClassification(t *testing.T) {
	const providerBudget = 100 * time.Millisecond

	// PG/ES 立即失败，Embedding 阻塞 500ms（远超 providerBudget）→ 按超时降级，Reranker 立即可用。
	emb := &blockingProviderChecker{block: 500 * time.Millisecond}

	start := time.Now()
	snap := NewSnapshotCollector(
		&mockPGChecker{err: errors.New("connection refused")},
		&mockESChecker{err: errors.New("ES connection refused")},
		emb,
		&mockRerankerChecker{available: true},
		&mockJobStatsQuery{},
		&mockProfileQuery{},
		WithProviderCheckTimeout(providerBudget),
	).Collect(context.Background())
	elapsed := time.Since(start)

	if snap.PostgreSQL.Status != "unavailable" {
		t.Errorf("PG status = %q, 期望 \"unavailable\"", snap.PostgreSQL.Status)
	}
	if snap.Elasticsearch.Status != "degraded" {
		t.Errorf("ES status = %q, 期望 \"degraded\"", snap.Elasticsearch.Status)
	}
	if snap.Embedding.Status != "degraded" {
		t.Errorf("Embedding status = %q, 期望 \"degraded\"（阻塞超过 %v 的 provider 预算）",
			snap.Embedding.Status, providerBudget)
	}
	if snap.Reranker.Status != "healthy" {
		t.Errorf("Reranker status = %q, 期望 \"healthy\"（不受其他检查影响）", snap.Reranker.Status)
	}
	if snap.IsReady() {
		t.Error("PG 不可用时 IsReady 应为 false（/readyz 语义：503）")
	}

	// 总耗时由超时的 Embedding 决定（约 providerBudget），而不是等待 500ms 的阻塞。
	if elapsed >= 500*time.Millisecond {
		t.Errorf("Collect 耗时 = %v, 不应等待 Embedding 的 500ms 阻塞（应按 provider 超时取消）", elapsed)
	}
}

