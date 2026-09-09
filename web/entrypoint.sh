#!/bin/sh
# web/entrypoint.sh — Nginx 运行时入口脚本
#
# 引入动机：Nginx 配置需要运行时从环境变量获取 server upstream 地址，
# 不在 Nuxt 构建阶段烘焙内部地址。
#
# 机制：使用 envsubst 将 nginx.conf.template 中的 ${SERVER_UPSTREAM}
# 替换为实际环境变量值，生成最终 nginx.conf 后启动 Nginx。
#
# 环境变量：
#   SERVER_UPSTREAM — Go 后端地址（如 server:8080），默认 localhost:8080
#
# 安全原则：不读取或输出任何 secret、.env、Cookie、Token。

set -e

# 默认值：本地开发时后端在 localhost:8080
SERVER_UPSTREAM="${SERVER_UPSTREAM:-localhost:8080}"

# 导出供 envsubst 使用
export SERVER_UPSTREAM

# 生成最终 Nginx 配置
envsubst '${SERVER_UPSTREAM}' < /etc/nginx/nginx.conf.template > /etc/nginx/nginx.conf

# 验证 Nginx 配置语法
nginx -t

# 启动 Nginx（前台运行，daemon off）
exec nginx -g 'daemon off;'
