# Partitura — Project Knowledge Workspace

## 概述

Partitura 是一个企业级 Project Knowledge Workspace，支持多用户、多 Workspace、Markdown 知识管理、语义搜索和版本控制。

## 架构

| 服务 | 技术栈 | 角色 |
|------|--------|------|
| server | Go | REST API、Job Worker、Scheduler |
| web | Nuxt 4 + Nuxt UI 4 | 前端 SSR |
| postgres | PostgreSQL 18 | 业务真相源 |
| elasticsearch | Elasticsearch 9.5 | 可重建搜索索引 |

**核心原则**：PostgreSQL 是业务真相源。Elasticsearch 是可重建的搜索索引，数据丢失后可从 PostgreSQL 完整重建。

## 先决条件

- Docker Engine 26+
- Docker Compose v2+
- 至少 2GB 可用内存（ES 需 512MB+）

## 快速开始

### 1. 创建环境配置

```bash
cp .env.example .env
# 编辑 .env，替换所有 <MUST_REPLACE> 标记
# 必须设置：
#   POSTGRES_PASSWORD — 强密码（至少 32 字符）
#   DB_PASSWORD — 与 POSTGRES_PASSWORD 一致
#   EMBEDDING_BASE_URL, EMBEDDING_API_KEY — Embedding Provider
#   RERANKER_BASE_URL, RERANKER_API_KEY — Reranker Provider
```

**禁止将 .env 提交到版本库。**

### 2. 启动服务

`docker-compose.yml` 默认使用 `MindPerformer` 组织发布到 GHCR 的镜像：

```bash
docker compose -f docker-compose.yml pull
docker compose -f docker-compose.yml up -d
```

默认镜像为：

- `ghcr.io/mindperformer/partitura-server:latest`
- `ghcr.io/mindperformer/partitura-web:latest`

部署指定版本时，在 `.env` 中设置 `IMAGE_TAG`，例如 `IMAGE_TAG=v1.0.0`。如需使用自定义镜像仓库，可设置 `SERVER_IMAGE` 和 `WEB_IMAGE`。

本地构建镜像后可通过覆盖变量使用：

```bash
docker build -f server/Dockerfile -t partitura-server:local .
docker build -f web/Dockerfile -t partitura-web:local .
SERVER_IMAGE=partitura-server WEB_IMAGE=partitura-web IMAGE_TAG=local docker compose -f docker-compose.yml up -d
```

GitHub Actions 在推送到 `main` 或版本标签（`v*.*.*`）时自动构建并发布两个镜像；Pull Request 只执行构建验证，不会推送镜像。GHCR 私有包需要先在部署主机执行 `docker login ghcr.io`。

### 3. 检查健康状态

```bash
# 进程存活检查（不因 ES/Provider 降级而失败）
curl http://localhost/healthz

# 就绪检查（报告 PG/ES/Embedding/Reranker/Jobs/Profile 状态）
curl http://localhost/readyz
```

`/readyz` 响应示例：
```json
{
  "status": "ready",
  "checks": {
    "postgresql": {"status": "healthy"},
    "elasticsearch": {"status": "healthy"},
    "embedding": {"status": "healthy"},
    "reranker": {"status": "healthy"},
    "pending_jobs": 0,
    "failed_jobs": 0,
    "active_profile": {"id": "...", "name": "default"}
  }
}
```

当 ES/Provider 不可用时，`status` 为 `"degraded"` 但 HTTP 仍返回 200。
当 PG 不可用时，`status` 为 `"not_ready"` 且 HTTP 返回 503。

### 4. 停止服务

```bash
docker compose down
```

Server 收到 SIGTERM 后会先停止接受新 HTTP 请求，再取消 Job Worker 和 Scheduler，等待进行中的任务安全完成或超时后退出。

## MCP 客户端（knowledge-mcp）

`knowledge-mcp` 是本地 stdio MCP 客户端二进制：Agent（Claude / Codex / Cursor 等）以子进程方式启动它，通过 stdin/stdout 走 MCP 协议；它再通过 HTTPS REST 与 Knowledge Server 通信。MCP 本身不开放网络端口。

源码位于 `mcp/`（独立 Go module），配置与加密凭据都放在**运行目录**下，多实例靠不同运行目录隔离。

### 本地构建

前置条件：Go 1.26.4（与 `mcp/go.mod` 一致）。跨平台打包另需 `tar`、`sha256sum`；打 Windows 包还需要 `zip`，或可用的 `python3` / `python`。

| 目标 | 命令 | 产物 |
|------|------|------|
| 当前平台快速构建 | `make mcp-build` | `dist/knowledge-mcp`（Windows 为 `dist/knowledge-mcp.exe`） |
| 全部 6 平台打包 | `make mcp-cross` | `dist/` 下 6 个平台包 + `checksums.txt` |
| 等价脚本调用 | `bash scripts/build-mcp.sh` | 同上，默认输出到 `dist/` |
| 指定版本与平台 | `bash scripts/build-mcp.sh v1.2.3 dist linux/amd64` | `dist/knowledge-mcp_v1.2.3_linux_amd64.tar.gz` |

平台为 windows/linux/darwin × amd64/arm64 共 6 个。`platforms` 参数接受 `os/arch` 或逗号分隔的多个 `os/arch`，不传则构建全部 6 个平台。

产物命名：

- `knowledge-mcp_<version>_<os>_<arch>.zip`（Windows，内含 `knowledge-mcp.exe`）
- `knowledge-mcp_<version>_<os>_<arch>.tar.gz`（其余平台，内含 `knowledge-mcp`）
- 每个包附带同名 `.sha256`
- `checksums.txt` 汇总本次构建的全部包，脚本生成后会自动 `sha256sum -c` 复验

