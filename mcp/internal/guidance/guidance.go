// Package guidance 提供 MCP Guidance 文本。
//
// 引入动机：design/02-MCP.md §MCP Guidance 要求本地 MCP 向 Agent 描述通用工作约束。
// 这些约束独立于 Workspace，不是服务端内容限制。
// Guidance 来自 design/00-MASTER.md §通用 Agent 规范。
package guidance

// MCPGuidance 是 MCP 向 Agent 描述的通用工作约束文本。
// 引入动机：design/02-MCP.md §MCP Guidance 和 design/00-MASTER.md §通用 Agent 规范。
// 此文本在 initialize 响应的 instructions 字段中返回给 Agent。
const MCPGuidance = `# Knowledge Workspace MCP — Agent Working Rules

## Workspace model
- One workspace is one project.
- Always call switch_workspace before any read, search, or write.
- Cross-workspace search is forbidden.

## Knowledge maintenance flow
1. switch_workspace to the target project first.
2. For large tasks, call workspace_bootstrap to get the project overview.
3. Prefer knowledge_search on existing knowledge before doing new research.
4. Prefer updating an existing document over creating duplicate content.
5. Use Markdown.

## Document reading strategy
- For long documents, use document_outline first to see the structure.
- Prefer document_read_section over reading a whole document.
- Use document_read_lines for a precise line range.
- Avoid reading very large documents in one call.

## Document writing strategy
- Read the document first to obtain the current revision and hash.
- For document_patch, confirm revision/hash first to avoid concurrency conflicts.
- Use upload_document_file to import an existing local Markdown file into the active workspace.
- upload_document_file creates a new document by default and never overwrites unless overwrite is set to true.
- Always pass an absolute file_path; the path is resolved by the machine running MCP, using that operating system's native path format.
- document_archive archives a document; pass delete=true only when permanent deletion is intended, which requires owner permission.
- After a conflict, read the latest content again before retrying.
- Do not store chain-of-thought or temporary debug noise.
- Do not copy complete source code into the knowledge base.
- Keep sources for technical research (source_attach).
- Organize project knowledge by topic, not by chat date.

## Version control
- Every write must carry expected_revision + expected_hash.
- On a 409 conflict the MCP retries once with a safe rebase.
- If the rebase still fails, read the latest content manually and retry.`
