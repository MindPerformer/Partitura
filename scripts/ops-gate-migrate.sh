#!/usr/bin/env bash
# ops-gate-migrate.sh — 验收门禁：数据库迁移
#
# 引入动机：design/06-IMPLEMENTATION.md Phase 6 要求迁移可执行。
# design/05-OPERATIONS.md §Backup 要求恢复后检查迁移状态。
#
# 前置条件：
#   - Docker + Docker Compose 可用
#   - Compose postgres 容器运行中
#   - server 镜像已构建
#
# 用法：
#   ./scripts/ops-gate-migrate.sh
#
# 退出码：
#   0 = 迁移验证通过
#   1 = 验证失败
#   2 = 环境不可用
set -euo pipefail

if ! command -v docker >/dev/null 2>&1; then
  echo "BLOCKED: docker 不可用" >&2
  exit 2
fi

if ! docker compose ps postgres --format '{{.Status}}' 2>/dev/null | grep -qi 'Up\|healthy'; then
  echo "BLOCKED: postgres 容器未运行" >&2
  echo "请先执行: docker compose up -d postgres" >&2
  exit 2
fi

PASS=0
FAIL=0

echo "=== 迁移状态检查 ==="

# 使用 server -migrate-status 查询当前迁移版本
MIGRATE_OUTPUT=$(docker compose run --rm server -migrate-status 2>&1) || {
  echo "FAIL: -migrate-status 执行失败"
  echo "输出: $MIGRATE_OUTPUT"
  exit 1
}

echo "$MIGRATE_OUTPUT"

# 验证输出包含 "当前迁移版本"
if echo "$MIGRATE_OUTPUT" | grep -q '当前迁移版本'; then
  echo "PASS: -migrate-status 输出包含迁移版本信息"
  PASS=$((PASS + 1))
else
  echo "FAIL: -migrate-status 输出缺少迁移版本信息"
  FAIL=$((FAIL + 1))
fi

# 提取迁移版本号
VERSION=$(echo "$MIGRATE_OUTPUT" | grep -o '当前迁移版本: [0-9]*' | grep -o '[0-9]*' || echo "")

# 当前项目有 10 个迁移文件（M001-M010）
EXPECTED_MIN_VERSION=10

if [ -n "$VERSION" ]; then
  if [ "$VERSION" -ge "$EXPECTED_MIN_VERSION" ]; then
    echo "PASS: 迁移版本 $VERSION >= $EXPECTED_MIN_VERSION（全部迁移已执行）"
    PASS=$((PASS + 1))
  else
    echo "FAIL: 迁移版本 $VERSION < $EXPECTED_MIN_VERSION（存在未执行的迁移）"
    FAIL=$((FAIL + 1))
  fi
else
  echo "FAIL: 无法从 -migrate-status 输出提取迁移版本号"
  FAIL=$((FAIL + 1))
fi

echo ""
echo "=== 迁移文件清单 ==="
MIGRATION_COUNT=$(ls server/migrations/*_up.sql 2>/dev/null | wc -l)
echo "迁移文件数: $MIGRATION_COUNT（up.sql）"

if [ "$MIGRATION_COUNT" -ge "$EXPECTED_MIN_VERSION" ]; then
  echo "PASS: 迁移文件数 $MIGRATION_COUNT >= $EXPECTED_MIN_VERSION"
  PASS=$((PASS + 1))
else
  echo "FAIL: 迁移文件数 $MIGRATION_COUNT < $EXPECTED_MIN_VERSION"
  FAIL=$((FAIL + 1))
fi

echo ""
echo "=== 汇总 ==="
echo "PASS: $PASS"
echo "FAIL: $FAIL"

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
