// rbac_test.go 测试 Workspace RBAC 权限矩阵和权限判断逻辑。
//
// 测试覆盖：
//   - viewer/editor/admin/owner 完整权限矩阵
//   - admin 不能拥有 transfer/purge
//   - HasPermission 对非法角色返回 false
//   - IsValidRole 验证
package workspace

import (
	"testing"
)

// TestHasPermission_Viewer 验证 viewer 角色的权限矩阵。
func TestHasPermission_Viewer(t *testing.T) {
	tests := []struct {
		perm  string
		allow bool
	}{
		{PermRead, true},
		{PermSearch, true},
		{PermHistory, true},
		{PermCreate, false},
		{PermUpdate, false},
		{PermMove, false},
		{PermArchive, false},
		{PermMemberManage, false},
		{PermSettings, false},
		{PermTransfer, false},
		{PermPurge, false},
	}
	for _, tt := range tests {
		if got := HasPermission(RoleViewer, tt.perm); got != tt.allow {
			t.Errorf("viewer.HasPermission(%q) = %v, 期望 %v", tt.perm, got, tt.allow)
		}
	}
}

// TestHasPermission_Editor 验证 editor 角色的权限矩阵。
func TestHasPermission_Editor(t *testing.T) {
	tests := []struct {
		perm  string
		allow bool
	}{
		{PermRead, true},
		{PermSearch, true},
		{PermHistory, true},
		{PermCreate, true},
		{PermUpdate, true},
		{PermMove, true},
		{PermArchive, false},
		{PermMemberManage, false},
		{PermSettings, false},
		{PermTransfer, false},
		{PermPurge, false},
	}
	for _, tt := range tests {
		if got := HasPermission(RoleEditor, tt.perm); got != tt.allow {
			t.Errorf("editor.HasPermission(%q) = %v, 期望 %v", tt.perm, got, tt.allow)
		}
	}
}

// TestHasPermission_Admin 验证 admin 角色的权限矩阵。
// 特别验证 admin 不能拥有 transfer/purge。
func TestHasPermission_Admin(t *testing.T) {
	tests := []struct {
		perm  string
		allow bool
	}{
		{PermRead, true},
		{PermSearch, true},
		{PermHistory, true},
		{PermCreate, true},
		{PermUpdate, true},
		{PermMove, true},
		{PermArchive, true},
		{PermMemberManage, true},
		{PermSettings, true},
		{PermTransfer, false}, // admin 不能 transfer
		{PermPurge, false},    // admin 不能 purge
	}
	for _, tt := range tests {
		if got := HasPermission(RoleAdmin, tt.perm); got != tt.allow {
			t.Errorf("admin.HasPermission(%q) = %v, 期望 %v", tt.perm, got, tt.allow)
		}
	}
}

// TestHasPermission_Owner 验证 owner 角色的权限矩阵（全部权限）。
func TestHasPermission_Owner(t *testing.T) {
	tests := []struct {
		perm  string
		allow bool
	}{
		{PermRead, true},
		{PermSearch, true},
		{PermHistory, true},
		{PermCreate, true},
		{PermUpdate, true},
		{PermMove, true},
		{PermArchive, true},
		{PermMemberManage, true},
		{PermSettings, true},
		{PermTransfer, true},
		{PermPurge, true},
	}
	for _, tt := range tests {
		if got := HasPermission(RoleOwner, tt.perm); got != tt.allow {
			t.Errorf("owner.HasPermission(%q) = %v, 期望 %v", tt.perm, got, tt.allow)
		}
	}
}

// TestHasPermission_InvalidRole 验证非法角色返回 false。
func TestHasPermission_InvalidRole(t *testing.T) {
	if HasPermission("superadmin", PermRead) {
		t.Error("非法角色应返回 false")
	}
	if HasPermission("", PermRead) {
		t.Error("空角色应返回 false")
	}
}

// TestIsValidRole 验证角色合法性检查。
func TestIsValidRole(t *testing.T) {
	valid := []string{RoleOwner, RoleAdmin, RoleEditor, RoleViewer}
	for _, r := range valid {
		if !IsValidRole(r) {
			t.Errorf("IsValidRole(%q) 应返回 true", r)
		}
	}
	invalid := []string{"superadmin", "", "member", "guest"}
	for _, r := range invalid {
		if IsValidRole(r) {
			t.Errorf("IsValidRole(%q) 应返回 false", r)
		}
	}
}

// TestIsValidWorkspaceStatus 验证 workspace 状态合法性检查。
func TestIsValidWorkspaceStatus(t *testing.T) {
	if !IsValidWorkspaceStatus(StatusActive) {
		t.Error("active 应为合法状态")
	}
	if !IsValidWorkspaceStatus(StatusArchived) {
		t.Error("archived 应为合法状态")
	}
	if IsValidWorkspaceStatus("deleted") {
		t.Error("deleted 应为非法状态")
	}
	if IsValidWorkspaceStatus("") {
		t.Error("空字符串应为非法状态")
	}
}
