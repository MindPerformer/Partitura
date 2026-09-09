# Project Knowledge Workspace — Master Prompt

你要从零实现一个可长期运行的企业级 Project Knowledge Workspace。

它不是 AI Memory，也不是聊天记录系统。

一个 Workspace 就是一个独立项目。AI、人类和子代理通过 Web / MCP 共同维护项目知识，让新的会话可以快速接手项目，而不是反复扫描源码、重新搜索网页、重新研究同一个问题。

## 目标

系统必须支持：

- 多用户
- 多 Workspace
- Workspace 权限控制
- Markdown 项目知识
- 文档创建、读取、修改、归档
- Section / 行号级读取
- 版本历史
- 搜索
- 在线 Embedding Provider
- 在线 Reranker Provider
- Elasticsearch Hybrid Retrieval
- 自动检索质量评测和调优
- 本地 MCP Client
- Nuxt + Nuxt UI Web
- Docker Compose 部署
- 长期运行与故障恢复

## 固定技术方向

Backend:
- Go
- PostgreSQL
- Elasticsearch
- REST API

Frontend:
- Nuxt
- Nuxt UI
- TypeScript

AI integration:
- 本地 Go MCP binary
- MCP 通过 stdio 暴露给 Agent
- MCP 通过 HTTPS API 与服务器通讯

Embedding:
- 仅在线 API
- 默认推荐 Qwen3 Embedding
- 必须支持任意自定义 Provider / Base URL / API Key / Model / Dimensions
- 不得写死模型或向量维度

Reranker:
- 仅在线 API
- 默认推荐 Qwen3 Reranker
- Provider 化
- 不得成为搜索单点故障

部署:
- Docker Compose
- server
- web
- postgres
- elasticsearch

不部署本地 Embedding/Reranker 模型。

## 核心数据原则

PostgreSQL 是业务真相源：

- users
- workspaces
- permissions
- documents
- revisions
- sources
- audit
- jobs
- search profiles
- evaluation data

Elasticsearch 是可重建搜索索引。

Elasticsearch 数据丢失时，必须能够从 PostgreSQL 完整重建。

## Workspace

一个 Workspace = 一个项目。

不存在：
- Team Knowledge Scope
- Global Knowledge Scope
- Root Knowledge Scope
- Workspace nesting

MCP 同时可以访问多个 Workspace，但任何读取、搜索和修改都必须先：

`switch_workspace`

之后所有工具默认只作用于当前 Workspace。

禁止跨 Workspace 搜索。

如果 Agent 要查看其他项目：
1. switch_workspace
2. bootstrap / search
3. 必要时再切回来

## 特殊文件

每个 Workspace 固定两个特殊文件：

- PROJECT.md
- AGENTS.md

PROJECT.md:
- 项目入口
- 项目是什么
- 关键模块
- 重要文档入口
- 当前项目地图

AGENTS.md:
- 当前项目对 Agent 的特殊工作规范
- 项目自己的构建、测试、修改注意事项等约束入口

重要：

PROJECT.md 和 AGENTS.md：
- 不进入语义索引
- 不进入 BM25 搜索
- 不出现在 knowledge_search 结果中

它们只通过：
- workspace_bootstrap
- document_read
- document_read_section
- document_read_lines
读取。

## 通用 Agent 规范

与具体项目无关的文档格式、工具用法、知识维护标准，不写进 Workspace。

这些规范必须由本地 MCP 自己告知 Agent。

例如：
- 先搜索已有知识再重复研究
- 优先修改已有文档，不创建重复内容
- 使用 Markdown
- 长文优先 section 读取
- patch 前确认 revision
- 不保存 chain-of-thought
- 不保存临时 debug 噪声
- 不复制完整源码
- 技术研究保留来源
- 项目知识按主题维护，不按聊天日期维护

这些是 MCP Guidance，不是服务端内容限制。

服务器不判断“什么内容允许写”。

## Workspace 默认目录

创建项目时建议初始化：

PROJECT.md
AGENTS.md

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

目录只是默认建议。

用户可以创建任意合法目录。

## 文档原则

知识应该描述：
- 为什么这样设计
- 模块负责什么
- 重要源码在哪里
- 构建方式
- 测试方式
- 技术路线
- 已知缺陷
- 未来计划
- 难以重新发现的项目事实
- 已完成技术研究
- 外部资料整理结果
- 重要来源

不要把系统做成聊天总结仓库。

## 开发提示词拆分

按阶段读取：

- `01-SEARCH.md`：搜索、Embedding、Reranker、自动调优
- `02-MCP.md`：本地 MCP、缓存、登录、工具
- `03-DOCUMENTS.md`：Markdown、Section、Revision、Source
- `04-WEB-API.md`：Nuxt UI、REST、RBAC
- `05-OPERATIONS.md`：Docker Compose、高可用、索引维护
- `06-IMPLEMENTATION.md`：实际开发顺序和验收

执行时不要一次重新读取全部文件。

## 实现要求

你不是写架构提案。

你需要真正创建和完成项目。

每个阶段：
1. 实现
2. migration
3. test
4. build
5. 修复错误
6. 再进入下一阶段

禁止留下大量 placeholder、空 handler、伪实现。

对于没有明确规定的小型技术决策，选择最简单、成熟、长期可维护的实现。
