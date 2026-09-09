// integration_test.go 测试 workspace 模块的 PostgreSQL 集成。
//
// 测试覆盖：
//   - CreateWorkspace 原子创建 workspace + owner membership
//   - 跨 workspace 访问拒绝
//   - 成员管理 owner 不变量
//   - audit 日志写入
//
// 运行条件：设置 TEST_DATABASE_URL 环境变量指向可用的 PostgreSQL 实例。
// 未设置时测试跳过，不算集成通过。
package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"partitura/server/internal/audit"
	"partitura/server/internal/db/testutil"
)

// setupIntegrationDB 创建测试数据库连接并执行迁移。
// 使用跨进程 advisory lock + 完整 reset 隔离，不依赖测试执行顺序。
// 返回 *sql.DB 和 cleanup 函数。
func setupIntegrationDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	return testutil.SetupTestDB(t)
}

// createIntegrationUser 在数据库中创建一个测试用户并返回其 ID。
func createIntegrationUser(t *testing.T, db *sql.DB, username, systemRole string, wsCreatePerm bool) string {
	t.Helper()
	var userID string
	err := db.QueryRowContext(context.Background(),
		`INSERT INTO users (username, email, password_hash, system_role, workspace_create_perm)
		 VALUES ($1, $2, 'dummyhash', $3, $4) RETURNING id`,
		username, username+"@test.example", systemRole, wsCreatePerm,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}
	return userID
}

// TestIntegration_CreateWorkspace_Atomic 集成测试：创建 workspace 时 workspace 和 owner membership 原子创建。
func TestIntegration_CreateWorkspace_Atomic(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	repo := NewPGRepository(db)
	ctx := context.Background()

	userID := createIntegrationUser(t, db, "intwsuser1", "user", true)

	ws, err := repo.CreateWorkspace(ctx, "intws1", "Integration WS 1", "test desc", userID)
	if err != nil {
		t.Fatalf("CreateWorkspace 失败: %v", err)
	}

	if ws.ID == "" {
		t.Fatal("workspace ID 不应为空")
	}
	if ws.Name != "intws1" {
		t.Errorf("name = %q, 期望 intws1", ws.Name)
	}
	if ws.Status != StatusActive {
		t.Errorf("status = %q, 期望 active", ws.Status)
	}

	// 验证 owner membership 创建
	role, err := repo.GetMemberRole(ctx, ws.ID, userID)
	if err != nil {
		t.Fatalf("查询创建者角色失败: %v", err)
	}
	if role != RoleOwner {
		t.Errorf("创建者角色 = %q, 期望 owner", role)
	}

	// 验证 owner 数量 = 1
	count, err := repo.CountOwners(ctx, ws.ID)
	if err != nil {
		t.Fatalf("查询 owner 数量失败: %v", err)
	}
	if count != 1 {
		t.Errorf("owner 数量 = %d, 期望 1", count)
	}
}

// TestIntegration_CrossWorkspace_Isolation 集成测试：跨 workspace 访问被拒绝。
func TestIntegration_CrossWorkspace_Isolation(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	repo := NewPGRepository(db)
	ctx := context.Background()

	user1 := createIntegrationUser(t, db, "intuser1", "user", true)
	user2 := createIntegrationUser(t, db, "intuser2", "user", true)

	ws1, _ := repo.CreateWorkspace(ctx, "intws_iso1", "WS1", "", user1)
	ws2, _ := repo.CreateWorkspace(ctx, "intws_iso2", "WS2", "", user2)

	_ = ws1
	_ = ws2

	// user1 不是 ws2 的成员
	_, err := repo.GetMemberRole(ctx, ws2.ID, user1)
	if err != sql.ErrNoRows {
		t.Errorf("user1 不应是 ws2 的成员，应返回 ErrNoRows，实际: %v", err)
	}

	// user1 的 workspace 列表不应包含 ws2
	result, err := repo.ListWorkspacesByUser(ctx, user1, 100, 0)
	if err != nil {
		t.Fatalf("ListWorkspacesByUser 失败: %v", err)
	}
	for _, ws := range result.Workspaces {
		if ws.ID == ws2.ID {
			t.Error("user1 的 workspace 列表不应包含 ws2")
		}
	}

	// user1 尝试列出 ws2 成员应返回空或被拒绝
	// （在 repository 层，ListMembers 按 workspace_id 查询，不检查调用者权限；
	//   权限检查在 handler/middleware 层完成）
	membersResult, err := repo.ListMembers(ctx, ws2.ID, 100, 0)
	if err != nil {
		t.Fatalf("ListMembers 失败: %v", err)
	}
	// ws2 应只有 user2 作为 owner
	for _, m := range membersResult.Members {
		if m.UserID == user1 {
			t.Error("ws2 成员列表不应包含 user1")
		}
	}
}

