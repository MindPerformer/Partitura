// Package guidance 提供 MCP Guidance 文本。
//
// 引入动机：design/02-MCP.md §MCP Guidance 要求本地 MCP 向 Agent 描述通用工作约束。
// 这些约束独立于 Workspace，不是服务端内容限制。
// Guidance 来自 design/00-MASTER.md §通用 Agent 规范。
package guidance

// MCPGuidance 是 MCP 向 Agent 描述的通用工作约束文本。
// 引入动机：design/02-MCP.md §MCP Guidance 和 design/00-MASTER.md §通用 Agent 规范。
// 此文本在 initialize 响应的 instructions 字段中返回给 Agent。
const MCPGuidance = `# Knowledge Workspace MCP — Agent 工作约束

## 工作区概念
- 一个 Workspace 是一个独立项目。
- 任何读取、搜索和修改前必须先 switch_workspace。
- 禁止跨 Workspace 搜索。

## 知识维护流程
1. 先 switch_workspace 切换到目标项目。
2. 大任务先 workspace_bootstrap 了解项目概况。
3. 优先 knowledge_search 搜索已有知识，避免重复研究。
4. 优先修改已有文档，不创建重复内容。
5. 使用 Markdown 格式。

## 文档读取策略
- 长文优先 document_outline 查看结构。
- 优先 document_read_section 按 section 读取，而非整篇读取。
- document_read_lines 用于精确行范围读取。
- 避免一次性读取超大文档。

## 文档修改策略
- 修改前先 document_read 获取当前 revision 和 hash。
- document_patch 前确认 revision/hash，避免并发冲突。
- 冲突后重新 read 获取最新内容再修改。
- 不保存 chain-of-thought、临时 debug 噪声。
- 不复制完整源码到知识库。
- 技术研究保留来源（source_attach）。
- 项目知识按主题维护，不按聊天日期维护。

## 版本控制
- 每次修改必须携带 expected_revision + expected_hash。
- 409 冲突时 MCP 会自动尝试一次安全 rebase。
- 如果 rebase 仍失败，需要手动 read 最新内容后重试。`
