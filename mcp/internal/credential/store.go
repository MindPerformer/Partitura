// Package credential 实现 MCP 客户端凭据的持久化抽象。
//
// 引入动机：design/02-MCP.md §登录 要求 access token + refresh token 保存到本机凭据存储，
// 禁止把 token 明文写进 MCP 配置文件。
//
// 设计变更（动机）：早期实现按平台分派（Windows 使用 Credential Manager，
// 其他平台为 fail-fast 桩），导致非 Windows 既无法编译也无法运行，且三平台行为不一致。
// 现统一改为跨平台的 AES-256-GCM 加密文件存储（见 FileStore），
// 凭据文件与配置同放在运行目录（当前工作目录）下的 .knowledge-mcp 中。
//
// 设计原则：
//   - 定义 Store 接口，实现与调用方解耦，测试可注入替身
//   - 凭据落盘必须加密，禁止明文存储
//   - 任何失败必须 fail-fast 并有安全日志，不静默降级
//   - 提供 logout/revoke 清理凭据的完整能力
//   - 不使用反射
package credential

import (
	"fmt"
	"log/slog"
)

// Tokens 是持久化的认证凭据。
// 引入动机：MCP client 需要 access token 和 refresh token 调用 server API。
// design/02-MCP.md §登录 要求登录后保存 access token + refresh token。
type Tokens struct {
	// AccessToken 是 bearer access token，用于 API 请求的 Authorization 头。
	AccessToken string `json:"access_token"`

	// RefreshToken 是 refresh token，用于 access token 过期后刷新。
	RefreshToken string `json:"refresh_token"`

	// ServerURL 是与此凭据关联的 server URL。
	// 引入动机：支持多 server 凭据，logout 时按 server 清理。
	ServerURL string `json:"server_url"`
}

// Store 定义凭据持久化接口。
// 引入动机：调用方只依赖抽象，测试可注入内存替身，生产使用加密文件实现。
type Store interface {
	// Save 持久化指定 server 的 token。
	// 引入动机：login 成功后需要持久化 token。
	// 存储失败必须返回错误（fail-fast），不静默降级、不吞错误。
	Save(serverURL string, tokens *Tokens) error

	// Load 读取指定 server 的 token。
	// 引入动机：MCP 进程启动时需要加载已保存的 token。
	// 如果凭据不存在，返回空 Tokens 和 nil error（表示未登录）。
	Load(serverURL string) (*Tokens, error)

	// Delete 删除指定 server 的 token。
	// 引入动机：logout/revoke 需要清理凭据。
	// 如果凭据不存在，返回 nil（幂等操作）。
	Delete(serverURL string) error
}

// failFastLog 在凭据操作失败时记录安全日志并返回错误。
// 引入动机：design 要求存储失败必须 fail-fast 并有安全日志。
// 安全要求：日志中不输出 token 值。
func failFastLog(operation, serverURL string, err error) error {
	slog.Error("凭据操作失败",
		"operation", operation,
		"server_url", serverURL,
		"error", err.Error(),
	)
	return fmt.Errorf("%s 失败: %w", operation, err)
}
