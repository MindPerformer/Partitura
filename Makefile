.PHONY: build test test-integration migrate-up migrate-down migrate-status clean e2e e2e-setup e2e-down mcp-build mcp-cross mcp-test mcp-vet

# Go 相关变量
GO        := go
SERVER_DIR := server
MCP_DIR   := mcp
MCP_DIST  := dist

# MCP 构建元信息（经 ldflags 注入 partitura/mcp/internal/buildinfo）
MCP_VERSION ?= dev
MCP_COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
MCP_DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
MCP_LDFLAGS := -s -w -X partitura/mcp/internal/buildinfo.Version=$(MCP_VERSION) -X partitura/mcp/internal/buildinfo.Commit=$(MCP_COMMIT) -X partitura/mcp/internal/buildinfo.Date=$(MCP_DATE)
MCP_EXE     := $(if $(findstring windows,$(shell $(GO) env GOOS)),.exe,)

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

# ==== MCP（mcp/ 独立 Go module：knowledge-mcp）====
# 跨平台打包的构建逻辑统一收敛在 scripts/build-mcp.sh，本地与 CI 共用同一真相源；
# 如需 MCP_VERSION 之外的注入值，可覆盖变量：make mcp-build MCP_VERSION=v1.2.3

# 构建当前平台二进制到 dist/（含 ldflags 版本注入）
mcp-build:
	mkdir -p $(MCP_DIST)
	cd $(MCP_DIR) && CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(MCP_LDFLAGS)" -o ../$(MCP_DIST)/knowledge-mcp$(MCP_EXE) ./cmd/knowledge-mcp

# 构建全部 6 个平台（windows/linux/darwin × amd64/arm64）并打包 + 生成校验和
mcp-cross:
	bash scripts/build-mcp.sh "$(MCP_VERSION)" $(MCP_DIST)

# MCP 单元测试
mcp-test:
	cd $(MCP_DIR) && $(GO) test ./...

# MCP 静态检查
mcp-vet:
	cd $(MCP_DIR) && $(GO) vet ./...
