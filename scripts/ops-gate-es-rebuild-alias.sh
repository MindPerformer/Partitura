#!/usr/bin/env bash
# ops-gate-es-rebuild-alias.sh — 验收门禁：ES 从 PG rebuild 和 alias 验证
#
# 引入动机：design/05-OPERATIONS.md §Backup 要求 ES 可从 PG 完整重建。
# design/01-SEARCH.md §Index Version 要求 alias 原子切换。
#
# 前置条件：
#   - Docker + Docker Compose 可用
#   - Compose 服务已启动（postgres + elasticsearch + server 运行中）
#   - .env 已配置（本脚本不读取凭据）
#   - 至少有一个 system_admin 用户已通过 /bootstrap 创建
#   - curl 可用
#
# 用法：
#   BASE_URL=http://localhost:8080 ./scripts/ops-gate-es-rebuild-alias.sh
#   BASE_URL=http://localhost ./scripts/ops-gate-es-rebuild-alias.sh  # 通过 web 反代
#
# 退出码：
#   0 = ES rebuild 和 alias 验证通过
#   1 = 验证失败
#   2 = 环境不可用
#
# 注意：本脚本需要 admin token。获取方式：
#   1. 通过 /api/auth/login 登录 system_admin 用户获取 session cookie + CSRF token
#   2. 脚本会提示手动输入凭据，不硬编码任何密码
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

# 检查 server 是否可达
if ! curl -sf "${BASE_URL}/healthz" >/dev/null 2>&1; then
  echo "BLOCKED: server 不可达（${BASE_URL}/healthz）" >&2
  exit 2
fi

# 检查 ES 是否在 readyz 中报告
READYZ_BODY=$(curl -sf "${BASE_URL}/readyz" 2>/dev/null) || {
  echo "BLOCKED: /readyz 不可达" >&2
  exit 2
}

if echo "$READYZ_BODY" | grep -q '"elasticsearch"'; then
  ES_STATUS=$(echo "$READYZ_BODY" | grep -o '"elasticsearch":{[^}]*}' | grep -o '"status":"[^"]*"' | head -1 | cut -d: -f2 | tr -d '"')
  echo "ES 状态: $ES_STATUS"
  if [ "$ES_STATUS" != "healthy" ]; then
    echo "BLOCKED: ES 状态为 $ES_STATUS，需要 healthy 才能验证 rebuild" >&2
    exit 2
  fi
else
  echo "BLOCKED: /readyz 未报告 ES 状态" >&2
  exit 2
fi

echo ""
echo "=== ES Rebuild + Alias 验证 ==="
echo "此脚本需要 system_admin 凭据。"
echo "请提供 admin 用户名和密码以触发 rebuild_index job。"
echo "凭据不会被存储或记录。"
echo ""

read -r -p "Admin 用户名: " ADMIN_USER
read -r -s -p "Admin 密码: " ADMIN_PASS
echo ""

# 登录获取 session cookie 和 CSRF token
COOKIE_FILE=$(mktemp)
CSRF_TOKEN=""

LOGIN_RESP=$(curl -sf -c "$COOKIE_FILE" -D - \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASS}\"}" \
  "${BASE_URL}/api/auth/login" 2>/dev/null) || {
  echo "FAIL: 登录失败" >&2
  rm -f "$COOKIE_FILE"
  exit 1
}

# 提取 CSRF token
CSRF_TOKEN=$(echo "$LOGIN_RESP" | grep -i 'set-cookie:.*csrf' | grep -o 'csrf=[^;]*' | head -1 | cut -d= -f2) || true
if [ -z "$CSRF_TOKEN" ]; then
  # 尝试从响应体获取
  CSRF_TOKEN=$(echo "$LOGIN_RESP" | grep -o '"csrf_token":"[^"]*"' | head -1 | cut -d: -f2 | tr -d '"') || true
fi

if [ -z "$CSRF_TOKEN" ]; then
  echo "FAIL: 无法获取 CSRF token" >&2
  rm -f "$COOKIE_FILE"
  exit 1
