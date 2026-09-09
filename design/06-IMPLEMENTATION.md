# Implementation Plan

## Phase 1 — Foundation

读取：
- 00-MASTER.md
- 04-WEB-API.md

实现：
- Go project
- PostgreSQL migrations
- auth
- device/session model
- users
- workspaces
- RBAC
- audit

验收：
- workspace isolation
- roles
- no cross-workspace access

## Phase 2 — Documents

读取：
- 03-DOCUMENTS.md

实现：
- Markdown documents
- PROJECT.md / AGENTS.md initialization
- outline parser
- section read
- lines read
- revisions
- sources
- archive
- optimistic concurrency

验收：
- long Markdown section reading
- exact line ranges
- revision conflict
- retention cleanup

## Phase 3 — Search

读取：
- 01-SEARCH.md

实现：
- Elasticsearch mapping
- BM25
- dense vector
- chunking
- Embedding Provider
- Reranker Provider
- RRF
- diversification
- Search Profiles
- index jobs
- rebuild
- evaluation

必须保证 PROJECT.md / AGENTS.md 永远不进入索引。

验收：
- identifier search
- semantic question
- rerank
- profile switching
- alias switch
- rebuild
- degradation
- integrity repair

## Phase 4 — Local MCP

读取：
- 02-MCP.md

实现：
- Go MCP binary
- stdio MCP
- device/browser login
- OS credential storage
- in-memory 30m cache
- switch_workspace
- bootstrap
- section/line read
- local patch
- conflict handling
- MCP Guidance

验收：
- MCP 进程退出后无文档缓存残留
- 不允许 cross-workspace search
- 未 switch workspace 时拒绝 project tools
- patch 409 正确处理

## Phase 5 — Nuxt UI

读取：
- 04-WEB-API.md

实现：
- Nuxt
- Nuxt UI
- workspace
- VitePress-like document reader
- editor
- search
- revisions/diff
- members
- admin
- search evaluation/profile UI

## Phase 6 — Operations

读取：
- 05-OPERATIONS.md

实现：
- Dockerfiles
- Compose
- health
- jobs
- backup/restore docs
- cleanup
- reindex
- production README
- graceful shutdown

## Tests

至少：

- RBAC
- cross-workspace isolation
- PROJECT.md/AGENTS.md excluded from ES
- path traversal
- section parser
- line read limits
- revision conflict
- revision retention
- lexical search
- semantic search
- RRF
- reranker fallback
- document diversity
- index rebuild
- alias switch
- search profile regression gate
- local MCP workspace switch
- local MCP no persistent document cache
- MCP permission enforcement
- index integrity repair

## 工程要求

- Go idiomatic
- 不过度 Clean Architecture
- 不引入 Redis/Kafka
- 不引入生成式 LLM
- 不部署本地模型
- PostgreSQL 是业务真相源
- Elasticsearch 可完整重建
- 所有 provider 可替换
- dimensions 可配置
- 所有列表和搜索分页
- 所有后台任务可重试
- 任何新 Search Profile 必须可回滚

## AI 执行方式

不要只写 proposal。

完成一个 Phase 后：
1. run tests
2. build
3. 修复问题
4. 再进入下一个 Phase

上下文不足时只读：
- 00-MASTER.md
- 当前 Phase 对应文件

不要反复加载所有设计规范。
