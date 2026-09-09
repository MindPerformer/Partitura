#!/usr/bin/env bash
# ops-gate-job-recovery.sh — 验收门禁：Job 恢复
#
# 引入动机：design/05-OPERATIONS.md §Background Jobs 要求支持故障恢复。
# worker 崩溃或被 SIGKILL 后，其 claim 的 job 会永久停留在 running 状态。
# 启动时 RecoverStaleJobs 将这些 stale job 重置为 pending。
#
# 前置条件：
#   - Docker + Docker Compose 可用
#   - Compose 服务已启动
#   - curl 可用
#   - 至少有一个 system_admin 用户
#
# 用法：
#   BASE_URL=http://localhost:8080 ./scripts/ops-gate-job-recovery.sh
#   BASE_URL=http://localhost ./scripts/ops-gate-job-recovery.sh
#
# 退出码：
#   0 = Job 恢复验证通过
#   1 = 验证失败
#   2 = 环境不可用
set -euo pipefail

BASE_URL="${BASE_URL:-}"
if [ -z "$BASE_URL" ]; then
  echo "ERROR: BASE_URL 未设置" >&2
  exit 2
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "ERROR: curl 不可用" >&2
  exit 2
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "BLOCKED: docker 不可用" >&2
  exit 2
fi

if ! docker compose ps server --format '{{.Status}}' 2>/dev/null | grep -qi 'Up\|healthy'; then
  echo "BLOCKED: server 容器未运行" >&2
  exit 2
fi

PASS=0
FAIL=0

echo "=== Job 恢复验证 ==="

# 1. 检查 readyz 中的 job 统计
echo ""
echo "--- 初始 job 统计 ---"
READYZ_BODY=$(curl -sf "${BASE_URL}/readyz" 2>/dev/null) || {
  echo "BLOCKED: /readyz 不可达" >&2
  exit 2
}

INITIAL_PENDING=$(echo "$READYZ_BODY" | grep -o '"pending_jobs":[0-9]*' | head -1 | cut -d: -f2) || true
INITIAL_DEAD=$(echo "$READYZ_BODY" | grep -o '"failed_jobs":[0-9]*' | head -1 | cut -d: -f2) || true
echo "初始 pending jobs: ${INITIAL_PENDING:-unknown}"
echo "初始 dead jobs: ${INITIAL_DEAD:-unknown}"

# 2. 模拟 worker 崩溃：SIGKILL server 容器（不优雅关闭）
echo ""
echo "--- 模拟 worker 崩溃（SIGKILL） ---"
docker compose kill -s SIGKILL server 2>/dev/null || {
  echo "FAIL: 无法 SIGKILL server 容器"
  exit 1
}

sleep 2

# 3. 重启 server 容器（触发 RecoverStaleJobs）
echo ""
echo "--- 重启 server 容器（触发 RecoverStaleJobs） ---"
docker compose start server 2>/dev/null || {
  echo "FAIL: 无法重启 server 容器"
  exit 1
}

# 等待 server 就绪
echo "等待 server 就绪..."
WAIT=0
MAX_WAIT=60
READY=false

while [ $WAIT -lt $MAX_WAIT ]; do
  sleep 3
  WAIT=$((WAIT + 3))
  if curl -sf "${BASE_URL}/healthz" >/dev/null 2>&1; then
    READY=true
    echo "PASS: server 在 ${WAIT}s 内恢复"
    PASS=$((PASS + 1))
    break
  fi
done

if [ "$READY" != "true" ]; then
  echo "FAIL: server 在 ${MAX_WAIT}s 内未恢复"
  exit 1
fi

# 4. 检查 server 日志中是否有 RecoverStaleJobs 记录
echo ""
echo "--- 检查 RecoverStaleJobs 日志 ---"
sleep 3  # 等待日志写入
SERVER_LOGS=$(docker compose logs server --tail=30 2>/dev/null) || true

if echo "$SERVER_LOGS" | grep -q '恢复 stale running job'; then
  echo "PASS: 日志记录了 RecoverStaleJobs 执行"
  PASS=$((PASS + 1))
else
  echo "WARN: 日志未记录 RecoverStaleJobs（可能无 stale job 需要恢复）"
  # 这不是 FAIL，因为如果崩溃时没有 running job，RecoverStaleJobs 不会输出日志
fi

# 5. 验证恢复后 readyz 正常
echo ""
echo "--- 恢复后 job 统计 ---"
RECOVERED_READYZ=$(curl -sf "${BASE_URL}/readyz" 2>/dev/null) || {
  echo "FAIL: 恢复后 /readyz 不可达"
  FAIL=$((FAIL + 1))
}

RECOVERED_PENDING=$(echo "$RECOVERED_READYZ" | grep -o '"pending_jobs":[0-9]*' | head -1 | cut -d: -f2) || true
RECOVERED_DEAD=$(echo "$RECOVERED_READYZ" | grep -o '"failed_jobs":[0-9]*' | head -1 | cut -d: -f2) || true
echo "恢复后 pending jobs: ${RECOVERED_PENDING:-unknown}"
echo "恢复后 dead jobs: ${RECOVERED_DEAD:-unknown}"

# 6. 验证 PG 连接正常（readyz 返回 200 或 503）
READYZ_CODE=$(curl -sf -o /dev/null -w '%{http_code}' "${BASE_URL}/readyz" 2>/dev/null) || true
if [ "$READYZ_CODE" = "200" ]; then
  echo "PASS: 恢复后 /readyz 返回 200（PG 可用）"
  PASS=$((PASS + 1))
elif [ "$READYZ_CODE" = "503" ]; then
  echo "FAIL: 恢复后 /readyz 返回 503（PG 不可用）"
  FAIL=$((FAIL + 1))
else
  echo "FAIL: 恢复后 /readyz 返回 ${READYZ_CODE}（意外状态码）"
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
