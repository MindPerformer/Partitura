# PostgreSQL 备份与恢复

## 核心原则

**PostgreSQL 是业务真相源。** 所有用户、文档、版本、任务、搜索配置都存储在 PostgreSQL 中。
Elasticsearch 数据丢失后可从 PostgreSQL 完整重建，因此核心备份对象是 PostgreSQL。

## 备份

### 一致性备份（pg_dump）

```bash
# 在宿主机执行，通过 Docker 执行 pg_dump
docker compose exec -T postgres pg_dump -U partitura partitura > backup_$(date +%Y%m%d_%H%M%S).sql
```

### 备份脚本示例

```bash
#!/bin/bash
set -euo pipefail

BACKUP_DIR="${1:?用法: backup.sh <目标目录>}"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
BACKUP_FILE="${BACKUP_DIR}/partitura_${TIMESTAMP}.sql"

# 检查目标目录存在
if [ ! -d "$BACKUP_DIR" ]; then
  echo "错误: 目标目录不存在: $BACKUP_DIR" >&2
  exit 1
fi

# 检查文件不覆盖
if [ -f "$BACKUP_FILE" ]; then
  echo "错误: 备份文件已存在，拒绝覆盖: $BACKUP_FILE" >&2
  exit 1
fi

# 执行备份（不输出密码到日志）
docker compose exec -T postgres pg_dump -U partitura partitura > "$BACKUP_FILE"

echo "备份完成: $BACKUP_FILE ($(wc -c < "$BACKUP_FILE") bytes)"
```

## 恢复

### 恢复到空数据库

**恢复优先级**：PostgreSQL 优先于 Elasticsearch。恢复 PG 后再触发 ES 重建。

```bash
# 1. 停止 server（避免恢复期间有写入）
docker compose stop server

# 2. 创建空数据库（如果数据库已存在且需要完全恢复）
docker compose exec -T postgres psql -U partitura -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"

# 3. 恢复备份
docker compose exec -T postgres psql -U partitura partitura < backup_20240115_120000.sql

# 4. 检查迁移状态
docker compose run --rm server -migrate-status
# 应输出: 当前迁移版本: 7

# 5. 启动 server
docker compose start server

# 6. 检查 server 就绪状态
curl http://localhost/readyz
```

### 恢复后验证

1. **Schema 完整性**：`-migrate-status` 显示正确版本
2. **数据完整性**：通过 API 检查文档列表
3. **Server readiness**：`/readyz` 返回 `status: "ready"` 或 `"degraded"`

## 恢复后 Elasticsearch 重建

PostgreSQL 恢复后，Elasticsearch 索引可能不一致。需要触发索引重建：

参见 `index-rebuild.md`。

## 安全注意事项

- 备份文件包含全部业务数据，需妥善保管
- 备份脚本不输出密码到日志
- 恢复前确认目标数据库为空或可覆盖
- 恢复操作需要停止 server 以避免写入冲突
