# Makefile for Partitura server
# 提供常用构建、测试、迁移等开发命令

# Go 相关变量
GOCMD = go
GOBUILD = $(GOCMD) build
GOTEST = $(GOCMD) test
GOVET = $(GOCMD) vet

# 服务器二进制名称
BINARY = bin/server

# 默认目标
.PHONY: all
all: build

# 编译服务器
.PHONY: build
build:
	$(GOBUILD) -o $(BINARY) ./cmd/server

# 运行服务器
.PHONY: run
run:
	$(GOCMD) run ./cmd/server

# 查询当前迁移版本（不执行迁移、不启动 HTTP、不等待信号）
# 需要设置 DB_HOST/DB_PORT/DB_USER/DB_PASSWORD/DB_NAME/DB_SSLMODE 环境变量
.PHONY: migrate-status
migrate-status:
	$(GOCMD) run ./cmd/server -migrate-status

# 执行迁移并启动服务器
.PHONY: migrate-up
migrate-up:
	$(GOCMD) run ./cmd/server

# 运行全部测试
.PHONY: test
test:
	$(GOTEST) ./...

# 运行 go vet
.PHONY: vet
vet:
	$(GOVET) ./...

# 清理构建产物
.PHONY: clean
clean:
	rm -rf bin/
