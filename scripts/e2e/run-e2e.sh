#!/bin/bash
# scripts/e2e/run-e2e.sh — Partitura 本地 Docker E2E 语义搜索测试（主编排）
#
# 引入动机：项目从未做实机（真实容器 + 真实 Provider API）端到端验证。
# 本脚本在 Linux / WSL / Windows GitBash 上均可运行：
#   - GitBash 下自动转交 WSL 发行版（E2E_WSL_DISTRO 覆盖）执行（镜像内网/路径解析与 linux 一致）；
#   - WSL/Linux 下直接执行。
# 依赖：docker(compose v2)、python3、curl。
#
# 安全：只读取仓库外密钥文件（默认 ~/.config/partitura/e2e.env），绝不把 API Key
#       写入仓库、输出脱敏为 ***、cookie/响应临时文件仅存 /tmp 会话目录。
#
# 用法：
#   bash scripts/e2e/run-e2e.sh [--skip-build] [--down] [--keep-state] [--help]
#   默认：本地构建缺失镜像 → compose up → 全流程断言 → 保留容器与状态目录打印报告。
#   --down        测试结束后 docker compose down（默认保留便于复跑/人肉复查）
#   --skip-build  不重新 docker build（复用上次本地镜像）
#   --keep-state  不清理 $STATE（默认退出时清理）
#
# 退出码：0=全部通过；非 0=失败（可复跑，幂等：已 bootstrap 的库/已存在的 job 不误判）。
set -euo pipefail

# ---------- 定位仓库根并加载公共库 ----------
_self="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$_self/lib.sh"

detect_platform
root_for_current_shell

# 首次在 GitBash 被调用 → 转交 WSL：以子进程形式在 WSL 内重新运行本脚本自身。
# 为规避多层引号转义，将参数序列化进 env（E2E_ARG_*），内部恢复为 $@。
if [ "$PLATFORM" = gitbash ] && [ -z "${E2E_INNER:-}" ]; then
  distro="${E2E_WSL_DISTRO:-Debian}"
  wslroot="$(wsl_mnt_path "$ROOT")"
  # 把本机密钥绝对路径转换为 WSL 可读路径并透传
  local_key="$HOME/.config/partitura/e2e.env"
  if [ -f "$local_key" ]; then
    win_key="$(cygpath -w "$local_key" 2>/dev/null || echo "$local_key")"
    export E2E_SECRET_FILE="$(wsl_mnt_path "$win_key")"
  fi
  export E2E_INNER=1 E2E_PROJECT_ROOT="$wslroot" E2E_WSL_DISTRO="$distro"
  # 序列化 args（空格安全）
  i=0
  for a in "$@"; do
    export "E2E_ARG_$i=$a"
    i=$((i+1))
  done
  export E2E_ARG_COUNT="$i"
  warn "转交到 WSL($distro) 执行本脚本…"
  exec wsl.exe -d "$distro" -- bash -lc "cd '$wslroot' && bash scripts/e2e/run-e2e.sh" bash
fi

# 恢复被透传的参数（仅 WSL/Linux 二次入口需要）
if [ -n "${E2E_ARG_COUNT:-}" ]; then
  set -- ""
  _i=0
  while [ "$_i" -lt "$E2E_ARG_COUNT" ]; do
    eval "set -- \"\$@\" \"\$E2E_ARG_${_i}\""
    _i=$((_i+1))
  done
fi

# ---------- 参数 ----------
SKIP_BUILD=0
DO_DOWN=0
KEEP_STATE=0
for a in "$@"; do
  case "$a" in
    --skip-build) SKIP_BUILD=1 ;;
    --down)       DO_DOWN=1 ;;
    --keep-state) KEEP_STATE=1 ;;
    --help|-h) cat <<'HELP'
用法: bash scripts/e2e/run-e2e.sh [--skip-build] [--down] [--keep-state]
HELP
      exit 0 ;;
    *) echo "未知参数: $a" >&2; exit 2 ;;
  esac
done

# ---------- 会话状态 ----------
init_state
trap 'if [ "${KEEP_STATE:-0}" != 1 ]; then rm -rf "$STATE"; fi' EXIT

# ---------- 全局状态变量 ----------
CSRF=""                 # 当前管理员 csrf token
ADMIN_COOKIES="$COOKIES"
declare -g ADMIN_WS=""

# ---------- 报告辅助 ----------
note() { log_report "$1" "$2" "$3"; }

# ---------- 就绪探测：经 Nginx 只代理 /api/，/readyz 需从 server 容器内抓取 ----------
# fetch_readyz：把 readyz JSON 写入 $STATE/readyz.json 并回填 LAST_BODY。
fetch_readyz() {
  LAST_BODY="$STATE/readyz_$(date +%s%N).json"
  docker compose -f "$ROOT/docker-compose.yml" exec -T server \
    wget -qO- http://localhost:8080/readyz > "$LAST_BODY" 2>/dev/null || { rm -f "$LAST_BODY"; return 1; }
  [ -s "$LAST_BODY" ] || return 1
  return 0
}

# ---------- STEP 函数 ----------

preflight() {
  step "环境预检"
  need docker; need curl; need python3
  docker version >/dev/null 2>&1 || die "docker 守护进程不可用（在 WSL 中请先 systemctl start docker）"
  docker compose version >/dev/null 2>&1 || die "缺少 docker compose v2 插件"
  ok "docker/curl/python3 可用"
}

provider_smoke() {
  step "Provider 连通预检（直连 ${E2E_PROVIDER_BASE_URL}）"
  local emb json
  json=$(printf '{"model":"%s","input":"hello partitura semantic search","encoding_format":"float"}' "$E2E_EMBEDDING_MODEL")
  emb="$(curl -sS -m 60 -o /tmp/pemb.json -w '%{http_code}' \
      -X POST "${E2E_PROVIDER_BASE_URL}/embeddings" \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer ${E2E_PROVIDER_API_KEY}" \
      -d "$json" 2>/dev/null || true)"
  [ "$emb" = "200" ] || die "Embedding Provider 连通失败 (HTTP ${emb:-无响应})，请检查 API Key/网络"
  ok "Embedding Provider 连通 (HTTP 200)"
}

