// Package buildinfo 提供 knowledge-mcp 的构建元信息。
//
// 引入动机：MCP server 需要在 initialize 握手与 version 命令中报告真实版本，
// 而不是在源码中硬编码版本字符串；否则发布产物无法自证其构建来源。
//
// 作用：集中承载版本、提交号和构建时间，供协议层与 CLI 复用。
//
// 注入方式：构建时通过 -ldflags "-X" 覆盖默认值，例如：
//
//	go build -ldflags "\
//	  -X partitura/mcp/internal/buildinfo.Version=v1.2.3 \
//	  -X partitura/mcp/internal/buildinfo.Commit=<git-sha> \
//	  -X partitura/mcp/internal/buildinfo.Date=<build-date>" \
//	  ./cmd/knowledge-mcp
//
// 未注入时保持默认值，保证本地开发构建可正常运行。
package buildinfo

var (
	// Version 是构建版本号。
	// 引入动机：MCP initialize 的 serverInfo.version 需要真实版本，而非硬编码常量。
	// 默认 "dev" 表示未经 ldflags 注入的开发构建。
	Version = "dev"

	// Commit 是构建对应的 git commit。
	// 引入动机：诊断线上问题时需要定位具体代码版本。
	// 默认 "unknown" 表示构建时未注入。
	Commit = "unknown"

	// Date 是构建时间。
	// 引入动机：判断部署产物新旧需要构建时间戳。
	// 默认 "unknown" 表示构建时未注入。
	Date = "unknown"
)
