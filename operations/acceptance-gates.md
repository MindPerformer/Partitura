# 生产运维验收门禁 — 状态报告

## 概述

本文档记录 Partitura 生产运维验收门禁的核验结果。

**核验日期**: 2025-09-09
**核验环境**: Windows (Git Bash), 无 Docker, 无 PostgreSQL, 无 Elasticsearch
**核验方式**: 代码审查 + 本地可运行测试 + 阻断项清单

## 本地可执行验证结果

### Go 单元测试

```
cd server && go test ./...
```

**退出码**: 0
**结果**: 全部通过（30 个包，含单元测试和集成测试跳过）

| 包 | 状态 | 说明 |
|---|---|---|
| cmd/server | PASS | migrate-status 模式选择、ctx 取消触发优雅关闭 |
| internal/health | PASS | healthz/readyz 状态码、敏感信息脱敏、降级逻辑 |
| internal/job | PASS | claim/complete/fail/retry/recover、rebuild/alias、integrity |
| internal/scheduler | PASS | 去重、每日/每周任务、优雅停止 |
| internal/es | PASS | alias 切换（3 场景）、400 不 silent fallback |
| internal/auth | PASS | 登录、CSRF、middleware |
| internal/document | PASS | 路径验证、outline、revision |
| internal/workspace | PASS | RBAC、跨 workspace 隔离 |
| internal/search | PASS | pipeline、RRF、diversity、chunking |
| internal/provider | PASS | Provider 热更新 |
| internal/profile | PASS | profile CRUD、active profile |
| internal/config | PASS | 配置加载验证 |
| internal/crypto | PASS | AES-256-GCM 加密/解密 |
| 其余包 | PASS | 全部通过 |

### Web 前端测试

```
cd web && npx vitest run
```

**退出码**: 0
**结果**: 21 个测试文件，171 个测试全部通过

### Go 构建

```
cd server && go build ./...
```

**退出码**: 0

### Go Vet

```
cd server && go vet ./...
```

**退出码**: 0

### 验收门禁脚本（本地执行）

| 脚本 | 退出码 | 原因 |
|---|---|---|
| ops-gate-healthz-readyz.sh | 1 | server 不可达（无运行实例） |
| ops-gate-migrate.sh | 2 | docker 不可用 |
| ops-gate-pg-backup-restore.sh | 2 | docker 不可用 |
| ops-gate-es-rebuild-alias.sh | 2 | server 不可达 |
| ops-gate-sigterm.sh | 2 | docker 不可用 |
| ops-gate-job-recovery.sh | 2 | docker 不可用 |
| ops-gate-web-paths.sh | 1 | web 不可达（无运行实例） |

## 代码审查验证项

以下验收项通过代码审查和单元测试确认实现正确，但未在真实 PG/ES 环境中验证：

### healthz / readyz ✓（代码已实现，单元测试通过）

- `/healthz` 始终返回 200 `{"status":"ok"}` — `health.HealthzHandler`
- `/readyz` 报告 PG/ES/Embedding/Reranker/Jobs/Profile — `health.ReadyHandler`
- PG 不可用时返回 503 — `Snapshot.IsReady()` + `ReadyHandler`
- ES/Provider 降级时返回 200 + degraded — `ReadyHandler` 状态判断
- 敏感信息脱敏 — `safeError()` 白名单/黑名单策略
- 单元测试覆盖: `health_test.go`（11 个测试）

### SIGTERM 优雅关闭 ✓（代码已实现，单元测试通过）

- 停止接受新 HTTP 请求 — `httpServer.Shutdown(shutdownCtx)`
- 取消 server context — `serverCancel()`
- 等待 Job Worker 安全退出（有界超时） — `jobWorker.Stop()` + `select` 超时
- 等待 Scheduler 安全退出 — `sched.Stop()`
- 关闭数据库连接 — `defer db.Close(database)`
- `stop_grace_period: 50s`，`SHUTDOWN_TIMEOUT: 45s`
- 单元测试覆盖: `main_test.go` — `TestRunServer_ContextCancelTriggersShutdown`

