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
