#!/usr/bin/env bash
# ops-gate-healthz-readyz.sh — 验收门禁：/healthz 和 /readyz 端点
#
# 引入动机：design/05-OPERATIONS.md §Health 要求：
#   - /healthz：进程存活，不因 ES/Provider 降级而失败
#   - /readyz：结构化报告 PG/ES/Embedding/Reranker/Jobs/Profile
#   - PG 不可用时 503，ES/Provider 降级时 200+degraded
#
# 前置条件：
#   - 目标 server 可通过 BASE_URL 访问
#   - curl 可用
#
# 用法：
#   BASE_URL=http://localhost:8080 ./scripts/ops-gate-healthz-readyz.sh
#   BASE_URL=http://localhost ./scripts/ops-gate-healthz-readyz.sh  # 通过 web 反代
#
# 退出码：
#   0 = 全部通过
#   1 = healthz 或 readyz 不符合预期
#   2 = 环境不可用（curl 缺失或 BASE_URL 未设置）
set -euo pipefail

BASE_URL="${BASE_URL:-}"
if [ -z "$BASE_URL" ]; then
  echo "ERROR: BASE_URL 未设置" >&2
  echo "用法: BASE_URL=http://localhost:8080 ./scripts/ops-gate-healthz-readyz.sh" >&2
  exit 2
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "ERROR: curl 不可用" >&2
  exit 2
fi

PASS=0
FAIL=0

# --- /healthz 检查 ---
echo "=== /healthz ==="
HEALTHZ_RESP=$(curl -sf -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz" 2>/dev/null) || {
  echo "FAIL: /healthz 请求失败（server 不可达）"
  exit 1
}

if [ "$HEALTHZ_RESP" = "200" ]; then
  echo "PASS: /healthz 返回 200"
  PASS=$((PASS + 1))
else
  echo "FAIL: /healthz 返回 $HEALTHZ_RESP，期望 200"
  FAIL=$((FAIL + 1))
fi

# 验证 healthz 响应体包含 status:ok
HEALTHZ_BODY=$(curl -sf "${BASE_URL}/healthz" 2>/dev/null) || {
  echo "FAIL: 无法读取 /healthz 响应体"
  FAIL=$((FAIL + 1))
}
if echo "$HEALTHZ_BODY" | grep -q '"status":"ok"' || echo "$HEALTHZ_BODY" | grep -q '"status": "ok"'; then
  echo "PASS: /healthz 响应体包含 status:ok"
  PASS=$((PASS + 1))
else
  echo "FAIL: /healthz 响应体缺少 status:ok，实际: $HEALTHZ_BODY"
  FAIL=$((FAIL + 1))
fi

# --- /readyz 检查 ---
echo "=== /readyz ==="
READYZ_CODE=$(curl -sf -o /dev/null -w '%{http_code}' "${BASE_URL}/readyz" 2>/dev/null) || {
  echo "FAIL: /readyz 请求失败（server 不可达）"
  exit 1
}

READYZ_BODY=$(curl -sf "${BASE_URL}/readyz" 2>/dev/null) || {
  echo "FAIL: 无法读取 /readyz 响应体"
  exit 1
}

# readyz 应返回 200（ready 或 degraded）或 503（PG 不可用）
if [ "$READYZ_CODE" = "200" ] || [ "$READYZ_CODE" = "503" ]; then
  echo "PASS: /readyz 返回 $READYZ_CODE（合法状态码）"
  PASS=$((PASS + 1))
else
  echo "FAIL: /readyz 返回 $READYZ_CODE，期望 200 或 503"
  FAIL=$((FAIL + 1))
fi

# 验证 readyz 响应包含 checks 字段
if echo "$READYZ_BODY" | grep -q '"checks"' || echo "$READYZ_BODY" | grep -q '"postgresql"'; then
  echo "PASS: /readyz 响应包含结构化 checks"
  PASS=$((PASS + 1))
else
  echo "FAIL: /readyz 响应缺少结构化 checks，实际: $READYZ_BODY"
  FAIL=$((FAIL + 1))
fi

# 验证 readyz 包含 PG 状态
if echo "$READYZ_BODY" | grep -q '"postgresql"'; then
  echo "PASS: /readyz 包含 postgresql 状态"
  PASS=$((PASS + 1))
else
  echo "FAIL: /readyz 缺少 postgresql 状态"
  FAIL=$((FAIL + 1))
fi

# 验证 readyz 包含 ES 状态
if echo "$READYZ_BODY" | grep -q '"elasticsearch"'; then
  echo "PASS: /readyz 包含 elasticsearch 状态"
  PASS=$((PASS + 1))
else
  echo "FAIL: /readyz 缺少 elasticsearch 状态"
  FAIL=$((FAIL + 1))
fi

# 验证 readyz 包含 pending_jobs
if echo "$READYZ_BODY" | grep -q '"pending_jobs"'; then
  echo "PASS: /readyz 包含 pending_jobs"
  PASS=$((PASS + 1))
else
  echo "FAIL: /readyz 缺少 pending_jobs"
  FAIL=$((FAIL + 1))
fi

# 验证 readyz 包含 failed_jobs
if echo "$READYZ_BODY" | grep -q '"failed_jobs"'; then
  echo "PASS: /readyz 包含 failed_jobs"
  PASS=$((PASS + 1))
else
  echo "FAIL: /readyz 缺少 failed_jobs"
  FAIL=$((FAIL + 1))
fi

# 验证 readyz 包含 active_profile
if echo "$READYZ_BODY" | grep -q '"active_profile"'; then
  echo "PASS: /readyz 包含 active_profile"
  PASS=$((PASS + 1))
else
  echo "FAIL: /readyz 缺少 active_profile"
  FAIL=$((FAIL + 1))
fi

# --- 敏感信息泄漏检查 ---
echo "=== 敏感信息泄漏检查 ==="
if echo "$READYZ_BODY" | grep -qiE '(password|api_key|apikey|secret|token=|bearer )'; then
  echo "FAIL: /readyz 响应可能泄漏敏感信息"
  FAIL=$((FAIL + 1))
else
  echo "PASS: /readyz 响应未检测到敏感信息模式"
  PASS=$((PASS + 1))
fi

echo ""
echo "=== 汇总 ==="
echo "PASS: $PASS"
echo "FAIL: $FAIL"

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
