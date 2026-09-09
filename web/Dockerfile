# web/Dockerfile — Nuxt 4 前端多阶段构建（Nginx runtime）
#
# 引入动机：design/05-OPERATIONS.md 要求 Docker Compose 部署 web。
# 生产架构改为 Nuxt CSR 静态构建 + Nginx runtime。
# Nginx 提供静态文件、SPA fallback 和 /api 反向代理。
# Nginx 配置运行时从环境变量得到 server upstream，不在构建阶段烘焙内部地址。
#
# 构建阶段：使用 Corepack + Yarn 4 不可变安装，nuxt generate 产出静态 CSR 产物。
# 运行时阶段：nginx:alpine 非 root 用户，entrypoint.sh 通过 envsubst 注入 upstream。

# --- Builder 阶段 ---
FROM node:22-alpine AS builder

# 启用 Corepack 以使用 Yarn 4
RUN corepack enable

WORKDIR /build

# 先复制 package.json、yarn.lock 和 .yarnrc.yml，利用 Docker 层缓存
COPY web/package.json web/yarn.lock web/.yarnrc.yml ./

# 不可变安装：lockfile 不匹配时失败
RUN yarn install --immutable

# 复制源码
COPY web/ ./

# 重新生成 Nuxt 类型定义（.nuxt/imports.d.ts 等）
# 引入动机：postinstall 阶段已执行 nuxt prepare，但 COPY web/ ./ 覆盖源码后
# 需要重新生成类型定义，确保 typeCheck: true 能正确解析 Nuxt auto-imports
# （useI18n、definePageMeta、useAuth 等）和模块注入的类型（@nuxt/fonts 等）。
# 不执行此步骤会导致 vue-tsc 报 TS2304 Cannot find name 错误。
RUN yarn nuxt prepare

# 构建 Nuxt 静态 CSR 产物
# 设置 NUXT_PUBLIC_API_BASE=/api 使前端通过相对路径访问 API
# 引入动机：design/05-OPERATIONS.md 要求浏览器仅使用相对路径 /api，
# 生产环境由 Nginx 反向代理到内部 server，不暴露内部 hostname。
ENV NUXT_PUBLIC_API_BASE="/api"
RUN yarn generate

# --- Runtime 阶段 ---
FROM nginx:alpine AS runtime

# 安装 gettext 提供 envsubst 命令；curl 供非 root HEALTHCHECK 探测本地 /healthz
# 修复说明：nginx:alpine 官方镜像不自带 wget/curl，原 HEALTHCHECK 用 wget 在容器内永远失败，
# 导致 web 持续 unhealthy。curl 必须在 USER partitura 前以 root 安装。
RUN apk add --no-cache gettext curl

# 创建非 root 用户
# 引入动机：安全原则要求非 root 运行。nginx:alpine 默认以 root 运行，
# 需要创建非 root 用户并确保 pid/temp 路径可写。
RUN addgroup -S partitura && adduser -S partitura -G partitura

# 创建非 root 用户可写的目录
# pid 和 temp 路径在 nginx.conf.template 中配置为 /tmp/ 下，确保可写
RUN mkdir -p /tmp/client_body /tmp/proxy /tmp/fastcgi /tmp/uwsgi /tmp/scgi && \
    chown -R partitura:partitura /tmp /var/cache/nginx /var/log/nginx /etc/nginx

WORKDIR /app

# 从 builder 复制 Nuxt 静态产物到 Nginx 服务目录
COPY --from=builder /build/.output/public /app/dist

# 复制 Nginx 配置模板和 entrypoint
COPY web/nginx.conf.template /etc/nginx/nginx.conf.template
COPY web/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# 切换到非 root 用户
USER partitura

# 容器端口契约：Nginx 监听 8080
EXPOSE 8080

# 运行时环境变量：Go 后端 upstream 地址
# 在 compose.yaml 中设置：SERVER_UPSTREAM=server:8080
# 通过 entrypoint.sh 的 envsubst 注入 nginx.conf，不烘焙进构建产物
ENV SERVER_UPSTREAM="localhost:8080"

# 健康检查：Nginx /healthz 端点（curl 已在上方以 root 安装）
HEALTHCHECK --interval=15s --timeout=10s --retries=5 --start-period=10s \
    CMD curl -fsS http://localhost:8080/healthz || exit 1

# 启动 entrypoint：envsubst 生成配置 + 启动 Nginx
CMD ["/entrypoint.sh"]
