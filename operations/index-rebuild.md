# Elasticsearch 索引重建

## 核心原则

**Elasticsearch 是可重建的搜索索引。** 数据丢失或损坏后，可从 PostgreSQL 完整重建。
重建过程中文档读写不受影响，搜索暂时返回 degraded 结果。

## 何时需要重建

- PostgreSQL 恢复后
- Elasticsearch 数据卷损坏或丢失
- Search Profile 重大变更（embedding model/dimensions/analyzer）
- 索引完整性检查发现不可修复的差异

## 触发索引重建

### 通过 Admin API

端点是 `POST /api/admin/jobs/rebuild`，仅 `system_admin` 可调用。请求体是两个可选字段组成的 JSON 对象：

- `index_name`：目标索引名。留空时回退到 active profile 的 `es_index_name`。
- `workspace_id`：限定工作区。留空表示不限定。

请求体必须是合法 JSON 对象，且只允许上述字段（未知字段会被拒绝）。成功时返回 `202 Accepted`，响应体为 `{"status":"enqueued"}`；真正的重建由 job worker 异步执行。

该端点受 CSRF 保护：使用 cookie session 认证时必须在请求头携带 `X-CSRF-Token`，值为登录时下发的非 HttpOnly `csrf` cookie（header 名与 cookie 名可分别通过 `CSRF_HEADER_NAME`、`CSRF_COOKIE_NAME` 覆盖）。使用 Bearer token 认证时不需要 CSRF 头。

```bash
# cookie session 认证（需要 system_admin 用户）
curl -X POST http://localhost/api/admin/jobs/rebuild \
  -H "X-CSRF-Token: <csrf_token>" \
  -H "Content-Type: application/json" \
  -d '{}'
```

```bash
# Bearer token 认证（不需要 CSRF 头）
curl -X POST http://localhost/api/admin/jobs/rebuild \
  -H "Authorization: Bearer <admin_token>" \
  -H "Content-Type: application/json" \
  -d '{"index_name": "knowledge_v1"}'
```

### 通过 Job 队列

rebuild_index job 会被 worker 自动处理。可通过 admin API 查看 job 状态：

```bash
# 查看 job 列表
curl http://localhost/api/admin/jobs?status=pending \
  -H "Authorization: Bearer <admin_token>"
```

## 重建流程

1. `rebuild_index` job 创建新版本索引（如 `knowledge_v2`）
2. 从 PostgreSQL 读取所有非特殊文件、非 archived 文档
3. 对每个文档执行 chunking + embedding（如 Provider 可用）
4. 批量写入新索引
5. 执行完整性检查（integrity validation）
6. 修复发现的问题（missing/orphan/stale/hash/revision mismatch）
7. 原子切换 alias（`knowledge_current` → 新索引）
8. 旧索引保留以支持 rollback

## 重建后验证

```bash
# 1. 检查 /readyz 中 ES 状态
curl http://localhost/readyz | jq '.checks.elasticsearch'
# 应显示 {"status": "healthy"}

# 2. 检查 job 完成状态
curl http://localhost/api/admin/jobs?status=completed \
  -H "Authorization: Bearer <admin_token>"

# 3. 执行搜索测试
curl http://localhost/api/workspaces/<ws_id>/search?q=test \
  -H "Authorization: Bearer <token>"
```

## 索引完整性维护

系统每天自动执行完整性检查和修复：

- **每日**：integrity validation + stale/missing/orphan repair + failed job retry
- **每周**：Level 1 profile tuning + evaluation replay

这些维护任务通过 PG-backed scheduler 去重，多个 server 实例不会重复执行。

## 旧索引清理

`cleanup_old_indexes` job 自动清理旧版本索引，保留最近 2 个旧索引以支持 rollback。
当前 alias 指向的索引永远不会被清理。

## 降级行为

重建期间如果 Embedding Provider 不可用：
- 文档仍会被索引（仅 lexical 字段，无 embedding 向量）
- 语义搜索降级，词汇搜索正常
- Job 保持 pending 状态等待重试

重建期间如果 Elasticsearch 不可用：
- Job 保持 pending 状态等待重试
- 文档读写不受影响
- 搜索返回 degraded
