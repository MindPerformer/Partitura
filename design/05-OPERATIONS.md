# Operations

## Docker Compose

默认服务：

- server
- web
- postgres
- elasticsearch

Embedding / Reranker 都是在线 Provider，不在 Compose 部署模型。

## Server

Go 后端保持 stateless。

关键状态都在：
- PostgreSQL
- Elasticsearch（可重建）

允许未来横向运行多个 server replica。

禁止关键 session/job/lock 只存在单机内存。

## Background Jobs

使用 PostgreSQL-backed queue。

任务：
- index_document
- rebuild_index
- repair_index
- evaluate_profile
- optimize_profile
- cleanup_revisions
- cleanup_old_indexes

worker 使用 row lock / SKIP LOCKED 等可靠方式 claim。

支持多 worker。

## Fault Degradation

Elasticsearch down：
- 文档读取/修改继续
- search 返回 unavailable/degraded
- jobs pending/retry

Embedding down：
- lexical search 仍可用
- semantic path 降级
- indexing job 重试

Reranker down：
- RRF 结果直接返回

任何外部 Provider 故障不得阻止文档保存。

## Health

- /healthz
- /readyz

报告：
- PostgreSQL
- Elasticsearch
- Embedding Provider
- Reranker Provider
- pending jobs
- failed jobs
- active Search Profile

## Backup

核心备份：
- PostgreSQL

Elasticsearch 可以备份，但必须始终可从 PostgreSQL 重建。

必须文档化：
- pg_dump
- restore
- index rebuild

## Revision Cleanup

定时任务根据管理员设置：
- retention_days
- max_revisions

清理历史 snapshot。

当前版本永不清理。

## Search Maintenance

每天：
- integrity validation
- stale/missing/orphan repair
- failed job retry

每周：
- Level 1 profile tuning
- evaluation replay

低频/手动：
- full Search Profile experiment
- embedding model/provider change
- analyzer changes

## HA

Compose V1 可单机运行。

代码结构必须支持未来：
- reverse proxy
- multiple Go replicas
- PostgreSQL HA
- Elasticsearch multi-node

不要一开始做微服务拆分。

## Logging

使用 log/slog structured logging。

字段：
- request_id
- user_id
- workspace_id
- document_id
- search_id
- job_id
- latency
- error