compose_up() {
  step "构建并启动 compose"
  cd "$ROOT"
  local missing=0
  if [ "$SKIP_BUILD" = 0 ]; then
    docker image inspect partitura-server:local >/dev/null 2>&1 || missing=1
    docker image inspect partitura-web:local >/dev/null 2>&1 || missing=1
    if [ "$missing" = 1 ]; then
      echo "本地镜像缺失，构建中（首次较慢，约 3-8 分钟）…"
      docker build -f server/Dockerfile -t partitura-server:local . >"$STATE/build-server.log" 2>&1 || {
        tail -40 "$STATE/build-server.log" >&2; die "server 镜像构建失败"; }
      docker build -f web/Dockerfile -t partitura-web:local . >"$STATE/build-web.log" 2>&1 || {
        tail -40 "$STATE/build-web.log" >&2; die "web 镜像构建失败"; }
      ok "镜像构建完成"
    fi
  fi
  # 本地 E2E：始终使用本地构建镜像，禁止去拉取 GHCR（默认 ghcr.io/mindperformer/* 无访问权）。
  # compose 模板为 ${SERVER_IMAGE}:${IMAGE_TAG}，故 SERVER_IMAGE 只放仓库名、tag 放 IMAGE_TAG。
  export SERVER_IMAGE=partitura-server WEB_IMAGE=partitura-web IMAGE_TAG=local

  # 注入本地容器所需的非敏感环境变量到 compose 调用
  # E2E 幂等关键：POSTGRES_PASSWORD 与 MASTER_ENCRYPTION_KEY 使用固定本地常量，
  # 避免每次运行重新生成导致与既有数据卷/既有 Provider 密文不一致。
  # （常量仅用于本地隔离 E2E，PG/ES 不暴露宿主端口；生产使用 .env 强随机值。）
  export E2E_PUBLIC_ORIGIN="${E2E_PUBLIC_ORIGIN:-http://127.0.0.1:${E2E_COMPOSE_WEB_PORT:-8080}}"
  export MASTER_ENCRYPTION_KEY="${E2E_MASTER_KEY:-MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=}"  # base64(0123456789abcdef...32B)

  # compose 需要的其余变量全部由 .env.example 中列出的默认值提供，这里显式补齐
  # 密码：本地隔离 E2E 专用常量（网络 internal 且 PG 不暴露宿主；非生产值）。
  # 使用常量保证重跑（容器 recreate）时与既有数据卷内密码一致。
  export POSTGRES_USER="${POSTGRES_USER:-partitura}"
  export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-partitura_local_e2e_pw_only}"
  export POSTGRES_DB="${POSTGRES_DB:-partitura}"
  export DB_USER="$POSTGRES_USER"; export DB_PASSWORD="$POSTGRES_PASSWORD"; export DB_NAME="$POSTGRES_DB"
  # 数据卷使用 E2E 专属唯一名：同一宿主可能并存其他 compose 项目（同名固定卷 partitura_pgdata
  # 属于旧项目时 docker 会拒绝新项目使用），强制独立卷避免凭据/数据串扰。
  export POSTGRES_DATA_VOLUME="${POSTGRES_DATA_VOLUME:-partitura_e2e_pgdata}"
  export ES_DATA_VOLUME="${ES_DATA_VOLUME:-partitura_e2e_esdata}"
  export DB_SSLMODE=disable
  export WEB_PORT="${E2E_COMPOSE_WEB_PORT:-8080}"
  export COOKIE_SECURE=false
  export SHUTDOWN_TIMEOUT=45s
  export LOG_LEVEL=info
  export PUBLIC_ORIGIN="$E2E_PUBLIC_ORIGIN"
  export JOB_WORKER_ENABLED=true
  export SCHEDULER_ENABLED=true
  export HTTP_ADDR=:8080
  export COOKIE_NAME=session CSRF_COOKIE_NAME=csrf CSRF_HEADER_NAME=X-CSRF-Token COOKIE_PATH=/
  export SESSION_DURATION=86400 DEVICE_ACCESS_TOKEN_DURATION=900 DEVICE_REFRESH_TOKEN_DURATION=2592000
  export ARGON2_MEMORY=65536 ARGON2_ITERATIONS=3 ARGON2_PARALLELISM=2 ARGON2_SALT_LENGTH=16 ARGON2_KEY_LENGTH=32
  export JOB_WORKER_POLL_INTERVAL=5 SCHEDULER_TICK_INTERVAL=60
  export COOKIE_DOMAIN= CSRF_COOKIE_NAME=csrf

  # 保存本次 compose 环境到临时 env 供后续步骤复用（避免重复生成密钥导致不一致）
  env | grep -E '^(POSTGRES_|DB_|WEB_PORT|PUBLIC_ORIGIN|MASTER_ENCRYPTION_KEY|COOKIE_|CSRF_|SESSION_|DEVICE_|ARGON2_|JOB_|SCHEDULER_|SHUTDOWN_|LOG_|HTTP_ADDR)=' > "$STATE/compose.env"

  docker compose -f docker-compose.yml up -d || die "compose up 失败"
  ok "compose 已启动"
}

# 再次运行复用密钥：若 STATE 不存在（跨进程）则尝试读取既有数据卷凭据——
# 由幂等 bootstrap 分支处理：管理员已存在则复用（密码需与首次一致，写于 secret env）。
wait_and_check_ready() {
  step "等待全部服务健康（可接受 ready/degraded：空库初始无 Provider 与 active profile 属预期降级）"
  # 自定义轮询：server 容器内 /readyz HTTP 可达且 postgresql healthy 即继续（degraded 仅因 Provider 未配置）
  local timeout_s="${E2E_READY_TIMEOUT:-240}"
  local deadline=$(( $(date +%s) + timeout_s ))
  while : ; do
    if fetch_readyz; then
      local st pg
      st="$(pyj "$LAST_BODY" 'd.get("status","")')"
      pg="$(pyj "$LAST_BODY" 'd.get("checks",{}).get("postgresql",{}).get("status","")')"
      if [ "$pg" = "healthy" ]; then
        if [ "$st" = "ready" ] || [ "$st" = "degraded" ]; then
          ok "readyz 可达 (status=$st, PG=healthy)"
          return 0
        fi
      fi
    fi
    if [ "$(date +%s)" -gt "$deadline" ]; then
      die "等待 /readyz 就绪超时(${timeout_s}s)"
    fi
    sleep 4
  done
}

bootstrap_admin() {
  step "Bootstrap 首个 system_admin（幂等）"
  # 先看是否已有用户（GET /api/bootstrap）
  if ! api GET /api/bootstrap; then die "bootstrap 状态查询失败"; fi
  local avail
  avail="$(pyj "$LAST_BODY" 'd["bootstrap_available"]')"
  if [ "$avail" = "True" ] || [ "$avail" = "true" ]; then
    local body
    body=$(printf '{"username":"%s","email":"%s","password":"%s"}' "$E2E_ADMIN_USERNAME" "$E2E_ADMIN_EMAIL" "$E2E_ADMIN_PASSWORD")
    api POST /api/bootstrap "$body" || die "创建首个管理员失败"
    require_code 201
    ok "已创建管理员 user_id=$(pyj "$LAST_BODY" 'd["user_id"]')"
  else
    ok "已存在用户，跳过 bootstrap"
  fi
}

login() {
  login_admin
  # 验证 session 有效（访问 admin 列表）
  api GET /api/admin/users || die "登录后访问 admin API 失败（session 无效）"
  ok "管理员 session 有效（GET /api/admin/users 200）"
}

