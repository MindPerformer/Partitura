.PHONY: build test test-integration migrate-up migrate-down migrate-status clean e2e e2e-setup e2e-down

# Go 相关变量
GO        := go
SERVER_DIR := server

# 构建
build:
	cd $(SERVER_DIR) && $(GO) build ./...

# 运行所有测试（包括集成测试，如果 TEST_DATABASE_URL 已设置）
test:
	cd $(SERVER_DIR) && $(GO) test ./...

# 仅运行集成测试（需要 TEST_DATABASE_URL）
test-integration:
	cd $(SERVER_DIR) && $(GO) test -v -run Integration ./...

# 执行迁移 Up（需要 DB 环境变量）
migrate-up:
	cd $(SERVER_DIR) && $(GO) run ./cmd/server

# 查看迁移状态（需要 DB 环境变量）
migrate-status:
	cd $(SERVER_DIR) && $(GO) run ./cmd/server -migrate-status

# 清理构建产物
clean:
	cd $(SERVER_DIR) && $(GO) clean

# 下载依赖
deps:
	cd $(SERVER_DIR) && $(GO) mod tidy

# vet
vet:
	cd $(SERVER_DIR) && $(GO) vet ./...

# ==== E2E（本地 Docker 实机语义搜索测试）====
# 跨平台：Linux / WSL 直接执行；Windows GitBash 自动转交 WSL。
# 前置：docker(compose v2)；密钥文件 ~/.config/partitura/e2e.env（见 scripts/e2e/env.example）
e2e:
	bash scripts/e2e/run-e2e.sh

# 一键准备 docker 运行环境（Linux/WSL；幂等）
e2e-setup:
	bash scripts/e2e/setup-wsl-docker.sh

# 停止 E2E 容器（默认保留，仅显式停止）
e2e-down:
	bash scripts/e2e/run-e2e.sh --down
