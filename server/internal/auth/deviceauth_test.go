// deviceauth_test.go 测试 device authorization 的领域逻辑和 HTTP handler。
//
// 引入动机：Phase6 WP4 要求 token 一次性交换、空 device_name fallback、
// 日志不记录 code/token，以及 completed 终端状态。
package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"partitura/server/internal/audit"
)

// mockDeviceAuthRepo 是 DeviceAuthRepository 的内存 mock。
// 用于验证一次性交换、状态迁移和边界条件，不依赖 PostgreSQL。
type mockDeviceAuthRepo struct {
	mu      sync.Mutex
	records map[string]*DeviceAuthorizationRecord
	nextID  int
}

func newMockDeviceAuthRepo() *mockDeviceAuthRepo {
	return &mockDeviceAuthRepo{records: make(map[string]*DeviceAuthorizationRecord)}
}

func (m *mockDeviceAuthRepo) CreateDeviceAuthorization(ctx context.Context, deviceCode, userCode, deviceName string, expiresAt time.Time, pollInterval int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("auth-%d", m.nextID)
	rec := &DeviceAuthorizationRecord{
		ID:                 id,
		DeviceCode:         deviceCode,
		UserCode:           userCode,
		DeviceName:         deviceName,
		Status:             "pending",
		ExpiresAt:          expiresAt,
		PollIntervalSeconds: pollInterval,
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
	m.records[deviceCode] = rec
	return id, nil
}

func (m *mockDeviceAuthRepo) GetDeviceAuthorizationByDeviceCode(ctx context.Context, deviceCode string) (*DeviceAuthorizationRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[deviceCode]
	if !ok {
		return nil, sql.ErrNoRows
	}
	cpy := *rec
	return &cpy, nil
}

func (m *mockDeviceAuthRepo) GetDeviceAuthorizationByUserCode(ctx context.Context, userCode string) (*DeviceAuthorizationRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.records {
		if rec.UserCode == userCode && rec.Status == "pending" {
			cpy := *rec
			return &cpy, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (m *mockDeviceAuthRepo) GetDeviceAuthorizationByUserCodeAny(ctx context.Context, userCode string) (*DeviceAuthorizationRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.records {
		if rec.UserCode == userCode {
			cpy := *rec
			return &cpy, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (m *mockDeviceAuthRepo) ApproveDeviceAuthorization(ctx context.Context, id, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.records {
		if rec.ID == id && rec.Status == "pending" {
			rec.UserID = sql.NullString{String: userID, Valid: true}
			rec.Status = "authorized"
			rec.UpdatedAt = time.Now()
			return nil
		}
	}
	return sql.ErrNoRows
}

func (m *mockDeviceAuthRepo) DenyDeviceAuthorization(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.records {
		if rec.ID == id && rec.Status == "pending" {
			rec.Status = "denied"
			rec.UpdatedAt = time.Now()
			return nil
		}
	}
	return sql.ErrNoRows
}

func (m *mockDeviceAuthRepo) ExpireDeviceAuthorizations(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.records {
		if rec.Status == "pending" && time.Now().After(rec.ExpiresAt) {
			rec.Status = "expired"
		}
	}
	return nil
}

// ExchangeDeviceAuthorization 模拟原子交换：只有 status=authorized 且未过期的记录
// 可以转为 completed 并返回 userID/deviceName；重复调用返回 ErrDeviceAuthCompleted。
func (m *mockDeviceAuthRepo) ExchangeDeviceAuthorization(ctx context.Context, deviceCode, accessTokenHash, refreshTokenHash string, expiresAt, refreshExpiresAt time.Time) (userID, deviceName string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.records[deviceCode]
	if !ok {
		return "", "", sql.ErrNoRows
	}

	if rec.Status == "completed" {
		return "", "", ErrDeviceAuthCompleted
	}
	if rec.Status == "denied" {
		return "", "", ErrDeviceAuthDenied
	}
	if rec.Status == "expired" || (rec.Status == "pending" && time.Now().After(rec.ExpiresAt)) {
		rec.Status = "expired"
		return "", "", ErrDeviceAuthExpired
	}
	if rec.Status != "authorized" {
		return "", "", ErrDeviceAuthPending
	}
	if !rec.UserID.Valid {
		return "", "", ErrDeviceAuthNoUser
	}

	rec.Status = "completed"
	rec.UpdatedAt = time.Now()
	return rec.UserID.String, rec.DeviceName, nil
}

// TestDeviceAuthStart_EmptyNameFallback 验证空 device_name 生成安全 fallback。
func TestDeviceAuthStart_EmptyNameFallback(t *testing.T) {
	repo := newMockDeviceAuthRepo()
	cfg := DefaultDeviceAuthConfig()

	result, err := DeviceAuthStart(context.Background(), repo, cfg, "", "https://example.com/device-authorize")
	if err != nil {
		t.Fatalf("DeviceAuthStart 失败: %v", err)
	}

	if result.DeviceCode == "" || result.UserCode == "" || result.VerificationURL == "" {
		t.Error("生成结果不完整")
	}
	if !strings.Contains(result.VerificationURL, "/device-authorize?code=") {
		t.Errorf("verification_url 应包含 /device-authorize?code=，得到 %q", result.VerificationURL)
	}
	if strings.Contains(result.VerificationURL, result.DeviceCode) {
		t.Error("verification_url 不得包含 device_code")
	}
	if strings.Contains(result.VerificationURL, result.UserCode) {
		// user_code 是预期参数
	}
}

// TestDeviceAuthPoll_Pending 验证 pending 状态。
func TestDeviceAuthPoll_Pending(t *testing.T) {
	repo := newMockDeviceAuthRepo()
	cfg := DefaultAuthConfig()

	deviceCode, _ := generateDeviceCode()
	userCode, _ := generateUserCode(8)
	_, err := repo.CreateDeviceAuthorization(context.Background(), deviceCode, userCode, "mcp-test", time.Now().Add(10*time.Minute), 5)
	if err != nil {
		t.Fatalf("创建记录失败: %v", err)
	}

	result, err := DeviceAuthPoll(context.Background(), repo, cfg, deviceCode)
	if err != nil {
		t.Fatalf("DeviceAuthPoll 失败: %v", err)
	}
	if result.Status != "pending" {
		t.Errorf("期望 pending，得到 %s", result.Status)
	}
}

// TestDeviceAuthPoll_OneTimeExchange 验证首次 authorized 返回 token，重复/并发返回 completed。
func TestDeviceAuthPoll_OneTimeExchange(t *testing.T) {
	repo := newMockDeviceAuthRepo()
	cfg := DefaultAuthConfig()

	deviceCode, _ := generateDeviceCode()
	userCode, _ := generateUserCode(8)
	id, err := repo.CreateDeviceAuthorization(context.Background(), deviceCode, userCode, "mcp-test", time.Now().Add(10*time.Minute), 5)
	if err != nil {
		t.Fatalf("创建记录失败: %v", err)
	}

	// 模拟用户批准
	if err := repo.ApproveDeviceAuthorization(context.Background(), id, "user-1"); err != nil {
		t.Fatalf("批准失败: %v", err)
	}

	// 第一次 poll：authorized，返回 token
	result1, err := DeviceAuthPoll(context.Background(), repo, cfg, deviceCode)
	if err != nil {
		t.Fatalf("第一次 poll 失败: %v", err)
	}
	if result1.Status != "authorized" {
		t.Fatalf("期望 authorized，得到 %s", result1.Status)
	}
	if result1.AccessToken == "" || result1.RefreshToken == "" {
		t.Error("首次 authorized 应返回 token")
	}
	if result1.TokenType != "Bearer" {
		t.Errorf("期望 Bearer，得到 %s", result1.TokenType)
	}

	// 第二次 poll：completed，不再颁发 token
	result2, err := DeviceAuthPoll(context.Background(), repo, cfg, deviceCode)
	if err != nil {
		t.Fatalf("第二次 poll 失败: %v", err)
	}
	if result2.Status != "completed" {
		t.Fatalf("期望 completed，得到 %s", result2.Status)
	}
	if result2.AccessToken != "" || result2.RefreshToken != "" {
		t.Error("重复 poll 不应返回 token")
	}
}

// TestDeviceAuthPoll_Denied 验证 denied 状态。
func TestDeviceAuthPoll_Denied(t *testing.T) {
	repo := newMockDeviceAuthRepo()
	cfg := DefaultAuthConfig()

	deviceCode, _ := generateDeviceCode()
	userCode, _ := generateUserCode(8)
	id, err := repo.CreateDeviceAuthorization(context.Background(), deviceCode, userCode, "mcp-test", time.Now().Add(10*time.Minute), 5)
	if err != nil {
		t.Fatalf("创建记录失败: %v", err)
	}

	if err := repo.DenyDeviceAuthorization(context.Background(), id); err != nil {
		t.Fatalf("拒绝失败: %v", err)
	}

	result, err := DeviceAuthPoll(context.Background(), repo, cfg, deviceCode)
	if err != nil {
		t.Fatalf("DeviceAuthPoll 失败: %v", err)
	}
	if result.Status != "denied" {
		t.Errorf("期望 denied，得到 %s", result.Status)
	}
}

// TestDeviceAuthPoll_Expired 验证 expired 状态。
func TestDeviceAuthPoll_Expired(t *testing.T) {
	repo := newMockDeviceAuthRepo()
	cfg := DefaultAuthConfig()

	deviceCode, _ := generateDeviceCode()
	userCode, _ := generateUserCode(8)
	_, err := repo.CreateDeviceAuthorization(context.Background(), deviceCode, userCode, "mcp-test", time.Now().Add(-1*time.Minute), 5)
	if err != nil {
		t.Fatalf("创建记录失败: %v", err)
	}

	result, err := DeviceAuthPoll(context.Background(), repo, cfg, deviceCode)
	if err != nil {
		t.Fatalf("DeviceAuthPoll 失败: %v", err)
	}
	if result.Status != "expired" {
		t.Errorf("期望 expired，得到 %s", result.Status)
	}
}

// TestHandleStart_EmptyDeviceName 验证空 device_name 不返回 400。
func TestHandleStart_EmptyDeviceName(t *testing.T) {
	repo := newMockDeviceAuthRepo()
	cfg := DefaultAuthConfig()
	daCfg := DefaultDeviceAuthConfig()
	daCfg.PublicOrigin = "https://example.com"

	handler := NewDeviceAuthHandler(repo, nil, cfg, daCfg, nil)

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device/start", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.HandleStart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if resp["verification_url"] == "" || !strings.Contains(resp["verification_url"].(string), "/device-authorize?code=") {
		t.Errorf("verification_url 应包含 /device-authorize?code=")
	}
}

// TestHandleStart_MissingPublicOrigin 验证缺少 PUBLIC_ORIGIN 时 device start fail closed。
func TestHandleStart_MissingPublicOrigin(t *testing.T) {
	repo := newMockDeviceAuthRepo()
	cfg := DefaultAuthConfig()
	daCfg := DefaultDeviceAuthConfig()

	handler := NewDeviceAuthHandler(repo, nil, cfg, daCfg, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/device/start", strings.NewReader(`{}`))
	req.Host = "attacker.example"
	w := httptest.NewRecorder()
	handler.HandleStart(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("缺少 PUBLIC_ORIGIN 时应返回 500，得到 %d", w.Code)
	}
}

// TestHandleStart_UsesConfiguredPublicOrigin 验证 verification_url 只使用可信 origin，不受 Host 注入影响。
func TestHandleStart_UsesConfiguredPublicOrigin(t *testing.T) {
	repo := newMockDeviceAuthRepo()
	cfg := DefaultAuthConfig()
	daCfg := DefaultDeviceAuthConfig()
	daCfg.PublicOrigin = "https://auth.example.com"

	handler := NewDeviceAuthHandler(repo, nil, cfg, daCfg, nil)

	req := httptest.NewRequest(http.MethodPost, "http://evil.example/api/auth/device/start", strings.NewReader(`{"device_name":"laptop"}`))
	req.Host = "evil.example"
	w := httptest.NewRecorder()
	handler.HandleStart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	verificationURL, _ := resp["verification_url"].(string)
	if !strings.HasPrefix(verificationURL, "https://auth.example.com/device-authorize?code=") {
		t.Fatalf("verification_url 应使用可信 PUBLIC_ORIGIN，得到 %q", verificationURL)
	}
	if strings.Contains(verificationURL, "evil.example") {
		t.Fatalf("verification_url 不应包含请求 Host，得到 %q", verificationURL)
	}
}

// mockAuditRepo 是 audit.Repository 的内存实现，用于验证 device auth 审计记录。
type mockAuditRepo struct {
	mu      sync.Mutex
	entries []mockAuditEntry
}

// mockAuditEntry 是测试捕获的审计字段。
type mockAuditEntry struct {
	userID       string
	workspaceID  string
	action       string
	resourceType string
	resourceID   string
	detail       json.RawMessage
}

func (m *mockAuditRepo) Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, mockAuditEntry{
		userID:       userID,
		workspaceID:  workspaceID,
		action:       action,
		resourceType: resourceType,
		resourceID:   resourceID,
		detail:       detail,
	})
	return nil
}

func (m *mockAuditRepo) List(ctx context.Context, filter audit.ListFilter, limit, offset int) (*audit.ListResult, error) {
	return &audit.ListResult{}, nil
}

// TestDeviceAuthHandlers_WriteAudit 验证 approve/deny/completed 写入 audit_logs 且不包含 code/token。
func TestDeviceAuthHandlers_WriteAudit(t *testing.T) {
	repo := newMockDeviceAuthRepo()
	auditRepo := &mockAuditRepo{}
	cfg := DefaultAuthConfig()
	daCfg := DefaultDeviceAuthConfig()
	daCfg.PublicOrigin = "https://auth.example.com"
	handler := NewDeviceAuthHandler(repo, nil, cfg, daCfg, auditRepo)

	start1, err := DeviceAuthStart(context.Background(), repo, daCfg, "device-one", "https://auth.example.com/device-authorize")
	if err != nil {
		t.Fatalf("DeviceAuthStart 失败: %v", err)
	}
	start2, err := DeviceAuthStart(context.Background(), repo, daCfg, "device-two", "https://auth.example.com/device-authorize")
	if err != nil {
		t.Fatalf("DeviceAuthStart 失败: %v", err)
	}

	approveReq := httptest.NewRequest(http.MethodPost, "/api/auth/device/approve", strings.NewReader(`{"user_code":"`+start1.UserCode+`"}`))
	approveReq = approveReq.WithContext(WithIdentity(approveReq.Context(), &Identity{UserID: "user-approver"}))
	approveW := httptest.NewRecorder()
	handler.HandleApprove(approveW, approveReq)
	if approveW.Code != http.StatusOK {
		t.Fatalf("approve 期望 200，得到 %d", approveW.Code)
	}

	denyReq := httptest.NewRequest(http.MethodPost, "/api/auth/device/deny", strings.NewReader(`{"user_code":"`+start2.UserCode+`"}`))
	denyReq = denyReq.WithContext(WithIdentity(denyReq.Context(), &Identity{UserID: "user-denier"}))
	denyW := httptest.NewRecorder()
	handler.HandleDeny(denyW, denyReq)
	if denyW.Code != http.StatusOK {
		t.Fatalf("deny 期望 200，得到 %d", denyW.Code)
	}

	pollReq := httptest.NewRequest(http.MethodPost, "/api/auth/device/poll", strings.NewReader(`{"device_code":"`+start1.DeviceCode+`"}`))
	pollW := httptest.NewRecorder()
	handler.HandlePoll(pollW, pollReq)
	if pollW.Code != http.StatusOK {
		t.Fatalf("poll 期望 200，得到 %d", pollW.Code)
	}

	auditRepo.mu.Lock()
	defer auditRepo.mu.Unlock()
	if len(auditRepo.entries) != 3 {
		t.Fatalf("期望 3 条审计记录，得到 %d", len(auditRepo.entries))
	}

	wantActions := map[string]string{
		"device.auth.approve":   "user-approver",
		"device.auth.deny":      "user-denier",
		"device.auth.completed": "user-approver",
	}
	for _, entry := range auditRepo.entries {
		wantUser, ok := wantActions[entry.action]
		if !ok {
			t.Fatalf("未知审计 action: %s", entry.action)
		}
		if entry.userID != wantUser {
			t.Fatalf("action %s 期望 user_id %s，得到 %s", entry.action, wantUser, entry.userID)
		}
		if entry.resourceType != "device_authorization" {
			t.Fatalf("action %s 期望 resource_type device_authorization，得到 %s", entry.action, entry.resourceType)
		}
		detail := string(entry.detail)
		if strings.Contains(detail, start1.UserCode) || strings.Contains(detail, start2.UserCode) ||
			strings.Contains(detail, start1.DeviceCode) || strings.Contains(detail, start2.DeviceCode) ||
			strings.Contains(detail, "token") {
			t.Fatalf("审计 detail 不应包含 code/token: %s", detail)
		}
	}
}