其他 Makefile 目标：`make mcp-test`（`go test ./...`）、`make mcp-vet`（`go vet ./...`）。

版本号经 `-ldflags` 注入 `mcp/internal/buildinfo`；不指定时脚本用 `git describe --tags --always --dirty` 推断，本地可覆盖，例如 `make mcp-build MCP_VERSION=v1.2.3`。构建后 `knowledge-mcp version` 会打印 Version / Commit / Date。

### CI 发布

`.github/workflows/mcp-release.yml` 的行为：

- 触发：push 到 `master`、push 版本标签 `v*.*.*`、PR 到 `master`、手动 dispatch
- `test` job：在 ubuntu / windows / macOS 三个 runner 上各自执行 `go vet` + `go test`
- `build` job：在 ubuntu 交叉编译 6 个平台，各自上传 Actions artifact（`knowledge-mcp-<goos>-<goarch>`）
- `release` job：**仅 tag `v*.*.*` 触发**，汇总所有平台包与 `checksums.txt` 上传为 GitHub Release assets；非 tag 事件只产出 artifact
- 版本号：tag 事件使用 tag 名（如 `v1.2.3`），其余使用 `dev-<short sha>`

### 凭据与配置位置

| 内容 | 路径 |
|------|------|
| 配置（TOML） | `<cwd>/.knowledge-mcp/config.toml` |
| 加密凭据 | `<cwd>/.knowledge-mcp/.credentials` |

`<cwd>` 是运行目录（当前工作目录）。凭据使用 AES-256-GCM 加密，密钥是**源码中硬编码的 32 字节固定常量**，因此只提供**混淆级**保护：拿到二进制即可逆向提取密钥，它不是强加密，也无法抵御本地有权用户读取运行目录，只是提高了文件被误分享或目录被随意翻看时的门槛。目录权限 `0700`、文件权限 `0600`（Windows 不体现 POSIX 权限位，实际由 ACL 决定）。

请把 `.knowledge-mcp/`（或至少 `.credentials`）加入项目 `.gitignore`。

### 多实例与登录

MCP 是纯 stdio 协议，**没有网络端口**；多个实例的隔离靠**不同的运行目录**——每个项目目录各自拥有 `.knowledge-mcp/`，配置与凭据互不干扰。

因此 `login` 必须在**目标项目目录**中执行，凭据才会写到该目录：

```bash
cd /path/to/your-project
knowledge-mcp login https://knowledge.company.com
```

否则 Agent 从别的目录启动 `serve` 时找不到凭据。

其他命令：`logout`、`serve`（无参数时的默认命令）、`server-list`、`server-current`、`version`。

## 配置说明

所有配置通过环境变量读取，参见 `.env.example`。

### 关键配置项

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `POSTGRES_PASSWORD` | PostgreSQL 密码（必须替换） | — |
| `DB_SSLMODE` | PG SSL 模式 | disable（Compose 内部） |
| `COOKIE_SECURE` | Cookie Secure 标志 | true |
| `SHUTDOWN_TIMEOUT` | 优雅关闭超时 | 45s |
| `JOB_WORKER_ENABLED` | 启用 Job Worker | true |
| `SCHEDULER_ENABLED` | 启用维护调度器 | true |
| `EMBEDDING_MODEL` | Embedding 模型名 | Qwen3-Embedding-8B |
| `RERANKER_MODEL` | Reranker 模型名 | Qwen3-Reranker-8B |

### Web 端口与目录路由重定向

web 容器内 Nginx 固定监听 8080，宿主端口由 `WEB_PORT` 控制（默认 80）。
Nuxt generate 会为每个路由生成目录（`login/`、`admin/` 等），访问无结尾斜杠的 `/login` 时 `try_files $uri $uri/` 命中目录，Nginx 默认的 `absolute_redirect on` + `port_in_redirect on` 会返回 `301 Location: http://<host>:8080/login/`，把浏览器地址栏改写到容器端口。本仓库已在 `web/nginx.conf.template` 中设置 `absolute_redirect off`，使 `Location` 变为相对路径。
该修正通过 compose 运行时挂载 `web/nginx.conf.template` 到容器内 `/etc/nginx/nginx.conf.template` 生效，因此使用 GHCR 已发布镜像时无需自行重建镜像。
如需更换宿主端口，只改 `WEB_PORT` 即可（容器内 8080 不变）。

## 安全注意事项

- **PG/ES 不暴露宿主端口**：仅 web 对外暴露 80 端口，server/pg/es 仅内部网络可达
- **所有 Provider Key 从环境变量读取**：不写入源码、镜像层或日志
- **.env 不进入 Docker build context**：`.dockerignore` 排除了 `.env`
- **Health 响应不泄露敏感信息**：错误信息经过过滤，不输出 DSN/API key
- **非 root 运行**：server 和 web 镜像使用非 root 用户

## 运维操作

详细的备份恢复、索引重建和清理操作请参见 `operations/` 目录下的文档：

- `operations/backup-restore.md` — PostgreSQL 备份与恢复
- `operations/index-rebuild.md` — Elasticsearch 索引重建
- `operations/maintenance.md` — 日常维护与清理

## 降级行为

| 故障 | 影响 |
|------|------|
| Elasticsearch 不可用 | 文档读写继续；搜索返回 degraded |
| Embedding 不可用 | 词汇搜索仍可用；语义路径降级 |
| Reranker 不可用 | 返回 RRF 融合结果 |
| 任何 Provider 故障 | 不阻止文档保存 |

## 日志

使用 `log/slog` 结构化日志。查看日志：

```bash
docker compose logs server
docker compose logs web
```