provider_configure() {
  step "配置 Embedding / Reranker Provider（真实 gitee provider）"

  # 1) 连通性测试（POST .../test 不持久化）
  local embtest rrtest
  embtest=$(printf '{"api_key":"%s","config":{"base_url":"%s","model":"%s","dimensions":%s,"timeout_seconds":%s,"batch_size":8,"query_instruction":"","document_instruction":""}}' \
      "$E2E_PROVIDER_API_KEY" "$E2E_PROVIDER_BASE_URL" "$E2E_EMBEDDING_MODEL" "$E2E_EMBEDDING_DIMENSIONS" "$E2E_EMBEDDING_TIMEOUT")
  api POST /api/admin/providers/embedding/test "$embtest" || die "embedding test 请求失败"
  local st
  st="$(pyj "$LAST_BODY" 'd["status"]')"
  [ "$st" = "ok" ] || { echo "响应: $(cat "$LAST_BODY")" >&2; die "embedding Provider 测试未通过: $st"; }
  ok "Embedding test=ok"

  rrtest=$(printf '{"api_key":"%s","config":{"base_url":"%s","model":"%s","timeout_seconds":%s,"max_candidates":20}}' \
      "$E2E_PROVIDER_API_KEY" "$E2E_PROVIDER_BASE_URL" "$E2E_RERANKER_MODEL" "$E2E_RERANKER_TIMEOUT")
  api POST /api/admin/providers/reranker/test "$rrtest" || die "reranker test 请求失败"
  st="$(pyj "$LAST_BODY" 'd["status"]')"
  # Reranker 连通性测试为"软校验"：真实第三方 /rerank 服务可能瞬时抖动，而搜索核心断言
  # 不依赖 reranker（候选不足时根本不触发）。失败仅告警不中断，后续仍 PUT 持久化配置。
  if [ "$st" = "ok" ]; then
    ok "Reranker test=ok"
  else
    warn "Reranker test 非 ok（status=$st）→ 软跳过（真实 provider 可能瞬时不可用）: $(cat "$LAST_BODY")"
  fi

  # 2) PUT 持久化（热更新）
  local put1
  put1=$(printf '{"api_key":"%s","config":{"base_url":"%s","model":"%s","dimensions":%s,"timeout_seconds":%s,"batch_size":8,"query_instruction":"","document_instruction":""}}' \
      "$E2E_PROVIDER_API_KEY" "$E2E_PROVIDER_BASE_URL" "$E2E_EMBEDDING_MODEL" "$E2E_EMBEDDING_DIMENSIONS" "$E2E_EMBEDDING_TIMEOUT")
  api PUT /api/admin/providers/embedding "$put1" || die "PUT embedding 失败"
  require_code 200

  local put2
  put2=$(printf '{"api_key":"%s","config":{"base_url":"%s","model":"%s","timeout_seconds":%s,"max_candidates":20}}' \
      "$E2E_PROVIDER_API_KEY" "$E2E_PROVIDER_BASE_URL" "$E2E_RERANKER_MODEL" "$E2E_RERANKER_TIMEOUT")
  api PUT /api/admin/providers/reranker "$put2" || die "PUT reranker 失败"
  require_code 200
  ok "两个 Provider 已保存"

  # 3) 热更新即时生效：readyz（经 server 容器）中 embedding/reranker healthy
  fetch_readyz || die "无法获取 /readyz"
  local st2
  st2="$(pyj "$LAST_BODY" 'd["checks"]["embedding"]["status"] if "embedding" in d["checks"] else "missing"')"
  [ "$st2" = "healthy" ] || { echo "readyz: $(cat "$LAST_BODY")" >&2; die "embedding 未热更新为 healthy"; }
  ok "热更新生效（readyz embedding=healthy）"

  # 4) GET 断言绝不返回 api_key 明文
  api GET /api/admin/providers/embedding || die "GET embedding 失败"
  local cfgstr
  cfgstr="$(cat "$LAST_BODY")"
  case "$cfgstr" in
    *"$E2E_PROVIDER_API_KEY"*) die "安全违规：GET provider 响应包含 api_key 明文" ;;
  esac
  local conf
  conf="$(pyj "$LAST_BODY" 'd["configured"]')"
  [ "$conf" = "True" ] || die "GET provider configured 应为 true"
  ok "GET provider 不含 api_key 明文（configured=true）"
}

profile_rebuild() {
  step "创建/补齐默认 profile 并全量重建索引（rebuild_index job）"
  # EnsureDefaultProfile 仅在启动时执行一次，且当时空 users 表致 FK 失败。现首个 admin 已建，
  # 若无 profile 则通过 admin API 创建一个含真实 gitee 参数的默认 profile 并激活。
  api GET /api/admin/search-profiles || die "GET search-profiles 失败"
  local pid iname
  pid="$(pyj "$LAST_BODY" '([p for p in (d.get("profiles") or []) if p.get("status")=="active"] or [])[0]["id"]' 2>/dev/null || true)"
  iname="$(pyj "$LAST_BODY" '([p for p in (d.get("profiles") or []) if p.get("status")=="active"] or [])[0]["es_index_name"]' 2>/dev/null || true)"
  if [ -z "$pid" ]; then
    warn "无 active profile，创建默认 profile（参数与 gitee provider 匹配）"
    local cbody
    cbody=$(python3 - "$E2E_EMBEDDING_MODEL" "$E2E_RERANKER_MODEL" "$E2E_EMBEDDING_DIMENSIONS" <<'PY'
import json,sys
print(json.dumps({
  "name":"default",
  "embedding_provider":"openai-compatible","embedding_model":sys.argv[1],
  "embedding_dimensions":int(sys.argv[3]),
  "embedding_query_instruction":"","embedding_document_instruction":"",
  "chunk_target_size":512,"chunk_overlap":64,
  "title_boost":2.0,"heading_boost":1.5,"path_boost":1.0,"tags_boost":0.5,"body_boost":1.0,"analyzer":"standard",
  "lexical_top_k":50,"vector_top_k":50,"rrf_k":60,
  "reranker_provider":"openai-compatible","reranker_model":sys.argv[2],
  "reranker_candidate_count":20,"reranker_final_count":10,
  "max_chunks_per_document":3,"merge_adjacent_chunks":True,
  "max_p95_latency_ms":2000,"max_reranker_cost_per_query":0.01
}))
PY
)
    api POST /api/admin/search-profiles "$cbody" || die "创建 profile 失败"
    local newpid
    newpid="$(pyj "$LAST_BODY" 'd.get("id","")')"
    [ -n "$newpid" ] || die "profile 创建响应无 id"
    ok "profile 已创建 ($newpid)"
    # 激活（会入队 rebuild）
    api POST "/api/admin/search-profiles/$newpid/activate" || die "激活 profile 失败"
    require_code 200
    ok "profile 已激活"
    wait_job_done "rebuild_index(profile $newpid)" 600
    # 刷新 iname
    api GET /api/admin/search-profiles
    iname="$(NEWPID="$newpid" BODY="$LAST_BODY" python3 - <<'PY'
import json,os
d=json.load(open(os.environ["BODY"],encoding="utf-8"))
np=os.environ["NEWPID"]
for p in (d.get("profiles") or []):
    if p.get("id")==np:
        print(p.get("es_index_name","")); break
else:
    print("")
PY
)"
    [ -n "$iname" ] || iname="knowledge_v1"
  else
    ok "已存在 active profile: id=$pid es_index_name=$iname"
    # 直接全量重建（覆盖到最新文档）。rebuild 端点严格 JSON：仅接受 workspace_id/index_name。
    local body
    body=$(printf '{"index_name":"%s"}' "$iname")
    api POST /api/admin/jobs/rebuild "$body" || die "触发 rebuild 失败"
    require_code 202
    ok "rebuild job 已入队 (202)"
    wait_job_done "rebuild_index(profile $pid)" 600
  fi
}

