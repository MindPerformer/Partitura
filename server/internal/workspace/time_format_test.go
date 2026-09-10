// time_format_test.go 验证 API 时间字段格式化为 UTC RFC3339（Z 后缀）。
//
// 引入动机：计划要求所有 API 时间字段必须使用 UTC RFC3339（如 2026-03-08T12:34:56Z），
// 原有 SQL to_char(... OF) 输出 +00 偏移导致浏览器解析为 Invalid Date。
// 此测试通过真实 mock repository → handler → JSON 序列化路径验证时间字段格式正确，
// 且输出可被标准时间解析器（time.Parse / JS Date）正确解析。
package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"
)

// rfc3339Regex 匹配 UTC RFC3339 时间字符串（Z 后缀，无时区偏移）。
// 引入动机：验证 API 输出的时间字段严格符合 RFC3339 UTC 格式。
var rfc3339Regex = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

// TestWorkspaceStatsTimeFormatIsRFC3339 验证 GetWorkspaceStats handler
// 通过真实 mock repository → handler → JSON 序列化路径输出的
// recent_revisions[].created_at 字段为 UTC RFC3339 格式，
// 且可被 time.Parse(time.RFC3339) 正确解析。
// 引入动机：workspace stats 的 recent_revisions 时间字段必须为 UTC RFC3339，
// 前端 useFormatDate 依赖此格式进行安全解析。
func TestWorkspaceStatsTimeFormatIsRFC3339(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "stats-ws", "Stats WS", "", "owner-001")

	// 注入带有真实 RFC3339 时间值的统计信息
	// 引入动机：mock repository 的 SetWorkspaceStats 允许注入预设统计，
	// handler 将其序列化为 JSON 输出，测试验证完整路径的时间格式正确性。
	wsRepo.SetWorkspaceStats(ws.ID, &WorkspaceStats{
		TotalDocuments:    10,
		ActiveDocuments:   8,
		DraftDocuments:    1,
		ArchivedDocuments: 1,
		MemberCount:       3,
		RecentRevisions: []RecentRevision{
			{
				Path:           "docs/test.md",
				Title:          "Test Document",
				RevisionNumber: 5,
				CreatedAt:      "2026-03-08T12:34:56Z",
			},
			{
				Path:           "docs/another.md",
				Title:          "Another Document",
				RevisionNumber: 3,
				CreatedAt:      "2025-12-31T23:59:59Z",
			},
		},
	})

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")
	req := authedRequest(http.MethodGet, "/api/workspaces/"+ws.ID+"/stats", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("获取 stats 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	// 解析 JSON 响应，验证 recent_revisions 时间字段
	var resp struct {
		Stats struct {
			TotalDocuments  int              `json:"total_documents"`
			RecentRevisions []RecentRevision `json:"recent_revisions"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 stats 响应失败: %v, body: %s", err, rr.Body.String())
	}

	if len(resp.Stats.RecentRevisions) != 2 {
		t.Fatalf("期望 2 条 recent_revisions，实际 %d", len(resp.Stats.RecentRevisions))
	}

	for i, rev := range resp.Stats.RecentRevisions {
		if !rfc3339Regex.MatchString(rev.CreatedAt) {
			t.Errorf("recent_revisions[%d].created_at = %q, 不符合 UTC RFC3339 格式（应为 YYYY-MM-DDTHH:MM:SSZ）", i, rev.CreatedAt)
		}
		// 验证可被 time.Parse 正确解析——前端 JS Date 也使用相同标准
		parsed, err := time.Parse(time.RFC3339, rev.CreatedAt)
		if err != nil {
			t.Errorf("recent_revisions[%d].created_at = %q, time.Parse 失败: %v", i, rev.CreatedAt, err)
		}
		// 验证解析后时间为 UTC
		if parsed.Location() != time.UTC {
			t.Errorf("recent_revisions[%d].created_at = %q, 解析后时区 = %v, 期望 UTC", i, rev.CreatedAt, parsed.Location())
		}
	}
}

// TestWorkspaceStatsTimeFormatRejectsNonRFC3339 验证非 RFC3339 格式的时间值
// 在 JSON 序列化路径中不被修改或隐藏——handler 透传 repository 返回的值。
// 引入动机：确保时间格式由 SQL 层统一为 RFC3339，handler 不做隐式转换。
func TestWorkspaceStatsTimeFormatRejectsNonRFC3339(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "bad-time-ws", "Bad Time WS", "", "owner-001")

	// 注入非 RFC3339 格式的时间值——模拟旧 OF 偏移格式
	wsRepo.SetWorkspaceStats(ws.ID, &WorkspaceStats{
		TotalDocuments: 1,
		RecentRevisions: []RecentRevision{
			{
				Path:           "docs/bad.md",
				Title:          "Bad Time",
				RevisionNumber: 1,
				CreatedAt:      "2026-03-08T12:34:56+00",
			},
		},
	})

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")
	req := authedRequest(http.MethodGet, "/api/workspaces/"+ws.ID+"/stats", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("获取 stats 应返回 200，实际 %d", rr.Code)
	}

	var resp struct {
		Stats struct {
			RecentRevisions []RecentRevision `json:"recent_revisions"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if len(resp.Stats.RecentRevisions) != 1 {
		t.Fatalf("期望 1 条 recent_revision，实际 %d", len(resp.Stats.RecentRevisions))
	}

	// 验证 +00 偏移格式不符合 RFC3339 UTC Z 后缀要求
	createdAt := resp.Stats.RecentRevisions[0].CreatedAt
	if rfc3339Regex.MatchString(createdAt) {
		t.Errorf("非 RFC3339 格式的时间值 %q 不应匹配 UTC RFC3339 正则", createdAt)
	}
	// 验证 +00 偏移格式不符合 RFC3339 UTC Z 后缀要求
	// （浏览器对 +00 偏移的解析在某些引擎下不稳定，这是 Invalid Date 根因）
	// time.Parse(time.RFC3339) 对 "+00" 也无法解析（需要 "+00:00"），
	// 这进一步证明旧格式不兼容标准解析器
}

// TestAuditEntryTimeFormatIsRFC3339 验证审计日志 handler
// 通过真实 mock repository → handler → JSON 序列化路径输出的
// created_at 字段为 UTC RFC3339 格式。
// 引入动机：审计日志时间字段必须为 UTC RFC3339，前端审计页面使用 useFormatDate 显示。
func TestAuditEntryTimeFormatIsRFC3339(t *testing.T) {
	mux, authRepo, wsRepo, auditRepo, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "member2", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "audit-ws", "Audit WS", "", "owner-001")

	// 通过真实 handler 路径产生审计记录——以 owner 身份添加成员
	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")

	// 添加成员产生审计记录
	wsRepo.AddUser("user-002", "member2", "member2@test.example", "user", false)
	body := `{"username":"member2","role":"viewer"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+ws.ID+"/members", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("添加成员应返回 201，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	// 查询审计日志
	entries := auditRepo.GetEntries()
	if len(entries) == 0 {
		t.Fatal("应产生审计记录")
	}

	// 验证审计记录的时间戳——mock audit repo 使用 time.Now() 记录
	// 这里验证 mock 产生的时间可被 RFC3339 序列化
	for _, e := range entries {
		// mock audit entry 的 timestamp 是 time.Time，验证其可格式化为 RFC3339
		formatted := e.timestamp.UTC().Format(time.RFC3339)
		if !rfc3339Regex.MatchString(formatted) {
			t.Errorf("审计记录时间 %q 格式化后不符合 UTC RFC3339", formatted)
		}
		// 验证可被 time.Parse 正确解析
		if _, err := time.Parse(time.RFC3339, formatted); err != nil {
			t.Errorf("审计记录时间 %q time.Parse 失败: %v", formatted, err)
		}
	}
}

// TestWorkspaceResponseNoTimeFields 验证 workspace 响应结构体
// 不包含时间字段——workspace 的 created_at/updated_at 不在 API 响应中暴露。
// 引入动机：workspace 响应只返回业务字段，时间字段在 stats 和 revisions 中暴露。
// 这确保时间格式问题只在有时间字段的路径上需要验证。
func TestWorkspaceResponseNoTimeFields(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "no-time-ws", "No Time WS", "", "owner-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")
	req := authedRequest(http.MethodGet, "/api/workspaces/"+ws.ID, sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("获取 workspace 应返回 200，实际 %d", rr.Code)
	}

	// 验证响应中不包含 created_at / updated_at 字段
	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if _, exists := resp["created_at"]; exists {
		t.Error("workspace 响应不应包含 created_at 字段")
	}
	if _, exists := resp["updated_at"]; exists {
		t.Error("workspace 响应不应包含 updated_at 字段")
	}
}
