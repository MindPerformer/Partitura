# server/Dockerfile — Go 后端多阶段构建
#
# 引入动机：design/05-OPERATIONS.md 要求 Docker Compose 部署 server。
# 多阶段构建：builder 阶段下载依赖并编译，runtime 阶段仅包含二进制和迁移文件。
# 运行镜像使用非 root 用户，最小化攻击面。

# --- Builder 阶段 ---
FROM golang:1.26-alpine AS builder

# 安装编译所需的工具链（alpine 缺少 gcc 和 git）
RUN apk add --no-cache gcc musl-dev git

WORKDIR /build

# 先复制 go.mod/go.sum，利用 Docker 层缓存下载依赖
COPY server/go.mod server/go.sum ./
RUN go mod download

# 复制源码
COPY server/ ./

# 编译静态二进制（CGO_ENABLED=0 适配 alpine 无 libc 环境）
# -ldflags="-s -w" 去除调试信息减小镜像体积
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /partitura-server ./cmd/server

# --- Runtime 阶段 ---
FROM alpine:3.21

# 安装 ca-certificates 以支持 HTTPS 调用外部 Provider
RUN apk add --no-cache ca-certificates tzdata

# 创建非 root 用户
RUN addgroup -S partitura && adduser -S partitura -G partitura

WORKDIR /app

# 从 builder 复制编译产物
COPY --from=builder /partitura-server /app/partitura-server

# 复制迁移文件（findMigrationsDir 运行时定位 ../migrations）
COPY server/migrations /app/migrations

# 切换到非 root 用户
USER partitura

EXPOSE 8080

# 环境变量默认值（实际配置通过 compose.yaml env_file 注入）
ENV HTTP_ADDR=":8080" \
    MIGRATIONS_DIR="/app/migrations"

ENTRYPOINT ["/app/partitura-server"]
