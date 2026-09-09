# scripts/e2e/lib.sh — E2E 公共库
#
# 引入动机：让 run-e2e.sh 与 setup-wsl-docker.sh 共享环境探测、HTTP 封装、
# JSON 解析、等待/断言工具；并在 Windows GitBash / WSL / Linux 上透明工作。
#
# 用法：source scripts/e2e/lib.sh （须先 cd 到仓库根）
#
# 设计原则：
#   - 跨 Windows/WSL/Linux 统一入口：若在 Windows GitBash 且检测到 WSL，则自动
#     把命令转交给 wsl.exe（E2E_WSL_DISTRO 可覆盖发行版）；内部用 E2E_INNER=1
#     防止递归转交。
#   - secrets 只从仓库外/本地读入，绝不写入被 git 跟踪的文件。
#   - HTTP 请求统一携带 session cookie 与 CSRF header，失败即打印可读错误。

if [ -z "${E2E_LIB_LOADED:-}" ]; then

# ============ 基础工具 ============
E2E_LIB_LOADED=1

_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 颜色（非 tty 自动禁用）
if [ -t 1 ]; then
  C_RED=$'\033[31m'; C_GRN=$'\033[32m'; C_YLW=$'\033[33m'; C_BLU=$'\033[34m'; C_RST=$'\033[0m'
else
  C_RED=""; C_GRN=""; C_YLW=""; C_BLU=""; C_RST=""
fi

step() { echo; printf '%s▶ [%s] %s%s\n' "${C_BLU}" "$(date +%H:%M:%S)" "$*" "${C_RST}"; }
ok()   { printf '%s✓ %s%s\n' "${C_GRN}" "$*" "${C_RST}"; }
warn() { printf '%s⚠ %s%s\n' "${C_YLW}" "$*" "${C_RST}" >&2; }
die()  { printf '%s✗ %s%s\n' "${C_RED}" "$*" "${C_RST}" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "缺少依赖命令: $1"; }

# ============ 环境探测 ============

# detect_platform：设置 PLATFORM=gitbash|wsl|linux，并输出 ROOT（仓库根，Linux 路径）
detect_platform() {
  local kernel
  kernel="$(uname -s 2>/dev/null || echo Windows)"

  case "$kernel" in
    MINGW*|MSYS*|CYGWIN*)
      PLATFORM=gitbash
      # 找到 WSL
      if command -v wsl.exe >/dev/null 2>&1; then
        HAVE_WSL=1
      else
        HAVE_WSL=0
      fi
      ;;
    Linux)
      if [ -n "${WSL_DISTRO_NAME:-}" ] || (uname -r 2>/dev/null | grep -qi microsoft); then
        PLATFORM=wsl
      else
        PLATFORM=linux
      fi
      HAVE_WSL=0
      ;;
    *)
      PLATFORM=gitbash
      HAVE_WSL=0
      ;;
  esac
}

