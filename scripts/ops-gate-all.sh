#!/usr/bin/env bash
# ops-gate-all.sh — 生产运维验收总门禁
#
# 引入动机：统一执行所有运维验收门禁脚本，汇总结果。
# design/06-IMPLEMENTATION.md Phase 6 要求完整运维验收。
#
# 前置条件：
#   - 各子脚本的前置条件
#   - Docker + Docker Compose 可用（大部分脚本需要）
#   - Compose 服务已启动
#
# 用法：
#   BASE_URL=http://localhost ./scripts/ops-gate-all.sh
#   BASE_URL=http://localhost:8080 ./scripts/ops-gate-all.sh  # 直连 server
#
# 退出码：
#   0 = 全部通过
#   1 = 至少一个门禁失败
#   2 = 环境不可用
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_URL="${BASE_URL:-}"

PASS=0
FAIL=0
BLOCKED=0
RESULTS=()

run_gate() {
  local name="$1"
  local script="$2"
  local need_base_url="$3"

  echo ""
  echo "============================================"
  echo "  执行: $name"
  echo "============================================"

  local cmd
  if [ "$need_base_url" = "true" ] && [ -n "$BASE_URL" ]; then
    cmd="BASE_URL=${BASE_URL} bash ${SCRIPT_DIR}/${script}"
  else
    cmd="bash ${SCRIPT_DIR}/${script}"
  fi

  if eval "$cmd"; then
    echo "  结果: PASS"
    PASS=$((PASS + 1))
    RESULTS+=("PASS  $name")
  else
    local exit_code=$?
    if [ $exit_code -eq 2 ]; then
      echo "  结果: BLOCKED（环境不可用）"
      BLOCKED=$((BLOCKED + 1))
      RESULTS+=("BLOCK $name")
    else
      echo "  结果: FAIL"
      FAIL=$((FAIL + 1))
      RESULTS+=("FAIL  $name")
    fi
  fi
}

echo "Partitura 生产运维验收门禁"
echo "=========================="
echo "BASE_URL: ${BASE_URL:-未设置}"
echo ""

run_gate "healthz/readyz" "ops-gate-healthz-readyz.sh" "true"
run_gate "数据库迁移" "ops-gate-migrate.sh" "false"
run_gate "pg_dump 备份恢复" "ops-gate-pg-backup-restore.sh" "false"
run_gate "ES rebuild + alias" "ops-gate-es-rebuild-alias.sh" "true"
run_gate "SIGTERM 优雅关闭" "ops-gate-sigterm.sh" "false"
run_gate "Job 恢复" "ops-gate-job-recovery.sh" "true"
run_gate "Web 登录/文档/搜索" "ops-gate-web-paths.sh" "true"

echo ""
echo "=========================="
echo "  验收汇总"
echo "=========================="
for r in "${RESULTS[@]}"; do
  echo "  $r"
done
echo ""
echo "PASS:   $PASS"
echo "FAIL:   $FAIL"
echo "BLOCKED: $BLOCKED"

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
if [ "$BLOCKED" -gt 0 ] && [ "$PASS" -eq 0 ]; then
  exit 2
fi
exit 0