// TestIntegration_OwnerInvariant 集成测试：owner 不变量维护。
func TestIntegration_OwnerInvariant(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	repo := NewPGRepository(db)
	ctx := context.Background()

	owner := createIntegrationUser(t, db, "intowner", "user", true)
	member := createIntegrationUser(t, db, "intmember", "user", false)

	ws, _ := repo.CreateWorkspace(ctx, "intws_owner", "Owner Test", "", owner)

	// 添加一个 admin 成员
	_, err := repo.AddMember(ctx, ws.ID, member, RoleAdmin)
	if err != nil {
		t.Fatalf("AddMember 失败: %v", err)
	}

	// 尝试移除唯一的 owner 应在 handler 层被拒绝
	// 这里测试 repository 层的 CountOwners
	count, err := repo.CountOwners(ctx, ws.ID)
	if err != nil {
		t.Fatalf("CountOwners 失败: %v", err)
	}
	if count != 1 {
		t.Errorf("owner 数量 = %d, 期望 1", count)
	}

	// 添加第二个 owner
	_, err = repo.AddMember(ctx, ws.ID, member, RoleOwner)
	if err != nil {
		// 需要先移除 admin 成员记录再添加 owner
		_ = repo.RemoveMember(ctx, ws.ID, member)
		_, err = repo.AddMember(ctx, ws.ID, member, RoleOwner)
		if err != nil {
			t.Fatalf("添加第二个 owner 失败: %v", err)
		}
	}

	count, _ = repo.CountOwners(ctx, ws.ID)
	if count != 2 {
		t.Errorf("owner 数量 = %d, 期望 2", count)
	}
}

// TestIntegration_AuditLog 集成测试：审计日志写入和查询。
func TestIntegration_AuditLog(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	auditRepo := audit.NewPGRepository(db)
	ctx := context.Background()

	userID := createIntegrationUser(t, db, "intaudituser", "user", true)

	// 使用合法 UUID 作为 resource_id（audit_logs.resource_id 类型为 UUID）
	detail, _ := json.Marshal(map[string]string{"name": "test-ws"})
	err := auditRepo.Record(ctx, userID, "", "workspace.create", "workspace", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", detail, "req-001")
	if err != nil {
		t.Fatalf("Record 失败: %v", err)
	}

	// 查询审计日志
	result, err := auditRepo.List(ctx, 100, 0)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}

	if result.Total == 0 {
		t.Fatal("应至少有一条审计记录")
	}

	found := false
	for _, e := range result.Entries {
		if e.Action == "workspace.create" && e.UserID == userID {
			found = true
			// 验证 detail 不含 password/token
			detailStr := string(e.Detail)
			if strings.Contains(detailStr, "password") {
				t.Error("审计记录不应包含 password")
			}
		}
	}
	if !found {
		t.Error("未找到 workspace.create 审计记录")
	}
}

// TestIntegration_DuplicateWorkspaceName 集成测试：重复 workspace 名称返回错误。
func TestIntegration_DuplicateWorkspaceName(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	repo := NewPGRepository(db)
	ctx := context.Background()

	user1 := createIntegrationUser(t, db, "intdupuser1", "user", true)
	user2 := createIntegrationUser(t, db, "intdupuser2", "user", true)

	_, err := repo.CreateWorkspace(ctx, "dupname", "First", "", user1)
	if err != nil {
		t.Fatalf("第一次创建失败: %v", err)
	}

	_, err = repo.CreateWorkspace(ctx, "dupname", "Second", "", user2)
	if err == nil {
		t.Error("重复名称应返回错误")
	}
}