# wsl_mnt_path：把 Windows 路径转成 WSL 内路径（/mnt/<盘>/...）
# 支持：/mnt/d/...（已是）、D:\... 或 D:/...、以及 GitBash 风格 /d/...
wsl_mnt_path() {
  local p="$1"
  case "$p" in
    /mnt/*) echo "$p" ;;
    /[a-zA-Z]/*)
      # GitBash 风格 /d/Partitura → /mnt/d/Partitura
      local drive="${p:1:1}"
      echo "/mnt/$(echo "$drive" | tr 'A-Z' 'a-z')${p:2}"
      ;;
    [A-Za-z]:\\*|[A-Za-z]:/*)
      local drive="${p:0:1}"
      echo "/mnt/$(echo "$drive" | tr 'A-Z' 'a-z')${p:2}"
      ;;
    *)
      echo "$p"
      ;;
  esac
}

# root_for_current_shell：探测本 shell 看到的仓库根（Linux 路径）。
# E2E_PROJECT_ROOT 优先。
root_for_current_shell() {
  if [ -n "${E2E_PROJECT_ROOT:-}" ]; then
    ROOT="$(wsl_mnt_path "$E2E_PROJECT_ROOT")"
    return
  fi
  # 从本脚本位置向上定位仓库根
  local dir="$_script_dir"
  while [ "$dir" != "/" ] && [ "$dir" != "." ]; do
    if [ -f "$dir/docker-compose.yml" ] && [ -d "$dir/server" ] && [ -d "$dir/web" ]; then
      ROOT="$dir"
      return
    fi
    dir="$(dirname "$dir")"
  done
  ROOT="$(pwd)"
}

# run_in_wsl <bash_code>：在 Windows GitBash 下把逻辑转交给 WSL 执行。
# 自动把 E2E_* 环境变量与秘密文件路径透传。要求 bash code 是单字符串命令。
run_in_wsl() {
  local code="$1"
  local distro="${E2E_WSL_DISTRO:-Debian}"
  local wslroot
  wslroot="$(wsl_mnt_path "$ROOT")"

  # 构建环境透传（仅 E2E_* 白名单 + 关键变量）
  local envs=()
  for kv in $(env | grep -E '^E2E_[A-Z0-9_]+=' || true); do
    envs+=("$kv")
  done
  # E2E_INNER 防止递归
  envs+=("E2E_INNER=1" "E2E_PROJECT_ROOT=$wslroot")
  if [ -n "${E2E_SECRET_FILE:-}" ]; then
    envs+=("E2E_SECRET_FILE=$(wsl_mnt_path "$E2E_SECRET_FILE")")
  fi

  local envstr=""
  local kv
  for kv in "${envs[@]}"; do
    # 值可能含空格，做单引号转义
    local key="${kv%%=*}"
    local val="${kv#*=}"
    val="$(printf '%s' "$val" | sed "s/'/'\\\\''/g")"
    envstr="$envstr $key='$val'"
  done

  local full="cd '$wslroot' && $envstr bash '$wslroot/scripts/e2e/run-inner.sh' '$code'"
  warn "转交到 WSL($distro) 执行…"
  exec wsl.exe -d "$distro" -- bash -lc "$full"
}

# maybe_delegate_to_wsl <code>：若处于 GitBash 且有 WSL，则转交并退出当前 shell；
# 否则（linux/wsl/无 WSL 的 gitbash）直接以 bash -lc "$code" 执行并退出。
# 统一入口：调用后不会返回（exec 或 exit）。
maybe_delegate_to_wsl() {
  detect_platform
  if [ "$PLATFORM" = gitbash ]; then
    if [ -n "${E2E_INNER:-}" ]; then
      die "内部错误：已在 WSL 内执行却仍进入 delegate 分支"
    fi
    if [ "$HAVE_WSL" = 1 ]; then
      run_in_wsl "$1"
    else
      warn "当前是 GitBash 但未检测到 WSL；将在本地执行（需要本机已有 docker）"
      exec bash -lc "$1"
    fi
  else
    exec bash -lc "$1"
  fi
  exit 0
}

# ============ Secrets ============

# secret_load：从候选路径加载 env 文件到当前 shell。找不到时按提示退出。
secret_load() {
  local candidates=()
  if [ -n "${E2E_SECRET_FILE:-}" ]; then candidates+=("$E2E_SECRET_FILE"); fi
  candidates+=("$HOME/.config/partitura/e2e.env")
  # 仓库内兜底文件（仅在 E2E_ALLOW_REPO_SECRET=1 时使用，防止误入 git）
  if [ "${E2E_ALLOW_REPO_SECRET:-0}" = 1 ]; then
    candidates+=("$_script_dir/.secrets.env")
  fi

  local f
  for f in "${candidates[@]}"; do
    if [ -f "$f" ]; then
      # shellcheck disable=SC1090
      set -a; . "$f"; set +a
      E2E_SECRET_FILE="$f"
      return 0
    fi
  done

  warn "未找到 E2E 密钥文件。请先创建（两种方式任一）："
  warn "  方式A(推荐,仓库外):  mkdir -p ~/.config/partitura && cp scripts/e2e/env.example ~/.config/partitura/e2e.env"
  warn "    然后编辑 ~/.config/partitura/e2e.env 填写 ADMIN_PASSWORD 与 PROVIDER_API_KEY"
  warn "  方式B(本地临时):  cp scripts/e2e/env.example scripts/e2e/.secrets.env && 编辑之（.secrets.env 已被 gitignore）"
  die "缺少密钥文件，终止。"
}

# require_envs：校验一组 E2E_ 变量非空（且不含 <REPLACE_ME>）
require_envs() {
  local missing=0
  local v
  for v in "$@"; do
    local val="${!v:-}"
    if [ -z "$val" ] || [ "$val" = "<REPLACE_ME>" ]; then
      warn "环境变量 $v 未正确设置（当前值: '${val}'），请检查 $(basename "$E2E_SECRET_FILE")"
      missing=1
    fi
  done
  [ "$missing" = 0 ] || die "缺少必要环境变量，终止。"
}

# ============ 状态/临时目录 ============

# state_dir 初始化：/tmp 下按项目名建目录，报告写 $STATE/report.md，cookie/job body 存 $STATE
init_state() {
  STATE="${TMPDIR:-/tmp}/partitura-e2e-$(date +%s)-$$"
  mkdir -p "$STATE"
  COOKIES="$STATE/cookies.txt"
  CSRF=""
  REPORT="$STATE/report.md"
  : > "$REPORT"
}

# ============ JSON / HTTP ============

have_jq() { command -v jq >/dev/null 2>&1; }

# pyj <file> <python-expr-on-d>: 用 python3 对已解析 JSON 对象 d 求值并打印。
# 统一唯一 JSON 引擎为 python3（Debian/WSL 均自带；setup 也会安装）。
# expr 示例：d["user"]["id"]、[r["document_id"] for r in d["results"]]、
#           [x for x in d["jobs"] if x["Type"]=="index_document"]
# expr 经 argv 传入（不嵌入 heredoc），避免 shell/python 引号冲突。
pyj() {
  local file="$1" expr="$2"
  if ! command -v python3 >/dev/null 2>&1; then
    echo ""
    return 1
  fi
  python3 - "$file" "$expr" <<'PYEOF'
import json,sys
try:
    d=json.load(open(sys.argv[1],encoding="utf-8"))
except Exception as e:
    sys.stderr.write("pyj: 解析失败 %s\n" % e)
    sys.exit(1)
try:
    r=eval(sys.argv[2])
    if isinstance(r,(list,dict)):
        print(json.dumps(r,ensure_ascii=False))
    else:
        print(r)
except Exception as e:
    sys.stderr.write("pyj: 表达式失败 %s\n" % e)
    sys.exit(1)
PYEOF
}

# json_extract 通用：(go | python fallback) 从文件用 jq 语法（兼容性转换：形如 d.foo 表达式）
json_get() {
  local file="$1" expr="$2"
  # 统一交由 python3 引擎（jq 语法转 python dict 访问仅支持简单层级，复杂请直接用 pyj）
  pyj "$file" "$expr" 2>/dev/null || echo ""
}

# 简单路径提取：json_path <file> "results" 返回数组字段值等 —— 统一用 pyj
json_path() {
  local file="$1" key="$2"
  pyj "$file" "d.get(\"$key\")" 2>/dev/null || echo ""
}

# str_list_of <file> <expr-to-list-of-strings>: 输出逐行（用于多行 JSON 数组）
pyj_lines() {
  local file="$1" expr="$2"
  pyj "$file" "$expr" 2>/dev/null | python3 -c 'import sys,json
try:
    v=json.load(sys.stdin)
except Exception:
    sys.exit(0)
if isinstance(v,list):
    for x in v: print(x)
else:
    print(v)'
}

# api <METHOD> <path> [json-body]：带 session+CSRF 的 HTTP 封装。
# body 一律先写入临时文件再以 --data-binary @file 发送，杜绝引号/转义污染。
# 成功输出响应体文件路径到 $LAST_BODY（并 $LAST_CODE），返回 0/1。
# 200/201/202/204 视为成功；其余视为失败并打印 body。
api() {
  local method="$1" path="$2"
  local bodyfile=""
  if [ $# -ge 3 ]; then
    bodyfile="$STATE/body_$(date +%s%N).json"
    printf '%s' "$3" > "$bodyfile"
  fi
  local url="${E2E_WEB_BASE_URL}${path}"
  # 始终创建空 cookie 文件并同时 -b/-c，确保登录等 Set-Cookie 能落盘供后续使用
  [ -f "$COOKIES" ] || : > "$COOKIES"
  local curl_args=(-sS -m 120 -w '\n%{http_code}' -X "$method" "$url")
  curl_args+=(-H 'Content-Type: application/json')
  curl_args+=(-b "$COOKIES" -c "$COOKIES")
  if [ -n "$CSRF" ]; then
    curl_args+=(-H "X-CSRF-Token: $CSRF")
  fi
  if [ -n "$bodyfile" ]; then
    curl_args+=(--data-binary "@$bodyfile")
  fi

  local out
  out="$(curl "${curl_args[@]}" 2>&1)" || {
    LAST_CODE="000"
    LAST_BODY="$STATE/resp_$(date +%s%N).json"
    printf '%s\n' "$out" > "$LAST_BODY"
    printf '%s✗ curl 失败  %s %s%s\n' "${C_RED}" "$method" "$path" "${C_RST}" >&2
    printf '  %s\n' "$(printf '%s' "$out" | head -c 800)" >&2
    return 1
  }
  LAST_CODE="$(printf '%s\n' "$out" | tail -1)"
  LAST_BODY="$STATE/resp_$(date +%s%N).json"
  printf '%s\n' "$out" | head -n -1 > "$LAST_BODY"

  if [ "$LAST_CODE" = "200" ] || [ "$LAST_CODE" = "201" ] || [ "$LAST_CODE" = "202" ] || [ "$LAST_CODE" = "204" ]; then
    # 某些端点（login/bootstrap）响应体含 csrf_token，自动续期
    local t
    t="$(pyj "$LAST_BODY" 'd.get("csrf_token","")' 2>/dev/null || echo '')"
    [ -n "$t" ] && CSRF="$t"
    return 0
  fi
  printf '%s✗ HTTP %s  %s %s%s\n' "${C_RED}" "$LAST_CODE" "$method" "$path" "${C_RST}" >&2
  if [ -s "$LAST_BODY" ]; then
    printf '  body: %s\n' "$(head -c 1200 "$LAST_BODY")" >&2
  fi
  return 1
}

# require_code <expect>：断言 LAST_CODE == expect
require_code() {
  local want="$1"
  if [ "$LAST_CODE" != "$want" ]; then
    die "期望 HTTP $want，实际 $LAST_CODE（body: $(head -c 1500 "$LAST_BODY" 2>/dev/null)）"
  fi
}

# assert_contains <haystack-file-or-str> <needle> <msg>
assert_contains() {
  local hay="$1" needle="$2" msg="$3"
  if printf '%s' "$hay" | grep -qF -- "$needle"; then
    ok "$msg"
  else
    die "断言失败: $msg （未找到 '$needle'）"
  fi
}

# log_report <step> <status> <detail>
log_report() {
  printf -- '- **%s**: %s — %s\n' "$1" "$2" "$3" >> "$REPORT"
}

# wait_for <desc> <timeout_s> <cmd...>: 轮询直到命令成功
wait_for() {
  local desc="$1" timeout_s="$2"
  shift 2
  local deadline=$(( $(date +%s) + timeout_s ))
  while : ; do
    if "$@" >/dev/null 2>&1; then
      ok "$desc"
      return 0
    fi
    if [ "$(date +%s)" -gt "$deadline" ]; then
      die "等待超时(${timeout_s}s): $desc"
    fi
    sleep 2
  done
}

# login_admin：使用当前 secret 登录，成功设置 CSRF
login_admin() {
  step "管理员登录"
  local body
  body=$(printf '{"username":"%s","password":"%s"}' "$E2E_ADMIN_USERNAME" "$E2E_ADMIN_PASSWORD")
  if ! api POST /api/auth/login "$body"; then
    die "管理员登录失败"
  fi
  require_code 200
  CSRF="$(pyj "$LAST_BODY" 'd.get("csrf_token","")')"
  [ -n "$CSRF" ] || die "登录响应无 csrf_token"
  ok "登录成功，csrftoken 已获取"
}

# wait_job_done <label> <timeout>: 轮询 /api/admin/jobs，直到无 pending/running。
# job 列表为 Go 结构（字段名首字母大写：ID/Type/Status/Payload…），用 python 引擎处理。
wait_job_done() {
  local label="$1" timeout_s="${2:-300}"
  step "等待后台任务完成: $label"
  local deadline=$(( $(date +%s) + timeout_s ))
  while : ; do
    if api GET "/api/admin/jobs?limit=100"; then
      local pending
      pending="$(pyj "$LAST_BODY" 'len([j for j in (d.get("jobs") or []) if j.get("Status") in ("pending","running")])')"
      if [ -n "$pending" ] && [ "$pending" = "0" ]; then
        ok "无 pending/running 任务: $label"
        return 0
      fi
    else
      warn "查询 jobs 失败，重试…"
    fi
    if [ "$(date +%s)" -gt "$deadline" ]; then
      die "等待任务完成超时: $label"
    fi
    sleep 3
  done
}

fi
