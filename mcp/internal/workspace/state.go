// Package workspace 实现 MCP client 的 active workspace 状态管理。
//
// 引入动机：design/02-MCP.md §Workspace 要求：
//   - 必须先 switch_workspace 才能使用 project tools
//   - 所有 project tools（document/search/source）仅作用于 active workspace
//   - 调用方不得控制 workspace ID，由 client 内部注入
//   - 禁止跨 workspace 搜索
//
// 安全原则：
//   - switch_workspace 必须验证用户有该 workspace 权限
//   - active workspace ID 由 client 内部注入到所有请求路径
//   - 工具调用方不能指定 workspace ID 参数
package workspace

import (
	"fmt"
	"sync"
)

// State 管理 MCP client 的 active workspace 状态。
// 引入动机：所有 project tools 依赖 active workspace ID，
// 状态管理确保未 switch 时拒绝 project tools。
type State struct {
	mu sync.RWMutex

	// activeWorkspaceID 是当前 active workspace 的 UUID。
	// 空字符串表示未切换 workspace。
	activeWorkspaceID string

	// activeWorkspaceName 是当前 active workspace 的显示名称。
	activeWorkspaceName string
}

// NewState 创建 workspace 状态管理器。
func NewState() *State {
	return &State{}
}

// Switch 设置 active workspace。
// 引入动机：switch_workspace 工具验证权限后调用此方法。
func (s *State) Switch(workspaceID, workspaceName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeWorkspaceID = workspaceID
	s.activeWorkspaceName = workspaceName
}

// Clear 清除 active workspace 状态。
// 引入动机：logout 时清除 workspace 状态。
func (s *State) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeWorkspaceID = ""
	s.activeWorkspaceName = ""
}

// IsActive 返回是否已切换 workspace。
func (s *State) IsActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeWorkspaceID != ""
}

// ID 返回 active workspace ID。
// 引入动机：project tools 构建请求路径时需要 workspace ID。
// 如果未切换，返回空字符串。
func (s *State) ID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeWorkspaceID
}

// Name 返回 active workspace 名称。
func (s *State) Name() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeWorkspaceName
}

// RequireActive 检查是否已切换 workspace，否则返回错误。
// 引入动机：所有 project tools 在执行前调用此方法。
func (s *State) RequireActive() error {
	if !s.IsActive() {
		return fmt.Errorf("未切换 workspace，请先使用 switch_workspace 工具")
	}
	return nil
}