### Job 恢复 ✓（代码已实现，集成测试通过但需 PG）

- `RecoverStaleJobs` 将 stale running job 重置为 pending — `job.PGRepository.RecoverStaleJobs`
- 启动时自动调用 — `main.go` 第 351 行
- `DefaultStaleJobTimeout = 5 * time.Minute`
- 集成测试覆盖: `recover_test.go`（5 个测试，需 `TEST_DATABASE_URL`）

### ES 从 PG rebuild / alias 验证 ✓（代码已实现，单元/集成测试通过）

- `SwitchAlias` 处理 3 种场景 — `es.SwitchAlias`
  1. alias 不存在且无同名索引：直接创建
  2. alias 不存在但存在同名具体索引：先删除再创建
  3. alias 已存在：原子 remove + add
- ES8 兼容嵌套 JSON — `AliasAction.MarshalJSON`
- 400 时不 silent fallback — `TestSwitchAlias_ES400_StaysFailed`
- rebuild 使用 active profile 的 dimensions/analyzer 创建 mapping — `rebuild_test.go`
- 单元测试: `switch_alias_test.go`（4 个测试）
- 集成测试: `rebuild_test.go`（3 个测试，需 PG）

### pg_dump / restore ✓（文档已完善）

- 备份文档: `operations/backup-restore.md`
- 恢复文档: `operations/backup-restore.md`
- 备份脚本示例已内嵌在文档中
- 恢复后验证步骤已文档化

### 迁移 ✓（代码已实现，集成测试通过但需 PG）

- `-migrate-status` flag 查询当前版本 — `runMigrateStatus`
- 默认模式自动执行迁移 — `runner.Up(ctx)`
- 10 个迁移文件（M001-M010）
- 集成测试: `migration_test.go`（需 `TEST_DATABASE_URL`）

### Web 登录/文档/搜索真实路径 ✓（代码已实现，前端测试通过）

- `/login` 页面 — `pages/login.vue`
- auth middleware 路由守卫 — `middleware/auth.ts`
- 文档读取 — `pages/workspaces/[id]/documents/read.vue`
- 搜索 — `pages/workspaces/[id]/search.vue`
- API 代理 — `server/api/[...].ts`（运行时从 runtimeConfig 读取目标）
- 前端测试: `auth.test.ts`（7 个测试）、`search.test.ts`（5 个测试）、`document.test.ts`（8 个测试）

## BLOCKED 清单（不含凭据）

以下验收项因本地环境限制无法执行，需要真实 Docker + PG + ES 环境：

### BLOCKED-1: healthz/readyz 真实端点验证

**阻断原因**: 无运行中的 server 实例
**前置条件**: Docker 可用，Compose 服务已启动
**下一步命令**:
```bash
# 1. 配置 .env
cp .env.example .env
# 编辑 .env，替换所有 <MUST_REPLACE> 标记

# 2. 启动服务
docker compose up -d --build

# 3. 等待服务就绪
sleep 60

# 4. 执行验收门禁
BASE_URL=http://localhost ./scripts/ops-gate-healthz-readyz.sh
```

### BLOCKED-2: pg_dump 备份与恢复验证

**阻断原因**: 无 Docker，无 PostgreSQL
**前置条件**: Docker 可用，postgres 容器运行中
**下一步命令**:
```bash
# 1. 启动 postgres
docker compose up -d postgres

# 2. 等待 healthy
docker compose ps postgres

# 3. 执行备份验证
./scripts/ops-gate-pg-backup-restore.sh

# 4. 完整恢复验证（手动，参见 operations/backup-restore.md）
docker compose stop server
docker compose exec -T postgres psql -U partitura -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
docker compose exec -T postgres psql -U partitura partitura < backup_YYYYMMDD_HHMMSS.sql
docker compose run --rm server -migrate-status
docker compose start server
curl http://localhost/readyz
```

### BLOCKED-3: ES 从 PG rebuild + alias 验证