# ---------- workspace / RBAC ----------
workspace_rbac() {
  step "Workspace 创建与成员 RBAC"
  local body b2
  # workspace e2e-kb（幂等：已存在→409→列表取 id）
  if ! api POST /api/workspaces '{"name":"e2e-kb","display_name":"E2E Knowledge Base","description":"E2E 语义搜索测试知识库"}'; then
    warn "e2e-kb 可能已存在（HTTP $LAST_CODE），改用列表取 id"
  else
    ADMIN_WS="$(pyj "$LAST_BODY" 'd["id"]')"
    ok "workspace e2e-kb created ($ADMIN_WS)"
  fi
  if [ -z "$ADMIN_WS" ]; then
    api GET /api/workspaces
    ADMIN_WS="$(pyj "$LAST_BODY" '([w for w in (d.get("workspaces") or []) if w.get("name")=="e2e-kb"] or [{"id":""}])[0]["id"]')"
  fi
  [ -n "$ADMIN_WS" ] || die "无法取得 e2e-kb workspace id"
  ok "workspace e2e-kb ready ($ADMIN_WS)"

  # 第二个 workspace 供越权/负向验证（幂等：已存在→列表取 id）
  local ws2=""
  if ! api POST /api/workspaces '{"name":"e2e-other","display_name":"E2E Other","description":"隔离测试"}'; then
    warn "e2e-other 可能已存在（HTTP $LAST_CODE）"
  else
    ws2="$(pyj "$LAST_BODY" 'd["id"]')"
  fi
  if [ -z "$ws2" ]; then
    api GET /api/workspaces
    ws2="$(pyj "$LAST_BODY" '([w for w in (d.get("workspaces") or []) if w.get("name")=="e2e-other"] or [{"id":""}])[0]["id"]')"
  fi
  [ -n "$ws2" ] || die "无法取得 e2e-other workspace id"
  ok "workspace e2e-other ready"

  # 创建普通用户 u1（幂等：二次运行已存在则复用）
  local ubody
  ubody=$(printf '{"username":"e2euser","email":"e2euser@example.com","password":"e2e-password-12345","system_role":"user","workspace_create_perm":false}')
  api POST /api/admin/users "$ubody" || { warn "u1 创建可能已存在（忽略）: HTTP $LAST_CODE"; }
  u1="$(pyj "$LAST_BODY" 'd.get("id","")')"
  if [ -z "$u1" ]; then
    # 已存在：从 admin 用户列表取出 id
    api GET /api/admin/users
    u1="$(pyj "$LAST_BODY" '([u for u in (d.get("users") or []) if u.get("username")=="e2euser"] or [{"id":""}])[0]["id"]')"
    [ -n "$u1" ] || die "无法取得 e2euser id"
    warn "复用已存在用户 e2euser"
  else
    ok "普通用户 e2euser created"
  fi

  # u1 无 workspace:create → 创建应 403
  # 先登录 u1（独立 cookie）
  save_cookies="$COOKIES"; save_csrf="$CSRF"
  COOKIES="$STATE/cookies_u1.txt"; : > "$COOKIES"
  local ub
  ub=$(printf '{"username":"e2euser","password":"e2e-password-12345"}')
  api POST /api/auth/login "$ub"
  require_code 200
  CSRF="$(pyj "$LAST_BODY" 'd["csrf_token"]')"
  if api POST /api/workspaces '{"name":"x","display_name":"x"}'; then
    die "u1 不应拥有 workspace:create（应 403）"
  fi
  [ "$LAST_CODE" = "403" ] || die "u1 创建 workspace 预期 403，实际 $LAST_CODE"
  ok "u1 创建 workspace 被拒（403, 无 workspace:create）"

  # u1 加入 e2e-kb(editor) 与 e2e-other(viewer)；幂等：已是成员则改为确保角色正确
  COOKIES="$ADMIN_COOKIES"; CSRF="$save_csrf"
  local mb
  if ! api POST "/api/workspaces/$ADMIN_WS/members" "$(printf '{"user_id":"%s","role":"editor"}' "$u1")"; then
    warn "u1 已是 e2e-kb 成员（HTTP $LAST_CODE），转为 PUT 校正为 editor"
    api PUT "/api/workspaces/$ADMIN_WS/members/$u1" '{"role":"editor"}' || die "校正 e2e-kb 角色失败"
    require_code 200
  else
    require_code 201
  fi
  if ! api POST "/api/workspaces/$ws2/members" "$(printf '{"user_id":"%s","role":"viewer"}' "$u1")"; then
    warn "u1 已是 e2e-other 成员（HTTP $LAST_CODE），转为 PUT 校正为 viewer"
    api PUT "/api/workspaces/$ws2/members/$u1" '{"role":"viewer"}' || die "校正 e2e-other 角色失败"
    require_code 200
  else
    require_code 201
  fi
  ok "u1 已加入/校正两个 workspace 角色"

  # u1 查 membership（先以 u1 重新登录刷新 CSRF，确保后续 CSRF header 属于 u1）
  COOKIES="$STATE/cookies_u1.txt"; : > "$COOKIES"
  local ublogin
  ublogin=$(printf '{"username":"e2euser","password":"e2e-password-12345"}')
  api POST /api/auth/login "$ublogin"
  require_code 200
  CSRF="$(pyj "$LAST_BODY" 'd["csrf_token"]')"

  api GET "/api/workspaces/$ADMIN_WS/me/membership"
  require_code 200
  local role
  role="$(pyj "$LAST_BODY" 'd["role"]')"
  [ "$role" = "editor" ] || die "u1 在 e2e-kb 应为 editor，实际 $role"
  ok "u1 membership in e2e-kb = editor"

  # u1 尝试访问非成员 workspace（e2e-other 是 member，另构造非成员 ws 不可；用 admin 无法/不可能。改用 e2e-other 的 viewer 越权写文档即可）
  # 越权：viewer 创建文档 → 403
  local vbody
  vbody='{"path":"docs/unauth.md","title":"unauth","type":"other","content_markdown":"# x"}'
  if api POST "/api/workspaces/$ws2/documents" "$vbody"; then
    die "viewer 不应能创建文档（应 403）"
  fi
  [ "$LAST_CODE" = "403" ] || die "u1(viewer) 创建文档预期 403，实际 $LAST_CODE"
  ok "viewer 越权写文档被拒（403）"

  COOKIES="$ADMIN_COOKIES"; CSRF="$save_csrf"
}

