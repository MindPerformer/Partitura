// Package guidance 提供 MCP Guidance 文本。
//
// 引入动机：design/02-MCP.md §MCP Guidance 要求本地 MCP 向 Agent 描述通用工作约束。
// 这些约束独立于 Workspace，不是服务端内容限制。
// Guidance 来自 design/00-MASTER.md §通用 Agent 规范。
//
// 设计原则：只保留「跨工具的工作流纪律」和「schema 表达不了的判断」，
// 参数级细节由 tools/list 的 schema 描述承载，避免重复占用 initialize 上下文。
package guidance

// MCPGuidance 是 MCP 向 Agent 描述的通用工作约束文本。
// 引入动机：design/02-MCP.md §MCP Guidance 和 design/00-MASTER.md §通用 Agent 规范。
// 此文本在 initialize 响应的 instructions 字段中返回给 Agent。
const MCPGuidance = `# Knowledge Workspace MCP — Working Rules
- Call switch_workspace first; every other tool acts on the active workspace. No cross-workspace search.
- Prefer knowledge_search before new research; prefer updating an existing document over creating a duplicate.
- Long documents: document_outline first, then read a slice via document_read(section_path or start_line+end_line).
- Writes carry expected_revision+expected_hash taken from your last read. document_patch reuses the cached read; on a 409 it retries one safe rebase — if it still conflicts, re-read and retry.
- Write tools return metadata only by default (verbose=true adds full content_markdown). Don't request it unless needed.
- document_archive: omit delete to archive (recoverable); delete=true permanently deletes (owner only).
- upload_document_file: file_path must be an absolute path on the machine running MCP; overwrite=false fails if the path exists, overwrite=true replaces the document.
- Keep knowledge concise and topic-organized: no chain-of-thought, no debug noise, no full source dumps. Record provenance with source_attach.`
