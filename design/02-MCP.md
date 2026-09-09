# Local MCP

## 架构

```text
Claude / Codex / Cursor
        │
      stdio MCP
        │
        ▼
knowledge-mcp (local Go binary)
        │
      HTTPS REST
        │
        ▼
Knowledge Server
```

服务器不直接暴露给 Agent 作为远程 MCP。

## 登录

提供：

`knowledge-mcp login <server>`

推荐使用浏览器登录 / device authorization 风格。

登录后：
- access token + refresh token
- 保存到 OS Credential Store

禁止把 token 明文写进 MCP 配置文件。

支持：
- logout
- revoke session
- server list/current

## 本地缓存

绝对禁止持久化 Workspace 文档缓存。

只允许进程内 memory cache。

默认：
- TTL = 30 minutes
- MCP config 可调整
- 进程退出全部丢失

可缓存：
- document content
- revision
- content hash
- section outline
- workspace metadata

缓存只用于性能和本地 patch。

服务器永远是真相源。

写请求必须携带：
- expected_revision
- expected_hash

缓存不能替代并发控制。

## Workspace

必须先切换：

`switch_workspace`

工具：

- workspace_list
- workspace_current
- switch_workspace
- workspace_bootstrap

禁止跨 Workspace search。

knowledge_search 永远只查询 active workspace。

MCP 工具不要提供：
- all_workspaces search
- workspace list filter search
- cross-workspace query

如果 Agent 要使用其他项目，必须显式切换。

## switch_workspace

切换成功只返回少量信息：

- workspace name
- PROJECT.md / AGENTS.md 存在情况
- 推荐下一步操作

不要自动把整个 PROJECT.md / AGENTS.md 塞入返回。

## workspace_bootstrap

返回：

- PROJECT.md 的必要内容
- AGENTS.md 的必要内容
- 项目重要入口
- 推荐先阅读的文档 outline / link

控制上下文大小。

PROJECT.md 和 AGENTS.md 不通过 search 找。

## MCP Guidance

本地 MCP 必须向 Agent 描述通用工作约束。

这些约束独立于 Workspace：

- 一个 Workspace 是一个项目
- 先 switch_workspace
- 大任务先 bootstrap
- 优先 search 已有知识
- 优先 section 读取而非整篇读取
- 优先修改已有文档
- 使用 Markdown
- 保留重要来源
- 不保存 chain-of-thought
- 不保存临时 debug 噪声
- 不复制完整源码
- patch 前确认 revision/hash
- 冲突后重新 read

这些只是 Agent guidance。

服务端不得根据这些规则拒绝正文内容。

## Document Tools

必须至少：

- document_list
- document_outline
- document_read
- document_read_section
- document_read_lines
- document_create
- document_patch
- document_replace
- document_move
- document_archive
- document_history
- document_revision
- knowledge_search
- source_attach

## document_outline

返回 Markdown heading tree：

- heading
- level
- start_line
- end_line
- section_path

## document_read_section

输入：
- path
- section_path[]

返回该 Section Markdown。

section_path 使用结构化数组，避免同名 heading 冲突。

## document_read_lines

输入：
- path
- start_line
- end_line

限制最大行数，防止一次返回超大内容。

## document_patch

Local MCP：

1. 从 memory cache 或 server 获取 base revision
2. 本地应用 old_text -> new_text
3. 验证唯一匹配
4. 生成 candidate content
5. 计算 hash
6. 上传 server

Server：
- revision/hash 一致 -> accept
- 不一致 -> 409

MCP 可尝试一次安全 rebase：
- 获取最新文档
- 如果 old_text 仍唯一匹配则重新 patch

否则返回 conflict。

不要做复杂自动三方 merge。

## MCP 配置

示例：

```toml
server = "https://knowledge.company.com"
cache_ttl = "30m"
cache_max_entries = 200
default_search_mode = "hybrid"
search_result_limit = 10
```

认证凭证不放在这里。
