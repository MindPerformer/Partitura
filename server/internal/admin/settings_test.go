// Package admin 测试业务配置管理 handler。
//
// 引入动机：Phase6 WP3 需要验证 allowlist、类型校验、restart_required 和运行时状态不泄密。
package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"partitura/server/internal/auth"
	"partitura/server/internal/config"
	"partitura/server/internal/health"
	"partitura/server/internal/settings"
)

// fakeSettingsRepo 是 settings.Repository 的内存测试实现。
type fakeSettingsRepo struct {
	records []settings.Record
	updated map[string]settings.Record
}

func newFakeSettingsRepo(records []settings.Record) *fakeSettingsRepo {
	return &fakeSettingsRepo{
		records: records,
		updated: make(map[string]settings.Record),
	}
}

func (r *fakeSettingsRepo) ListNonSecret(ctx context.Context) ([]settings.Record, error) {
	return r.records, nil
}

func (r *fakeSettingsRepo) Get(ctx context.Context, key string) (*settings.Record, error) {
	for _, rec := range r.records {
		if rec.Key == key {
			return &rec, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (r *fakeSettingsRepo) Update(ctx context.Context, key, value, valueType, updatedBy string) error {
	r.updated[key] = settings.Record{Key: key, Value: value, ValueType: valueType, UpdatedBy: updatedBy}
	return nil
}

// fakeAuditRepo 是 AuditRepository 的内存测试实现。
type fakeAuditRepo struct {
	calls []struct {
		userID       string
		workspaceID  string
		action       string
		resourceType string
		resourceID   string
		detail       json.RawMessage
		requestID    string
	}
}

func (a *fakeAuditRepo) Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error {
	a.calls = append(a.calls, struct {
		userID       string
		workspaceID  string
		action       string
		resourceType string
		resourceID   string
		detail       json.RawMessage
		requestID    string
	}{userID, workspaceID, action, resourceType, resourceID, detail, requestID})
	return nil
}

func newSettingsTestHandler(records []settings.Record) (*SettingsHandler, *fakeSettingsRepo, *fakeAuditRepo) {
	repo := newFakeSettingsRepo(records)
	audit := &fakeAuditRepo{}
	cfg := &config.Config{
		LogLevel:                "info",
		DBHost:                  "pg.example.com",
		DBName:                  "partitura",
		DBPassword:              "top-secret-pg-password",
		ESURL:                   "http://es:9200",
		EmbeddingBaseURL:        "http://embedding:8000",
		EmbeddingAPIKey:         "sk-embedding-secret",
		EmbeddingModel:          "qwen3-embedding",
		RerankerBaseURL:         "http://reranker:8000",
		RerankerAPIKey:          "sk-reranker-secret",
		RerankerModel:           "qwen3-reranker",
		JobWorkerEnabled:        true,
		SchedulerEnabled:        false,
		CookieSecure:            true,
	}
	healthCollector := health.NewSnapshotCollector(nil, nil, nil, nil, nil, nil)
	return NewSettingsHandler(repo, audit, cfg, healthCollector), repo, audit
}

func mustAuthRequest(t *testing.T, method, path string, body string) *http.Request {
	t.Helper()
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "admin-001", AuthMethod: auth.AuthMethodCookie}))
	return req
}

// TestListSettings 验证 GET /api/admin/config/settings 返回允许的配置项和默认值。
func TestListSettings(t *testing.T) {
	handler, _, _ := newSettingsTestHandler([]settings.Record{
		{Key: "default_embedding_model", Value: "qwen3-embedding", ValueType: "string", Category: "business", Description: "默认 embedding 模型名"},
	})

	req := mustAuthRequest(t, http.MethodGet, "/api/admin/config/settings", "")
	rec := httptest.NewRecorder()
	handler.ListSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望状态码 200，得到 %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Settings []settings.Record `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	found := false
	for _, s := range body.Settings {
		if s.Key == "default_embedding_model" && s.Value == "qwen3-embedding" {
			found = true
		}
		if s.Key == "job_worker_enabled" && s.Value != "true" && s.Value != "false" {
			t.Errorf("job_worker_enabled 应为合法布尔字符串，实际 %q", s.Value)
		}
	}
	if !found {
		t.Error("响应中应包含 default_embedding_model 设置项")
	}
}

// TestUpdateSetting_Success 验证合法更新返回规范化值和 restart_required，并记录审计。
func TestUpdateSetting_Success(t *testing.T) {
	handler, repo, audit := newSettingsTestHandler(nil)

	req := mustAuthRequest(t, http.MethodPut, "/api/admin/config/settings/default_embedding_timeout_seconds", `{"value":"60"}`)
	req.SetPathValue("key", "default_embedding_timeout_seconds")
	rec := httptest.NewRecorder()
	handler.UpdateSetting(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望状态码 200，得到 %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Key             string `json:"key"`
		Value           string `json:"value"`
		RestartRequired bool   `json:"restart_required"`
		Message         string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body.Key != "default_embedding_timeout_seconds" {
		t.Errorf("key 不匹配: %q", body.Key)
	}
	if body.Value != "60" {
		t.Errorf("value 应被规范化为 60，实际 %q", body.Value)
	}
	if !body.RestartRequired {
		t.Error("default_embedding_timeout_seconds 修改后应返回 restart_required=true")
	}

	if got, ok := repo.updated["default_embedding_timeout_seconds"]; !ok || got.Value != "60" {
		t.Errorf("repository 未正确更新: %v", got)
	}
	if len(audit.calls) != 1 {
		t.Errorf("应记录 1 条审计日志，实际 %d", len(audit.calls))
	}
}

// TestUpdateSetting_ForbiddenKey 验证不在 allowlist 的 key 返回 403。
func TestUpdateSetting_ForbiddenKey(t *testing.T) {
	handler, _, _ := newSettingsTestHandler(nil)

	req := mustAuthRequest(t, http.MethodPut, "/api/admin/config/settings/db_password", `{"value":"secret"}`)
	req.SetPathValue("key", "db_password")
	rec := httptest.NewRecorder()
	handler.UpdateSetting(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("期望状态码 403，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUpdateSetting_InvalidValue 验证类型非法的 value 返回 400。
func TestUpdateSetting_InvalidValue(t *testing.T) {
	handler, _, _ := newSettingsTestHandler(nil)

	req := mustAuthRequest(t, http.MethodPut, "/api/admin/config/settings/job_worker_enabled", `{"value":"maybe"}`)
	req.SetPathValue("key", "job_worker_enabled")
	rec := httptest.NewRecorder()
	handler.UpdateSetting(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望状态码 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGetRuntimeStatus_NoSecrets 验证 runtime 状态不暴露 DSN、key、password 等敏感信息。
func TestGetRuntimeStatus_NoSecrets(t *testing.T) {
	handler, _, _ := newSettingsTestHandler(nil)

	req := mustAuthRequest(t, http.MethodGet, "/api/admin/config/runtime", "")
	rec := httptest.NewRecorder()
	handler.GetRuntimeStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望状态码 200，得到 %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, secret := range []string{
		"top-secret-pg-password",
		"sk-embedding-secret",
		"sk-reranker-secret",
		"pg.example.com",
		"http://es:9200",
		"http://embedding:8000",
		"http://reranker:8000",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("runtime 状态不应包含敏感值 %q，body:\n%s", secret, body)
		}
	}
	if !strings.Contains(body, "pg_configured") {
		t.Error("runtime 状态应包含 pg_configured 布尔标志")
	}
	if !strings.Contains(body, "log_level") {
		t.Error("runtime 状态应包含 log_level")
	}
	fmt.Println(body)
}
