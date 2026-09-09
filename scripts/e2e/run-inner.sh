#!/bin/bash
# scripts/e2e/run-inner.sh — E2E 内部入口（仅在 WSL/Linux 内运行）
#
# 引入动机：lib.sh 的转交机制需要一个位于仓库内、可在任意路径 cd 后执行的
# 统一入口文件。用法：bash scripts/e2e/run-inner.sh '<代码>'
# 直接执行代码，不参与转交判断（E2E_INNER=1 已由调用方注入）。
set -euo pipefail

CODE="${1:?缺少要执行的代码}"
eval "$CODE"