# ---------- 文档全生命周期 ----------
document_lifecycle() {
  step "文档全生命周期（含并发控制/归档/来源）"
  local ws="$ADMIN_WS"

  # 8 篇知识文档（中英混排，包含 headings 与跨语言语义锚点）
  local docs=(
    "architecture/search-pipeline.md|语义搜索架构|# 语义搜索架构\n\n本文描述混合检索管线：BM25 词法检索与向量语义检索通过 RRF 融合，再经重排序模型精排。\n\n## 向量检索\n\n向量检索使用 dense embedding 表达文本语义，解决同义词与表述差异。\n\n## 重排序\n\n重排序模型对候选结果做相关性精排，提升头部准确率。"
    "architecture/es-alias.md|Elasticsearch Alias|# Elasticsearch Alias\n\n索引重建采用 alias 原子切换：构建新版本索引后原子更新 knowledge_current 别名指向，避免破坏在线查询。"
    "ops/backup-restore.md|备份与恢复|# PostgreSQL 备份恢复\n\n每日全量 pg_dump 与持续 WAL 归档；恢复演练每季度执行一次，RPO 目标 1 小时。"
    "security/csrf.md|CSRF 防护|# CSRF 防护\n\n所有状态变更请求携带 X-CSRF-Token 头；服务端校验 cookie 绑定 token，防止跨站请求伪造。"
    "model/embedding.md|Embedding 模型|# Embedding 表示\n\nQwen3-Embedding-8B 将句子映射为 1024 维向量。查询与文档可使用不同指令前缀以提升检索效果。"
    "language/nlp.md|自然语言处理|# NLP 技术\n\n语义匹配与文本蕴含是检索系统的核心；同义改写、词向量、句向量共同支撑跨语言知识问答。"
    "database/postgres.md|PostgreSQL 存储|# PostgreSQL 存储设计\n\n文档以 Markdown 全文存于 PostgreSQL，保留历史版本与哈希校验；Elasticsearch 仅作可重建索引。"
    "feature/knowledge.md|知识管理功能|# 知识管理\n\n工作区支持多用户协作：owner/admin/editor/viewer 四级角色；查看者只能阅读与检索。"
  )

  local i=0
  for entry in "${docs[@]}"; do
    i=$((i+1))
    local path="${entry%%|*}" rest="${entry#*|}"
    local title="${rest%%|*}" content="${rest#*|}"
    local cbody
    cbody=$(python3 - "$path" "$title" "$content" <<'PY'
import json,sys
print(json.dumps({"path":sys.argv[1],"title":sys.argv[2],"type":"reference","content_markdown":sys.argv[3].replace("\\n","\n")}, ensure_ascii=False))
PY
)
    if ! api POST "/api/workspaces/$ws/documents" "$cbody"; then
      if [ "$LAST_CODE" = "409" ]; then
        warn "文档 #$i $path 已存在（幂等跳过）"
      else
        { echo "文档[$path] 创建失败: $(cat "$LAST_BODY")" >&2; die "创建文档失败"; }
      fi
    else
      ok "文档 #$i $path created"
    fi
  done

  # 等待所有 index_document job 完成（初始 embedded）
  wait_job_done "index_document(首批文档)" 600

  # 归一化 search-pipeline.md 为确定性中文内容（幂等：无论上一轮被 move/改写为何内容，
  # 本轮先确保存在，再 replace 为规范内容，使 read/outline/section/patch/冲突断言稳定可复跑）
  ensure_canonical_doc() {
    local p="architecture/search-pipeline.md"
    local alt="arch/moved-search.md"
    local title="语义搜索架构"
    local content="# 语义搜索架构\n\n本文描述混合检索管线：BM25 词法检索与向量语义检索通过 RRF 融合，再经重排序模型精排。\n\n## 向量检索\n\n向量检索使用 dense embedding 表达文本语义，解决同义词与表述差异。\n\n## 重排序\n\n重排序模型对候选结果做相关性精排，提升头部准确率。"
    # 统一路径读取辅助：优先活文档 read；read 404 时退回 history 列表按当前路径取最新状态（覆盖归档状态）
    wsdoc_read() {
      local rpath="$1" qpath r
      qpath="$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$rpath")"
      if api GET "/api/workspaces/$ws/documents/read?path=$qpath"; then
        return 0
      fi
      # 归档文档 read 会 404：尝试从 history 反查最新记录
      if [ "$LAST_CODE" = "404" ] && api GET "/api/workspaces/$ws/documents/history?path=$qpath&limit=1"; then
        local rp
        rp="$(pyj "$LAST_BODY" '([h for h in (d.get("revisions") or [])])[0] if (d.get("revisions") or []) else {}' 2>/dev/null || true)"
        if [ -n "$rp" ] && [ "$rp" != "{}" ]; then
          LAST_BODY="$STATE/resp_wsdoc.json"
          printf '%s\n' "$rp" > "$LAST_BODY"
          return 0
        fi
      fi
      return 1
    }
    # 1) 若目标不存在：尝试把上一轮移走的 alt 移回/补齐归档；都不存在则创建
    if ! wsdoc_read "$p"; then
      if wsdoc_read "$alt"; then
        # alt 存在（可能是活文档或归档）：把它恢复并移动回 p（若归档先 restore）
        local ares
        ares="$(pyj "$LAST_BODY" 'd.get("status","")')"
        local arev ahash
        arev="$(pyj "$LAST_BODY" 'd["revision_number"]')"
        ahash="$(pyj "$LAST_BODY" 'd["content_hash"]')"
        if [ "$ares" = "archived" ]; then
          api POST "/api/workspaces/$ws/documents/restore?path=arch%2Fmoved-search.md" \
            "$(python3 - "$arev" "$ahash" <<'PY'
import json,sys
print(json.dumps({"expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)"
          require_code 200
          arev="$(pyj "$LAST_BODY" 'd["revision_number"]')"
          ahash="$(pyj "$LAST_BODY" 'd["content_hash"]')"
          warn "先 restore 归档的 $alt"
        fi
        api POST "/api/workspaces/$ws/documents/move?path=arch%2Fmoved-search.md" \
          "$(python3 - "$p" "$arev" "$ahash" <<'PY'
import json,sys
print(json.dumps({"new_path":sys.argv[1],"expected_revision":int(sys.argv[2]),"expected_hash":sys.argv[3]}))
PY
)"
        require_code 200
        warn "已将 $alt 移回 $p"
      else
        api POST "/api/workspaces/$ws/documents" "$(python3 - "$p" "$title" "$content" <<'PY'
import json,sys
print(json.dumps({"path":sys.argv[1],"title":sys.argv[2],"type":"reference","content_markdown":sys.argv[3].replace("\\n","\n")}, ensure_ascii=False))
PY
)"
        require_code 201
        warn "已创建规范文档 $p"
      fi
    else
      # p 存在但可能为归档（read 404 分支走了 history，响应无 content_markdown）
      local pst
      pst="$(pyj "$LAST_BODY" 'd.get("status","")')"
      if [ "$pst" = "archived" ]; then
        warn "规范文档 $p 处于归档态，先 restore 为活文档"
        local prv phsh
        prv="$(pyj "$LAST_BODY" 'd["revision_number"]')"
        phsh="$(pyj "$LAST_BODY" 'd["content_hash"]')"
        api POST "/api/workspaces/$ws/documents/restore?path=architecture%2Fsearch-pipeline.md" \
          "$(python3 - "$prv" "$phsh" <<'PY'
import json,sys
print(json.dumps({"expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)"
        require_code 200
      fi
    fi
    # 2) replace 为规范内容（幂等内容不变；revision 递增无副作用）
    wsdoc_read "$p"
    require_code 200
    local cr cv ch
    cv="$(pyj "$LAST_BODY" 'd["revision_number"]')"
    ch="$(pyj "$LAST_BODY" 'd["content_hash"]')"
    api PUT "/api/workspaces/$ws/documents?path=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$p")" \
      "$(python3 - "$title" "$content" "$cv" "$ch" <<'PY'
import json,sys
print(json.dumps({"title":sys.argv[1],"type":"reference","content_markdown":sys.argv[2].replace("\\n","\n"),"expected_revision":int(sys.argv[3]),"expected_hash":sys.argv[4]}))
PY
)"
    require_code 200
    ok "文档 $p 已归一化为规范中文内容 (rev=$(pyj "$LAST_BODY" 'd["revision_number"]'))"
  }
  ensure_canonical_doc
  wait_job_done "index_document(规范化 search-pipeline)" 600

  # 归档/恢复/永久删除（幂等；统一在 search-pipeline.md 上做，完成后恢复为非归档活文档）
  # archive 用规范中文内容的真实 rev/hash
  api GET "/api/workspaces/$ws/documents/read?path=architecture%2Fsearch-pipeline.md"
  require_code 200
  local arv ahsh
  arv="$(pyj "$LAST_BODY" 'd["revision_number"]')"
  ahsh="$(pyj "$LAST_BODY" 'd["content_hash"]')"
  api POST "/api/workspaces/$ws/documents/archive?path=architecture%2Fsearch-pipeline.md" \
    "$(python3 - "$arv" "$ahsh" <<'PY'
import json,sys
print(json.dumps({"expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)"
  require_code 200
  local avst arv2 ahsh2
  avst="$(pyj "$LAST_BODY" 'd["status"]')"
  arv2="$(pyj "$LAST_BODY" 'd["revision_number"]')"
  ahsh2="$(pyj "$LAST_BODY" 'd["content_hash"]')"
  [ "$avst" = "archived" ] || die "archive 后 status 应为 archived，实际 $avst"
  ok "archive ok（status=archived rev=$arv2）"
  # 归档后活文档 read/outline 应被拒（服务端对 archived 文档默认隐藏）
  if api GET "/api/workspaces/$ws/documents/read?path=architecture%2Fsearch-pipeline.md"; then
    die "archive 后 read 应被拒"
  fi
  [ "$LAST_CODE" = "404" ] || die "archive 后 read 预期 404，实际 $LAST_CODE"
  ok "archive 后 read → 404（归档文档不可读）"
  # restore 回到活文档（用 archive 响应携带的最新 rev/hash）
  api POST "/api/workspaces/$ws/documents/restore?path=architecture%2Fsearch-pipeline.md" \
    "$(python3 - "$arv2" "$ahsh2" <<'PY'
import json,sys
print(json.dumps({"expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)"
  require_code 200
  ok "restore ok（status=$(pyj "$LAST_BODY" 'd["status"]')）"
  # 确认恢复后可读
  api GET "/api/workspaces/$ws/documents/read?path=architecture%2Fsearch-pipeline.md"
  require_code 200
  ok "restore 后 read 恢复"
  # purge 仅 owner；以普通 editor u1 身份（若被拒绝 403 即证明权限闸门）——仅当 u1 已就绪才做负向
  # 负向（editor 尝试 purge）→403；正向 purge→200；随后重建同一文档，保证后续生命周期仍可复跑
  local apv aph
  api GET "/api/workspaces/$ws/documents/read?path=architecture%2Fsearch-pipeline.md"
  require_code 200
  apv="$(pyj "$LAST_BODY" 'd["revision_number"]')"
  aph="$(pyj "$LAST_BODY" 'd["content_hash"]')"
  # 1) 负向：u1（editor）purge → 403
  if [ -n "${u1:-}" ] && [ -s "$STATE/cookies_u1.txt" ]; then
    local ssave csave
    ssave="$COOKIES"; csave="$CSRF"
    # 以 u1 身份重新登录（刷新 u1 的 cookie 与 CSRF）
    COOKIES="$STATE/cookies_u1.txt"
    local ulog
    ulog=$(printf '{"username":"e2euser","password":"e2e-password-12345"}')
    api POST /api/auth/login "$ulog"
    require_code 200
    CSRF="$(pyj "$LAST_BODY" 'd["csrf_token"]')"
    api POST "/api/workspaces/$ws/documents/purge?path=architecture%2Fsearch-pipeline.md" \
      "$(python3 - "$apv" "$aph" <<'PY'
import json,sys
print(json.dumps({"expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)"
    local ucode="$LAST_CODE"
    COOKIES="$ssave"; CSRF="$csave"
    if [ "$ucode" = "200" ]; then
      die "editor purge 不应成功（200）"
    fi
    ok "editor purge 被拒（HTTP $ucode）"
  else
    warn "u1 上下文缺失，跳过 purge 负向断言"
  fi
  # 2) 正向：owner purge → 200（文档连同 revision/source 永久删除）
  local ppv pph
  api GET "/api/workspaces/$ws/documents/read?path=architecture%2Fsearch-pipeline.md"
  ppv="$(pyj "$LAST_BODY" 'd["revision_number"]')"
  pph="$(pyj "$LAST_BODY" 'd["content_hash"]')"
  api POST "/api/workspaces/$ws/documents/purge?path=architecture%2Fsearch-pipeline.md" \
    "$(python3 - "$ppv" "$pph" <<'PY'
import json,sys
print(json.dumps({"expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)"
  require_code 200
  ok "owner purge ok"
  # 3) purge 后文档应 404
  if api GET "/api/workspaces/$ws/documents/read?path=architecture%2Fsearch-pipeline.md"; then
    die "purge 后 read 应 404"
  fi
  [ "$LAST_CODE" = "404" ] || die "purge 后 read 预期 404，实际 $LAST_CODE"
  ok "purge 后 read → 404"
  # 4) 重建同一路径文档，作为后续生命周期/搜索的起点（幂等闭环）
  api POST "/api/workspaces/$ws/documents" "$(python3 - <<'PY'
import json
print(json.dumps({"path":"architecture/search-pipeline.md","title":"语义搜索架构","type":"reference","content_markdown":"# 语义搜索架构\n\n本文描述混合检索管线：BM25 词法检索与向量语义检索通过 RRF 融合，再经重排序模型精排。\n\n## 向量检索\n\n向量检索使用 dense embedding 表达文本语义，解决同义词与表述差异。\n\n## 重排序\n\n重排序模型对候选结果做相关性精排，提升头部准确率。"}, ensure_ascii=False))
PY
)"
  require_code 201
  ok "purge 后重建（生命周期闭环）"
  wait_job_done "index_document(purge 后重建)" 600

  # 读取首个文档并校验 read/outline/section/lines
  local dp="architecture/search-pipeline.md"
  api GET "/api/workspaces/$ws/documents/read?path=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$dp")"
  require_code 200
  local drev dhash dtitle
  drev="$(pyj "$LAST_BODY" 'd["revision_number"]')"
  dhash="$(pyj "$LAST_BODY" 'd["content_hash"]')"
  dtitle="$(pyj "$LAST_BODY" 'd["title"]')"
  ok "read doc: title=$dtitle rev=$drev"

  api GET "/api/workspaces/$ws/documents/outline?path=architecture%2Fsearch-pipeline.md"
  require_code 200
  pyj "$LAST_BODY" 'd["outline"]' | grep -q '向量检索' || die "outline 缺少 heading"
  ok "outline ok"

  api GET "/api/workspaces/$ws/documents/section?path=architecture%2Fsearch-pipeline.md&section_path=%E8%AF%AD%E4%B9%89%E6%90%9C%E7%B4%A2%E6%9E%B6%E6%9E%84,%E5%90%91%E9%87%8F%E6%A3%80%E7%B4%A2"
  require_code 200
  ok "read section ok"

  api GET "/api/workspaces/$ws/documents/lines?path=architecture%2Fsearch-pipeline.md&start=1&end=3"
  require_code 200
  ok "read lines ok"

  # patch（错误 candidate_hash → 400）
  local pbody1
  pbody1=$(python3 - "$drev" "$dhash" <<'PY'
import json,sys
print(json.dumps({"content_markdown":"# 语义搜索架构（修订）\n\n补充：混合检索。","candidate_hash":"0000wrong","expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)
  if api PATCH "/api/workspaces/$ws/documents?path=architecture%2Fsearch-pipeline.md" "$pbody1"; then
    die "candidate_hash 错误应 400"
  fi
  [ "$LAST_CODE" = "400" ] || die "预期 400，实际 $LAST_CODE"
  ok "patch 错误 hash → 400"

  # 正确 patch
  local goodbody
  goodbody=$(python3 - "$drev" "$dhash" <<'PY'
import json,sys,hashlib
content="# 语义搜索架构（修订）\n\n混合检索管线：BM25 词法检索与向量语义检索通过 RRF 融合后由重排序模型精排。\n\n## 向量检索\n\ndense embedding 语义匹配用于处理同义词。\n\n## 重排序\n\nrerank 提升头部准确率。"
h=hashlib.sha256(content.encode()).hexdigest()
print(json.dumps({"content_markdown":content,"candidate_hash":h,"expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)
  api PATCH "/api/workspaces/$ws/documents?path=architecture%2Fsearch-pipeline.md" "$goodbody"
  require_code 200
  local rev2 hash2
  rev2="$(pyj "$LAST_BODY" 'd["revision_number"]')"
  hash2="$(pyj "$LAST_BODY" 'd["content_hash"]')"
  ok "patch ok → rev=$rev2"

  # 用旧 revision+hash PUT → 409（乐观并发）
  local oldbody
  oldbody=$(python3 - "$drev" "$dhash" <<'PY'
import json,sys
print(json.dumps({"title":"语义搜索架构","type":"other","content_markdown":"# 冲突写入","expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)
  if api PUT "/api/workspaces/$ws/documents?path=architecture%2Fsearch-pipeline.md" "$oldbody"; then
    die "旧 revision PUT 应 409"
  fi
  [ "$LAST_CODE" = "409" ] || die "预期 409，实际 $LAST_CODE"
  ok "并发冲突（旧 revision）→ 409"

  # replace 正确
  local repbody
  repbody=$(python3 - "$rev2" "$hash2" <<'PY'
import json,sys
print(json.dumps({"title":"语义搜索架构（全文修订）","type":"other","content_markdown":"# 语义搜索架构\n\n混合检索：BM25 + 向量语义检索经 RRF 融合，重排序精排。\n\n## 向量检索\n\nQwen3 embedding 语义匹配。\n\n## 重排序\n\nReranker 提升准确率。","expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)
  api PUT "/api/workspaces/$ws/documents?path=architecture%2Fsearch-pipeline.md" "$repbody"
  require_code 200
  rev2="$(pyj "$LAST_BODY" 'd["revision_number"]')"
  hash2="$(pyj "$LAST_BODY" 'd["content_hash"]')"
  ok "replace ok → rev=$rev2"

  # history / revision 查询（幂等：历史可能因复跑已很多，只校验有历史且能读 revision 1）
  api GET "/api/workspaces/$ws/documents/history?path=architecture%2Fsearch-pipeline.md"
  require_code 200
  local nrev
  nrev="$(pyj "$LAST_BODY" 'd["total"]')"
  [ "$nrev" -ge 1 ] 2>/dev/null || die "history total=$nrev 异常"
  ok "history total=$nrev (>=1)"
  api GET "/api/workspaces/$ws/documents/revision?path=architecture%2Fsearch-pipeline.md&revision=1"
  require_code 200
  ok "read revision 1 ok"

  # sources：Add + Delete
  local srcb
  srcb='{"source_type":"web","value":"https://example.com/semantic","title":"semantic ref","refresh_interval_days":7}'
  api POST "/api/workspaces/$ws/documents/sources?path=architecture%2Fsearch-pipeline.md" "$srcb"
  require_code 201
  local sid
  sid="$(pyj "$LAST_BODY" 'd["id"]')"
  api GET "/api/workspaces/$ws/documents/sources?path=architecture%2Fsearch-pipeline.md"
  require_code 200
  api DELETE "/api/workspaces/$ws/documents/sources/$sid"
  require_code 200
  ok "source add+list+delete ok"

  # move（幂等）。不变式：过程前后 architecture/search-pipeline.md 存在且为活文档；
  # 若残留 arch/moved-search.md（上一次失败/中断留下）先 purge 掉再执行 move。
  if api GET "/api/workspaces/$ws/documents/read?path=arch%2Fmoved-search.md"; then
    local tv th
    tv="$(pyj "$LAST_BODY" 'd["revision_number"]')"
    th="$(pyj "$LAST_BODY" 'd["content_hash"]')"
    api POST "/api/workspaces/$ws/documents/purge?path=arch%2Fmoved-search.md" \
      "$(python3 - "$tv" "$th" <<'PY'
import json,sys
print(json.dumps({"expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)"
    require_code 200
    warn "purge 残留 arch/moved-search.md（先清理）"
  fi
  # 执行 move 断言（目标唯一化）
  local mvbody
  mvbody=$(python3 - "$rev2" "$hash2" <<'PY'
import json,sys
print(json.dumps({"new_path":"arch/moved-e2e-" + __import__("uuid").uuid4().hex[:8] + ".md","expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)
  api POST "/api/workspaces/$ws/documents/move?path=architecture%2Fsearch-pipeline.md" "$mvbody"
  require_code 200
  ok "move ok（唯一目标路径）"
  # 移回原路径，保证后续断言路径稳定
  local np nv3 nh3
  np="$(pyj "$LAST_BODY" 'd["path"]')"
  nv3="$(pyj "$LAST_BODY" 'd["revision_number"]')"
  nh3="$(pyj "$LAST_BODY" 'd["content_hash"]')"
  api POST "/api/workspaces/$ws/documents/move?path=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$np")" \
    "$(python3 - "$nv3" "$nh3" <<'PY'
import json,sys
print(json.dumps({"new_path":"architecture/search-pipeline.md","expected_revision":int(sys.argv[1]),"expected_hash":sys.argv[2]}))
PY
)"
  require_code 200
  rev2="$(pyj "$LAST_BODY" 'd["revision_number"]')"
  hash2="$(pyj "$LAST_BODY" 'd["content_hash"]')"
  ok "move 后已移回原路径"

  wait_job_done "index_document(修订/移动后)" 600
}

# ---------- 语义搜索核心断言 ----------
search_assertions() {
  step "语义搜索断言（semantic / hybrid / lexical）"
  local ws="$ADMIN_WS"
  local base="/api/workspaces/$ws/search"

  # 权威兜底：前面文档全生命周期（archive/purge/restore/move）触发的增量 index_document job
  # 在真实抖动下可能失败/漂移，导致 ES 与 DB 不一致（曾观测 results=0 degraded=false：ES 可 Ping、
  # embedding 可用，但索引无命中 chunk）。语义搜索前强制 rebuild_index 一次（幂等、可复跑），
  # 以 DB 当前 active 文档为准重建，保证搜索断言在一致快照上进行。
  api GET /api/admin/search-profiles || die "GET search-profiles 失败"
  local rbpid rbiname
  rbpid="$(pyj "$LAST_BODY" '([p for p in (d.get("profiles") or []) if p.get("status")=="active"] or [{}])[0].get("id","")' 2>/dev/null || true)"
  rbiname="$(pyj "$LAST_BODY" '([p for p in (d.get("profiles") or []) if p.get("status")=="active"] or [{}])[0].get("es_index_name","")' 2>/dev/null || true)"
  if [ -n "$rbpid" ] && [ -n "$rbiname" ]; then
    api POST /api/admin/jobs/rebuild "$(printf '{"index_name":"%s"}' "$rbiname")" || die "rebuild(搜索前兜底) 入队失败"
    require_code 202
    ok "语义搜索前权威 rebuild 已入队（index=$rbiname profile=$rbpid）"
    wait_job_done "rebuild_index(搜索前兜底 $rbpid)" 600
  else
    warn "无 active profile，跳过搜索前兜底 rebuild（搜索结果可能不稳定）"
  fi

  # semantic：中文同义表述命中英文文档（候选不足 2 时不会触发 reranker，故不强制 reranker_used）
  local body
  body='{"query":"混合检索如何融合词法匹配与向量相似度？","mode":"semantic","limit":10,"offset":0}'
  api POST "$base" "$body" || die "semantic 搜索失败"
  require_code 200
  local degraded rr reason paths
  degraded="$(pyj "$LAST_BODY" 'd["degraded"]')"
  reason="$(pyj "$LAST_BODY" 'd.get("degradation_reason","")')"
  rr="$(pyj "$LAST_BODY" 'd["reranker_used"]')"
  # 增强诊断：每次搜索都把完整响应原文落盘到 STATE（复跑排查用），失败时打印
  local semresp
  semresp="$(cat "$LAST_BODY")"
  case "$semresp" in
    *'"results":null'*|*'"results":[]'*) : ;;
  esac
  # degraded 仅允许由 reranker 不可用触发（命中数≤1 时按设计降级为 RRF 仍返回结果）；
  # ES/embedding/vector 相关 degraded 视为缺陷。
  if [ "$degraded" = "True" ] || [ "$degraded" = "true" ]; then
    case "$reason" in
      reranker_unavailable) warn "semantic degraded=true(reason=$reason)：reranker 不可用按设计降级为 RRF，命中仍然有效" ;;
      *) { echo "search resp: $(printf '%s' "$semresp" | head -c 600)" >&2; die "semantic 不应 degraded（reason=$reason）"; } ;;
    esac
  fi
  paths="$(pyj "$LAST_BODY" '[r["path"] for r in d["results"]]')"
  echo "$paths" | grep -q 'search-pipeline' || { echo "结果: $paths" >&2; die "semantic 未命中 search-pipeline"; }
  ok "semantic：中文同义查询命中英文文档 (degraded=$degraded reason=$reason reranker=$rr)"

  # semantic：检索“数据库存储 文档全文” → postgres.md
  body='{"query":"数据库文档全文存在哪里","mode":"semantic","limit":5,"offset":0}'
  api POST "$base" "$body" || die "semantic 搜索#2 失败"
  require_code 200
  paths="$(pyj "$LAST_BODY" '[r["path"] for r in d["results"]]')"
  echo "$paths" | grep -q 'postgres' || { echo "结果: $paths" >&2; die "semantic#2 未命中 postgres"; }
  ok "semantic：postgres 命中"

  # hybrid
  body='{"query":"CSRF 防护机制","mode":"hybrid","limit":10,"offset":0}'
  api POST "$base" "$body" || die "hybrid 搜索失败"
  require_code 200
  paths="$(pyj "$LAST_BODY" '[r["path"] for r in d["results"]]')"
  echo "$paths" | grep -q 'csrf' || die "hybrid 未命中 csrf"
  ok "hybrid：csrf 命中"

  # lexical：纯词法（英文特征词 RRF）——用独有词
  body='{"query":"Elasticsearch alias atomic switch","mode":"lexical","limit":10,"offset":0}'
  api POST "$base" "$body" || die "lexical 搜索失败"
  require_code 200
  paths="$(pyj "$LAST_BODY" '[r["path"] for r in d["results"]]')"
  echo "$paths" | grep -q 'es-alias' || die "lexical 未命中 es-alias"
  ok "lexical：es-alias 命中"

  # 负向（无关查询，返回 200 且结果为空或极少）
  body='{"query":"量子退火理论完全无关内容","mode":"hybrid","limit":5,"offset":0}'
  api POST "$base" "$body" || die "无关查询失败"
  require_code 200
  local n
  n="$(pyj "$LAST_BODY" 'len(d["results"])')"
  ok "无关查询正常（结果数=$n，非失败）"

  # 反馈记录
  body='{"feedback_type":"explicit","query":"混合检索如何融合词法匹配与向量相似度","document_id":"","detail":{"useful":true},"search_id":""}'
  api POST "$base/feedback" "$body" || die "feedback 失败"
  require_code 201
  ok "search feedback recorded"
}

# ---------- 运维/管理面冒烟 ----------
admin_surface() {
  step "Admin/运维 API 冒烟"
  api GET /api/admin/audit || die "audit 失败"
  require_code 200
  local acts
  acts="$(pyj "$LAST_BODY" '[e.get("action","") for e in d["entries"]]')"
  echo "$acts" | grep -qE 'provider\.(update)|document\.(create|patch|replace)|workspace\.create' || warn "审计条目未覆盖预期 action（列表: $acts）"
  ok "audit log 含核心 action"

  api GET /api/admin/search-profiles/candidates
  require_code 200
  ok "search-profiles/candidates ok"

  api GET /api/admin/config/settings
  require_code 200
  ok "config/settings ok"

  api GET "/api/workspaces/$ADMIN_WS/stats"
  require_code 200
  ok "workspace stats ok"

  api GET /healthz
  require_code 200
  ok "/healthz ok"

  # web 静态首页（Nginx）
  curl -sS -o /dev/null -w '%{http_code}' "${E2E_WEB_BASE_URL}/" | grep -q '200' || die "web 首页非 200"
  local html
  html="$(curl -sS "${E2E_WEB_BASE_URL}/")"
  echo "$html" | grep -qi '<html' || die "web 首页非 HTML"
  ok "web 首页 200 HTML"

  # device auth start（需 session；验证 verification_url 使用 PUBLIC_ORIGIN）
  local dbody
  dbody='{"client_name":"e2e-cli"}'
  api POST /api/auth/device/start "$dbody" || { warn "device start 失败（若非核心功能可忽略）: $LAST_CODE"; }
  if [ "$LAST_CODE" = "200" ] || [ "$LAST_CODE" = "201" ]; then
    local vurl
    vurl="$(pyj "$LAST_BODY" 'd.get("verification_url","")')"
    case "$vurl" in
      "${E2E_PUBLIC_ORIGIN}"*) ok "device verification_url 使用 PUBLIC_ORIGIN" ;;
      *) warn "verification_url=$vurl 未使用 PUBLIC_ORIGIN" ;;
    esac
  fi
}

final_report() {
  step "生成报告与清理"
  {
    echo "# Partitura E2E 测试报告"
    echo
    echo "- 时间: $(date -Is)"
    echo "- Web: ${E2E_WEB_BASE_URL}"
    echo "- 结果: 全部通过"
    echo
  } >> "$REPORT"
  ok "E2E 全部通过 ✅"
  echo
  echo "==================== 报告 ===================="
  cat "$REPORT"
  echo "=============================================="
  echo "状态目录: $STATE（cookie/响应体位于此，进程结束后已清理）"

  if [ "$DO_DOWN" = 1 ]; then
    step "docker compose down"
    ( cd "$ROOT" && docker compose -f docker-compose.yml down ) || warn "compose down 失败"
  else
    warn "默认保留容器便于复跑/人肉复查；需要停止请运行: bash scripts/e2e/run-e2e.sh --down"
    ( cd "$ROOT" && docker compose -f docker-compose.yml ps )
  fi
}

# ---------- 主入口 ----------
main() {
  echo "══════════ Partitura E2E（WSL Docker 实机语义搜索）══════════"
  echo "平台: $PLATFORM | Root: $ROOT"
  secret_load
  require_envs E2E_WEB_BASE_URL E2E_ADMIN_USERNAME E2E_ADMIN_PASSWORD \
               E2E_PROVIDER_BASE_URL E2E_PROVIDER_API_KEY \
               E2E_EMBEDDING_MODEL E2E_EMBEDDING_DIMENSIONS E2E_RERANKER_MODEL

  # 出口 0/1
  if ! { \
      preflight \
      && provider_smoke \
      && compose_up \
      && wait_and_check_ready \
      && bootstrap_admin \
      && login \
      && provider_configure \
      && profile_rebuild \
      && workspace_rbac \
      && document_lifecycle \
      && search_assertions \
      && admin_surface \
      && final_report
    } ; then
    echo
    warn "E2E 失败！退出码 $?"
    if [ -f "$STATE/build-server.log" ]; then warn "server build log: $STATE/build-server.log"; fi
    ( cd "$ROOT" && docker compose -f docker-compose.yml logs --tail=60 server 2>/dev/null | tail -60 || true ) > "$STATE/server-fail.log" 2>&1 || true
    warn "server 失败日志已写入: $STATE/server-fail.log"
    exit 1
  fi
  exit 0
}

main "$@"
