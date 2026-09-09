#!/bin/bash
# scripts/e2e/setup-wsl-docker.sh — 一键准备 Linux/WSL 的 Docker 运行环境
#
# 引入动机：目标机器（WSL Debian / Linux）默认没有 docker，需安装 docker 引擎、
# docker compose v2 插件与 jq/python3，并启动 docker 服务。
#
# 跨平台：Windows GitBash 下运行会自动转交 WSL(E2E_WSL_DISTRO) 执行；Linux/WSL 直接执行。
# 幂等：已安装则跳过安装，仅做可用性自检。
#
# 用法：
#   bash scripts/e2e/setup-wsl-docker.sh [--no-sudo] [--help]
#   --no-sudo  当前用户已是 root（如部分容器内）则跳过 sudo。
set -euo pipefail

_self="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$_self/lib.sh"
detect_platform
root_for_current_shell
[ -z "${E2E_WSL_DISTRO:-}" ] && E2E_WSL_DISTRO=Debian
export E2E_WSL_DISTRO

NO_SUDO=0
for a in "$@"; do
  case "$a" in
    --no-sudo) NO_SUDO=1 ;;
    --help|-h) sed -n '1,20p' "$0" >&2; exit 0 ;;
    *) echo "未知参数: $a" >&2; exit 2 ;;
  esac
done

if [ "$PLATFORM" = gitbash ] && [ -z "${E2E_INNER:-}" ]; then
  distro="${E2E_WSL_DISTRO:-Debian}"
  wslroot="$(wsl_mnt_path "$ROOT")"
  export E2E_INNER=1 E2E_PROJECT_ROOT="$wslroot" E2E_WSL_DISTRO="$distro" E2E_NO_SUDO="$NO_SUDO"
  warn "转交到 WSL($distro) 执行本脚本…"
  exec wsl.exe -d "$distro" -- bash -lc "cd '$wslroot' && bash scripts/e2e/setup-wsl-docker.sh" bash
fi

NO_SUDO="${E2E_NO_SUDO:-0}"

SUDO=""
if [ "$NO_SUDO" = 1 ] || [ "$(id -u)" = 0 ]; then
  SUDO=""
else
  need sudo
  SUDO="sudo"
fi

do_setup() {
  echo "════════ Partitura E2E 环境安装（$PLATFORM）════════"
  echo "平台: $PLATFORM | Root: $ROOT"

  # 1. 基础工具
  if ! command -v curl >/dev/null 2>&1 || ! command -v python3 >/dev/null 2>&1; then
    echo ">> 安装 curl/python3…"
    $SUDO apt-get update -y
    $SUDO apt-get install -y curl python3 jq ca-certificates
  fi

  # 2. Docker
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    echo ">> Docker 已安装: $(docker --version) / $(docker compose version 2>&1 | head -1)"
  else
    echo ">> 安装 docker（docker.io + docker-compose v2 插件）…"
    $SUDO apt-get update -y
    # Debian: docker.io 26.x + docker-compose(v2 CLI)。注意 WSL root 会话对 passwd 中
    # 用户名解析存在缺陷时 gpasswd 会失败，此处 usermod 同样可能失败——不致命，仅告警。
    $SUDO apt-get install -y docker.io docker-compose 2>&1 | tail -3 || $SUDO apt-get install -y docker.io docker-compose-v2 2>&1 | tail -3
  fi

  # 3. 启动 docker（systemd 或 service）
  if ! docker version >/dev/null 2>&1; then
    echo ">> 启动 docker 服务…"
    if command -v systemctl >/dev/null 2>&1; then
      $SUDO systemctl enable --now docker 2>/dev/null || true
      $SUDO systemctl start docker 2>/dev/null || true
    elif command -v service >/dev/null 2>&1; then
      $SUDO service docker start || true
    fi
    # 等待就绪（最多 30s）
    for _ in $(seq 1 15); do
      docker version >/dev/null 2>&1 && break
      sleep 2
    done
  fi

  docker version >/dev/null 2>&1 || die "docker 仍不可用。在 WSL 中请确认 PID1 是 systemd；否则手动执行: sudo service docker start"
  docker compose version >/dev/null 2>&1 || die "docker compose v2 缺失（安装 docker-compose-plugin）"
  echo ">> Docker 就绪：$(docker version --format '{{.Server.Version}}' 2>/dev/null) / $(docker compose version 2>&1 | head -1)"

  # 4. 当前用户加入 docker 组（避免每次 sudo）
  if ! id -nG 2>/dev/null | grep -qw docker; then
    if command -v usermod >/dev/null 2>&1 && [ "$(id -u)" != 0 ]; then
      $SUDO usermod -aG docker "$(id -un)" || true
      echo ">> 已将 $(id -un) 加入 docker 组（需重新登录 WSL 终端生效；本次会话内可用 sudo docker）"
    fi
  fi

  echo "════════ 环境就绪 ════════"
  echo "下一步：配置密钥并运行"
  echo "  mkdir -p ~/.config/partitura && cp scripts/e2e/env.example ~/.config/partitura/e2e.env"
  echo "  编辑 ~/.config/partitura/e2e.env 填写 ADMIN_PASSWORD / PROVIDER_API_KEY"
  echo "  bash scripts/e2e/run-e2e.sh"
}

# --- 主入口 ---
main() {
  do_setup
}
main "$@"
