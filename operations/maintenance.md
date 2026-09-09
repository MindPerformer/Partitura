# 日常维护与清理

## 自动维护调度

系统通过 PG-backed scheduler 自动执行周期性维护任务。调度去重基于 PostgreSQL `scheduler_runs` 表的唯一约束，多个 server 实例不会重复执行同一周期的任务。

### 每日任务

| 任务 | Job 类型 | 说明 |
|------|----------|------|
| 索引完整性检查与修复 | `repair_index` | 检查 PG vs ES 一致性，修复 missing/orphan/stale/hash/revision 差异 |
| 失败任务重试 | `repair_index` | 重试 dead 状态的 job |

### 每周任务

| 任务 | Job 类型 | 说明 |
|------|----------|------|
| Level 1 自动调优 | `optimize_profile` | 自动调整 field boost/top_k/RRF/candidate count，gate 通过后自动激活 |
| 评测重放 | `evaluate_profile` | 对 active profile 执行评测，计算 NDCG/Recall/MRR 等指标 |

## Revision 清理

### 管理员配置

Revision 清理基于 Workspace 级别的管理员设置：

- `retention_days`：历史版本保留天数
- `max_revisions`：每个文档最大版本数

### 清理规则

- **当前版本永不清理**：清理操作始终保留文档的当前 revision
- 清理由 `cleanup_revisions` job 执行，可手动触发或由调度器自动触发
- 清理操作逐文档执行，单个文档清理失败不影响其他文档

### 验证清理

```sql
-- 检查文档当前版本
SELECT id, path, revision_number FROM documents WHERE workspace_id = '<ws_id>';

-- 检查历史版本
SELECT document_id, revision_number, created_at FROM revisions
WHERE document_id = '<doc_id>'
ORDER BY revision_number DESC;

-- 确认当前版本存在
SELECT COUNT(*) FROM revisions
WHERE document_id = '<doc_id>' AND revision_number = (
  SELECT revision_number FROM documents WHERE id = '<doc_id>'
);
-- 应返回 1
```

## 旧索引清理

`cleanup_old_indexes` job 自动清理旧版本 ES 索引：

- 列出所有 `knowledge_v*` 索引
- 排除当前 alias 指向的索引（**永不清理**）
- 保留最近 2 个旧索引以支持 rollback
- 删除其余旧索引

### 手动触发

```bash
curl -X POST http://localhost/api/admin/jobs \
  -H "Authorization: Bearer <admin_token>" \
  -H "X-CSRF-Token: <csrf_token>" \
  -H "Content-Type: application/json" \
  -d '{"type": "cleanup_old_indexes", "payload": {"keep_count": 2}}'
```

## 日志与监控

### 查看日志

```bash
# Server 日志
docker compose logs -f server

# 过滤 job 相关日志
docker compose logs server | grep "job"

# 过滤 scheduler 相关日志
docker compose logs server | grep "调度"
```

### 健康检查

```bash
# 进程存活
curl http://localhost/healthz

# 就绪状态（含 job 计数和 profile 信息）
curl http://localhost/readyz
```

### Job 监控

```bash
# 查看 pending job 数量
curl http://localhost/readyz | jq '.checks.pending_jobs'

# 查看 dead job 数量
curl http://localhost/readyz | jq '.checks.failed_jobs'

# 通过 admin API 查看 job 列表
curl http://localhost/api/admin/jobs?status=dead \
  -H "Authorization: Bearer <admin_token>"
```

## 优雅关闭

Server 收到 SIGTERM 后的关闭顺序：

1. 停止接受新 HTTP 请求
2. 取消 server context（通知 Job Worker 和 Scheduler 停止）
3. 等待 Job Worker 安全退出（进行中的任务完成或超时）
4. 等待 Scheduler 安全退出
5. 关闭数据库连接

`stop_grace_period` 设置为 50s，`SHUTDOWN_TIMEOUT` 设置为 45s，确保有足够时间完成优雅关闭。

## 升级与迁移

```bash
# 1. 拉取新镜像
docker compose pull

# 2. 检查迁移状态
docker compose run --rm server -migrate-status

# 3. 重建并启动
docker compose up -d --build

# 4. 验证迁移版本
docker compose logs server | grep "迁移完成"
```