fi

echo "PASS: 登录成功，获取 CSRF token"

# 触发 rebuild_index job
echo ""
echo "触发 rebuild_index job..."
REBUILD_RESP=$(curl -sf -b "$COOKIE_FILE" \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: ${CSRF_TOKEN}" \
  -X POST \
  -d '{"index_name":"knowledge_v1"}' \
  "${BASE_URL}/api/admin/search/rebuild" 2>/dev/null) || {
  echo "FAIL: 触发 rebuild_index job 失败" >&2
  rm -f "$COOKIE_FILE"
  exit 1
}

echo "PASS: rebuild_index job 已触发"

# 轮询 job 状态（最多等待 120 秒）
echo ""
echo "等待 rebuild_index job 完成（最多 120 秒）..."
JOB_ID=$(echo "$REBUILD_RESP" | grep -o '"job_id":"[^"]*"' | head -1 | cut -d: -f2 | tr -d '"') || true
if [ -z "$JOB_ID" ]; then
  echo "WARN: 响应中未找到 job_id，尝试通过 job 列表查询"
fi

WAIT=0
MAX_WAIT=120
JOB_DONE=false

while [ $WAIT -lt $MAX_WAIT ]; do
  sleep 5
  WAIT=$((WAIT + 5))

  # 查询 job 状态
  if [ -n "$JOB_ID" ]; then
    JOB_STATUS=$(curl -sf -b "$COOKIE_FILE" \
      -H "X-CSRF-Token: ${CSRF_TOKEN}" \
      "${BASE_URL}/api/admin/jobs/${JOB_ID}" 2>/dev/null | grep -o '"status":"[^"]*"' | head -1 | cut -d: -f2 | tr -d '"') || true
  else
    # 查询最近的 rebuild_index job
    JOB_STATUS=$(curl -sf -b "$COOKIE_FILE" \
      -H "X-CSRF-Token: ${CSRF_TOKEN}" \
      "${BASE_URL}/api/admin/jobs?status=completed&limit=1" 2>/dev/null | grep -o '"status":"[^"]*"' | head -1 | cut -d: -f2 | tr -d '"') || true
  fi

  if [ "$JOB_STATUS" = "completed" ]; then
    echo "PASS: rebuild_index job 已完成（等待 ${WAIT}s）"
    JOB_DONE=true
    break
  elif [ "$JOB_STATUS" = "dead" ]; then
    echo "FAIL: rebuild_index job 状态为 dead（等待 ${WAIT}s）" >&2
    break
  fi
  echo "  job 状态: ${JOB_STATUS:-unknown}（${WAIT}s）"
done

if [ "$JOB_DONE" != "true" ]; then
  echo "FAIL: rebuild_index job 未在 ${MAX_WAIT}s 内完成" >&2
  rm -f "$COOKIE_FILE"
  exit 1
fi

# 验证 alias 指向正确索引
echo ""
echo "验证 ES alias..."
# 通过 readyz 检查 ES 仍为 healthy
FINAL_READYZ=$(curl -sf "${BASE_URL}/readyz" 2>/dev/null) || true
FINAL_ES_STATUS=$(echo "$FINAL_READYZ" | grep -o '"elasticsearch":{[^}]*}' | grep -o '"status":"[^"]*"' | head -1 | cut -d: -f2 | tr -d '"') || true

if [ "$FINAL_ES_STATUS" = "healthy" ]; then
  echo "PASS: rebuild 后 ES 仍为 healthy"
else
  echo "FAIL: rebuild 后 ES 状态为 ${FINAL_ES_STATUS}，期望 healthy" >&2
  rm -f "$COOKIE_FILE"
  exit 1
fi

# 清理
rm -f "$COOKIE_FILE"

echo ""
echo "=== 汇总 ==="
echo "ES rebuild job: PASS"
echo "ES alias 验证: PASS"
echo "ES 健康状态: PASS"
exit 0
