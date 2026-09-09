#!/usr/bin/env bash
# ops-gate-sigterm.sh — 验收门禁：SIGTERM 优雅关闭
#
# 引入动机：design/05-OPERATIONS.md §Graceful Shutdown 要求：
#   - 停止接受新 HTTP 请求
#   - 取消 server context（通知 Job Worker 和 Scheduler 停止）
#   - 等待 Job Worker 安全退出
#   - 等待 Scheduler 安全退出
#   - 关闭数据库连接
#
# 前置条件：
#   - Docker + Docker Compose 可用
#   - Compose 服务已启动
#   - curl 可用
#
# 用法：
#   ./scripts/ops-gate-sigterm.sh
#
# 退出码：
#   0 = 优雅关闭验证通过
#   1 = 优雅关闭失败
#   2 = 环境不可用
set -euo pipefail

if ! command -v docker >/dev/null 2>&1; then
  echo "BLOCKED: docker 不可用" >&2
  exit 2
fi

if ! docker compose ps server --format '{{.Status}}' 2>/dev/null | grep -qi 'Up\|healthy'; then
  echo "BLOCKED: server 容器未运行" >&2
  echo "请先执行: docker compose up -d" >&2
  exit 2
fi

PASS=0
FAIL=0

echo "=== SIGTERM 优雅关闭验证 ==="

# 1. 记录 server 容器 ID
SERVER_CONTAINER=$(docker compose ps server -q 2>/dev/null) || {
  echo "FAIL: 无法获取 server 容器 ID"
  exit 1
}

# 2. 发送 SIGTERM
echo "发送 SIGTERM 到 server 容器..."
docker compose kill -s SIGTERM server 2>/dev/null || {
  echo "FAIL: 发送 SIGTERM 失败"
  exit 1
}

# 3. 等待容器退出（stop_grace_period=50s，SHUTDOWN_TIMEOUT=45s）
echo "等待 server 容器退出（最多 60s）..."
WAIT=0
MAX_WAIT=60
EXITED=false

while [ $WAIT -lt $MAX_WAIT ]; do
  sleep 2
  WAIT=$((WAIT + 2))

  CONTAINER_STATUS=$(docker inspect -f '{{.State.Status}}' "$SERVER_CONTAINER" 2>/dev/null) || true
  if [ "$CONTAINER_STATUS" = "exited" ]; then
    echo "PASS: server 容器在 ${WAIT}s 内退出"
    PASS=$((PASS + 1))
    EXITED=true
    break
  fi
done

if [ "$EXITED" != "true" ]; then
  echo "FAIL: server 容器在 ${MAX_WAIT}s 内未退出（可能优雅关闭超时）"
  FAIL=$((FAIL + 1))
fi

# 4. 检查退出码
EXIT_CODE=$(docker inspect -f '{{.State.ExitCode}}' "$SERVER_CONTAINER" 2>/dev/null) || true
if [ "$EXIT_CODE" = "0" ]; then
  echo "PASS: server 容器退出码为 0（正常退出）"
  PASS=$((PASS + 1))
else
  echo "FAIL: server 容器退出码为 ${EXIT_CODE}，期望 0"
  FAIL=$((FAIL + 1))
fi

# 5. 检查日志中是否有优雅关闭记录
echo ""
echo "检查优雅关闭日志..."
SERVER_LOGS=$(docker compose logs server --tail=20 2>/dev/null) || true

if echo "$SERVER_LOGS" | grep -q '收到关闭信号'; then
  echo "PASS: 日志记录了收到关闭信号"
  PASS=$((PASS + 1))
else
  echo "FAIL: 日志未记录收到关闭信号"
  FAIL=$((FAIL + 1))
fi

if echo "$SERVER_LOGS" | grep -q '优雅关闭完成'; then
  echo "PASS: 日志记录了优雅关闭完成"
  PASS=$((PASS + 1))
else
  echo "FAIL: 日志未记录优雅关闭完成"
  FAIL=$((FAIL + 1))
fi

# 6. 重启 server 容器
echo ""
echo "重启 server 容器..."
docker compose start server 2>/dev/null || {
  echo "WARN: server 容器重启失败，可能需要手动启动: docker compose up -d server"
}

# 等待 server 恢复
sleep 5
if docker compose ps server --format '{{.Status}}' 2>/dev/null | grep -qi 'Up\|healthy'; then
  echo "PASS: server 容器已重启"
  PASS=$((PASS + 1))
else
  echo "WARN: server 容器可能需要更多时间启动"
fi

echo ""
echo "=== 汇总 ==="
echo "PASS: $PASS"
echo "FAIL: $FAIL"

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
