// workspace_test.go 测试 workspace 状态管理和安全约束。
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 4 验收要求：
//   - 未 switch 时每个 project 工具拒绝
//   - switch 后 search 请求不可注入或跨 workspace
package workspace

import (
	"testing"
)

func TestStateNotActive(t *testing.T) {
	s := NewState()

	if s.IsActive() {
		t.Fatal("新创建的 State 不应 active")
	}
	if s.ID() != "" {
		t.Errorf("ID = %q, 期望空", s.ID())
	}
}

func TestStateRequireActiveRejectsBeforeSwitch(t *testing.T) {
	s := NewState()

	err := s.RequireActive()
	if err == nil {
		t.Fatal("期望 RequireActive 在未 switch 时返回错误")
	}
}

func TestStateSwitch(t *testing.T) {
	s := NewState()

	s.Switch("ws-uuid-123", "Test Workspace")

	if !s.IsActive() {
		t.Fatal("switch 后应 active")
	}
	if s.ID() != "ws-uuid-123" {
		t.Errorf("ID = %q, 期望 ws-uuid-123", s.ID())
	}
	if s.Name() != "Test Workspace" {
		t.Errorf("Name = %q, 期望 Test Workspace", s.Name())
	}
}

func TestStateRequireActiveAfterSwitch(t *testing.T) {
	s := NewState()
	s.Switch("ws-uuid-123", "Test")

	err := s.RequireActive()
	if err != nil {
		t.Fatalf("switch 后 RequireActive 不应返回错误: %v", err)
	}
}

func TestStateClear(t *testing.T) {
	s := NewState()
	s.Switch("ws-uuid-123", "Test")
	s.Clear()

	if s.IsActive() {
		t.Fatal("Clear 后不应 active")
	}
	if s.ID() != "" {
		t.Errorf("Clear 后 ID = %q, 期望空", s.ID())
	}
}

func TestStateRequireActiveAfterClear(t *testing.T) {
	s := NewState()
	s.Switch("ws-uuid-123", "Test")
	s.Clear()

	err := s.RequireActive()
	if err == nil {
		t.Fatal("Clear 后 RequireActive 应返回错误")
	}
}