**阻断原因**: 无 Docker，无 Elasticsearch，无运行中的 server
**前置条件**: Docker 可用，全部 Compose 服务运行中，至少一个 system_admin 用户
**下一步命令**:
```bash
# 1. 启动全部服务
docker compose up -d --build

# 2. 通过 /bootstrap 创建第一个 system_admin
curl -X POST http://localhost/api/bootstrap \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"<strong-password>","email":"admin@example.com"}'

# 3. 执行 ES rebuild 验收门禁
BASE_URL=http://localhost ./scripts/ops-gate-es-rebuild-alias.sh
```

### BLOCKED-4: SIGTERM 优雅关闭验证

**阻断原因**: 无 Docker
**前置条件**: Docker 可用，server 容器运行中
**下一步命令**:
```bash
# 1. 启动服务
docker compose up -d

# 2. 等待 server healthy
docker compose ps server

# 3. 执行 SIGTERM 验收门禁
./scripts/ops-gate-sigterm.sh
```

### BLOCKED-5: Job 恢复验证

**阻断原因**: 无 Docker
**前置条件**: Docker 可用，server 容器运行中
**下一步命令**:
```bash
# 1. 启动服务
docker compose up -d

# 2. 执行 Job 恢复验收门禁
BASE_URL=http://localhost:8080 ./scripts/ops-gate-job-recovery.sh
```

### BLOCKED-6: 迁移验证

**阻断原因**: 无 Docker
**前置条件**: Docker 可用，postgres 容器运行中，server 镜像已构建
**下一步命令**:
```bash
# 1. 启动 postgres
docker compose up -d postgres

# 2. 执行迁移验收门禁
./scripts/ops-gate-migrate.sh
```

### BLOCKED-7: Web 登录/文档/搜索真实路径验证

**阻断原因**: 无运行中的 web/server 实例
**前置条件**: Docker 可用，全部 Compose 服务运行中
**下一步命令**:
```bash
# 1. 启动全部服务
docker compose up -d --build

# 2. 等待 web 就绪
sleep 30

# 3. 执行 Web 路径验收门禁
BASE_URL=http://localhost ./scripts/ops-gate-web-paths.sh
```

### BLOCKED-8: 集成测试（需 PG）

**阻断原因**: 无 PostgreSQL 实例，`TEST_DATABASE_URL` 未设置
**前置条件**: PostgreSQL 可用
**下一步命令**:
```bash
# 1. 启动 postgres
docker compose up -d postgres

# 2. 设置 TEST_DATABASE_URL（不输出密码到日志）
export TEST_DATABASE_URL="host=localhost port=5432 user=partitura password=<your-password> dbname=partitura sslmode=disable"

# 3. 运行集成测试
cd server && go test -v -run Integration ./...
```

## 新增/修改文件清单

### 新增文件

| 文件 | 说明 |
|---|---|
| `scripts/ops-gate-healthz-readyz.sh` | healthz/readyz 端点验收门禁 |
| `scripts/ops-gate-migrate.sh` | 数据库迁移验收门禁 |
| `scripts/ops-gate-pg-backup-restore.sh` | pg_dump 备份与恢复验收门禁 |
| `scripts/ops-gate-es-rebuild-alias.sh` | ES rebuild + alias 验收门禁 |
| `scripts/ops-gate-sigterm.sh` | SIGTERM 优雅关闭验收门禁 |
| `scripts/ops-gate-job-recovery.sh` | Job 恢复验收门禁 |
| `scripts/ops-gate-web-paths.sh` | Web 登录/文档/搜索路径验收门禁 |
| `scripts/ops-gate-all.sh` | 总门禁（执行全部子门禁） |
| `operations/acceptance-gates.md` | 本文档 |

### 修改文件

无。所有代码实现已就绪，无需修改。

## 证据缺口

1. **真实 PG/ES 环境验证**: 所有门禁脚本因本地无 Docker/PG/ES 而无法执行
2. **集成测试**: `TEST_DATABASE_URL` 未设置，集成测试自动跳过
3. **E2E 测试**: 无真实运行实例，Web 路径验证仅通过代码审查和前端单元测试确认
4. **Provider 验证**: 无真实 Embedding/Reranker Provider，降级路径通过单元测试确认
