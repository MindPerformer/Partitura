# Partitura — Project Knowledge Workspace

## 概述

Partitura 是一个企业级 Project Knowledge Workspace，支持多用户、多 Workspace、Markdown 知识管理、语义搜索和版本控制。

## 架构

| 服务 | 技术栈 | 角色 |
|------|--------|------|
| server | Go | REST API、Job Worker、Scheduler |
| web | Nuxt 4 + Nuxt UI 4 | 前端 SSR |
| postgres | PostgreSQL 17 | 业务真相源 |
| elasticsearch | Elasticsearch 8.17 | 可重建搜索索引 |

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
