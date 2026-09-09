// Package credential 实现操作系统级凭据存储抽象。
//
// 引入动机：design/02-MCP.md §登录 要求 access token + refresh token 保存到 OS Credential Store，
// 禁止把 token 明文写进 MCP 配置文件。
//
// 设计原则：
//   - 定义 Store 接口，平台实现分离
//   - Windows 使用 Credential Manager（cmdkey / advapi32）
//   - 存储失败必须 fail-fast 并有安全日志
//   - 提供 logout/revoke 清理凭据的完整能力
//   - 不使用反射
package credential

import (
	"fmt"
	"log/slog"
)

// CredentialTarget 是凭据在 OS credential store 中的标识前缀。
// 引入动机：避免与其他应用的凭据冲突，使用统一前缀。
const CredentialTarget = "Partitura/knowledge-mcp"

// Tokens 是存储在 credential store 中的认证凭据。
// 引入动机：MCP client 需要 access token 和 refresh token 调用 server API。
// design/02-MCP.md §登录 要求登录后保存 access token + refresh token。
type Tokens struct {
	// AccessToken 是 bearer access token，用于 API 请求的 Authorization 头。
	AccessToken string

	// RefreshToken 是 refresh token，用于 access token 过期后刷新。
	RefreshToken string

	// ServerURL 是与此凭据关联的 server URL。
	// 引入动机：支持多 server 凭据，logout 时按 server 清理。
	ServerURL string
}

// Store 定义操作系统级凭据存储接口。
// 引入动机：不同操作系统使用不同的 credential store 实现，
// 通过接口抽象使测试可注入安全替身，生产使用真实 OS 实现。
type Store interface {
	// Save 将 token 保存到 OS credential store。
	// 引入动机：login 成功后需要持久化 token。
	// 存储失败必须返回错误（fail-fast），不静默降级到文件存储。
	Save(serverURL string, tokens *Tokens) error

	// Load 从 OS credential store 读取 token。
	// 引入动机：MCP 进程启动时需要加载已保存的 token。
	// 如果凭据不存在，返回空 Tokens 和 nil error（表示未登录）。
	Load(serverURL string) (*Tokens, error)

	// Delete 从 OS credential store 删除 token。
	// 引入动机：logout/revoke 需要清理凭据。
	// 如果凭据不存在，返回 nil（幂等操作）。
	Delete(serverURL string) error
}

// targetName 构建 credential store 中的条目名称。
// 引入动机：使用 server URL 作为后缀，支持多 server 凭据。
func targetName(serverURL string) string {
	return fmt.Sprintf("%s/%s", CredentialTarget, serverURL)
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