// TestIntegration_UpdateWorkspace 集成测试：更新 workspace。
func TestIntegration_UpdateWorkspace(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	repo := NewPGRepository(db)
	ctx := context.Background()

	userID := createIntegrationUser(t, db, "intupdateuser", "user", true)
	ws, _ := repo.CreateWorkspace(ctx, "intupdatews", "Original", "orig desc", userID)

	updated, err := repo.UpdateWorkspace(ctx, ws.ID, "Updated Name", "new desc")
	if err != nil {
		t.Fatalf("UpdateWorkspace 失败: %v", err)
	}
	if updated.DisplayName != "Updated Name" {
		t.Errorf("display_name = %q, 期望 Updated Name", updated.DisplayName)
	}
	if updated.Description != "new desc" {
		t.Errorf("description = %q, 期望 new desc", updated.Description)
	}
}

// TestIntegration_ArchiveWorkspace 集成测试：归档 workspace。
func TestIntegration_ArchiveWorkspace(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	repo := NewPGRepository(db)
	ctx := context.Background()

	userID := createIntegrationUser(t, db, "intarchiveuser", "user", true)
	ws, _ := repo.CreateWorkspace(ctx, "intarchivews", "To Archive", "", userID)

	archived, err := repo.ArchiveWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("ArchiveWorkspace 失败: %v", err)
	}
	if archived.Status != StatusArchived {
		t.Errorf("status = %q, 期望 archived", archived.Status)
	}
}

// TestIntegration_AdminUserUpdate 集成测试：admin 更新用户系统角色和权限。
func TestIntegration_AdminUserUpdate(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	repo := NewPGRepository(db)
	ctx := context.Background()

	userID := createIntegrationUser(t, db, "intadminupdate", "user", false)

	// 验证初始值
	role, perm, err := repo.GetUserSystemInfo(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserSystemInfo 失败: %v", err)
	}
	if role != "user" {
		t.Errorf("初始 system_role = %q, 期望 user", role)
	}
	if perm {
		t.Error("初始 workspace_create_perm 应为 false")
	}

	// 更新
	err = repo.UpdateUserSystemRole(ctx, userID, "system_admin")
	if err != nil {
		t.Fatalf("UpdateUserSystemRole 失败: %v", err)
	}

	err = repo.UpdateUserWorkspaceCreatePerm(ctx, userID, true)
	if err != nil {
		t.Fatalf("UpdateUserWorkspaceCreatePerm 失败: %v", err)
	}

	// 验证更新后
	role, perm, _ = repo.GetUserSystemInfo(ctx, userID)
	if role != "system_admin" {
		t.Errorf("更新后 system_role = %q, 期望 system_admin", role)
	}
	if !perm {
		t.Error("更新后 workspace_create_perm 应为 true")
	}
}

// TestIntegration_ListAllWorkspaces 集成测试：admin 列出全部 workspace。
func TestIntegration_ListAllWorkspaces(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	repo := NewPGRepository(db)
	ctx := context.Background()

	user1 := createIntegrationUser(t, db, "intlistuser1", "user", true)
	user2 := createIntegrationUser(t, db, "intlistuser2", "user", true)

	repo.CreateWorkspace(ctx, fmt.Sprintf("intlistws1_%d", time.Now().UnixNano()), "WS1", "", user1)
	repo.CreateWorkspace(ctx, fmt.Sprintf("intlistws2_%d", time.Now().UnixNano()), "WS2", "", user2)

	result, err := repo.ListAllWorkspaces(ctx, 100, 0)
	if err != nil {
		t.Fatalf("ListAllWorkspaces 失败: %v", err)
	}
	if result.Total < 2 {
		t.Errorf("应至少有 2 个 workspace，实际 %d", result.Total)
	}
}
