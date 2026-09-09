# Documents

## 形式

所有项目知识正文统一为 Markdown。

系统本身不限制用户最终写什么。

MCP 只向 Agent 提供维护规范。

## 特殊文件

只有：

- PROJECT.md
- AGENTS.md

具有特殊意义。

它们：
- 可以正常读取和修改
- 有 Revision
- 不进入搜索索引

## 默认目录

建议：

architecture/
codebase/
development/
decisions/
issues/
roadmap/
research/
references/
operations/
standards/

不强制。

允许任意合法目录。

禁止：
- absolute path
- ..
- path traversal

## Metadata

数据库保存：

- id
- workspace_id
- path
- title
- type optional
- status
- content_markdown
- content_hash
- created_by
- updated_by
- created_at
- updated_at

type 可选：
- architecture
- codebase
- development
- decision
- issue
- roadmap
- research
- reference
- operation
- standard
- guide
- other

status：
- active
- draft
- archived

Front Matter 可支持，但数据库 metadata 是权威来源。

导出 Markdown 时可以生成 Front Matter。

## Source

V1 做 Document-level provenance。

source type：
- web
- code
- file
- issue
- commit
- conversation
- manual
- other

字段可包括：
- value/url/path
- title
- retrieved_at
- content_hash
- refresh_interval_days optional

如果外部 source 已经长期未验证：
- 标记 stale
- search/read 可以返回 freshness metadata

服务器不自动联网刷新，也不调用 LLM。

## Revision

每次 create/update/move/archive 产生 revision。

保存完整 Markdown snapshot。

不要使用长 diff chain 作为唯一历史。

默认 retention：
- 7 days
- max 30 revisions per document

管理员可以调整：
- revision_retention_days
- revision_max_count

当前版本永不因为 retention 删除。

历史版本满足任一上限后允许清理。

Workspace 可以选择允许管理员单独覆盖 retention。

## Diff

Web/Server 在需要时计算：
- revision A vs B
- current vs revision

## Archive

默认删除操作是 archive。

Archived document：
- 默认搜索排除
- 默认列表可隐藏
- 可恢复

永久 purge 仅高权限用户可执行。

## 大文档

默认单 Markdown 最大 2MB，可配置。

长文鼓励按 Section 组织。

读取优先：
- outline
- section
- lines

避免 Agent 总是 read whole document。
