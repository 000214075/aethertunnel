# AetherTunnel Makefile
# 简化编译和开发流程

.PHONY: all build test clean lint fmt vet help

# 项目信息
PROJECT_NAME := aethertunnel
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0-dev")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# 编译参数
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME) -X main.GitCommit=$(GIT_COMMIT)

# 输出目录
DIST_DIR := ./dist
BUILD_DIR := ./build

# 默认目标
all: fmt vet test build

## help: 显示帮助信息
help:
	@echo "AetherTunnel 构建系统"
	@echo ""
	@echo "可用命令:"
	@echo "  make build       - 编译所有平台的二进制文件"
	@echo "  make build-local - 编译本地平台"
	@echo "  make test        - 运行测试"
	@echo "  make fmt         - 格式化代码"
	@echo "  make vet         - 运行 go vet"
	@echo "  make lint        - 运行 golangci-lint"
	@echo "  make clean       - 清理构建文件"
	@echo "  make docker      - 使用 Docker 编译"
	@echo "  make release     - 创建发布包"
	@echo ""

## build-local: 编译本地平台
build-local: fmt
	@echo "编译本地平台..."
	@mkdir -p $(DIST_DIR)
	@go build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(PROJECT_NAME)-server main.go
	@go build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(PROJECT_NAME)-client ./client/main.go
	@echo "✅ 编译完成: $(DIST_DIR)"

## build: 编译所有平台
build:
	@echo "编译所有平台..."
	@chmod +x scripts/build.sh
	@./scripts/build.sh

## test: 运行测试
test:
	@echo "运行测试..."
	@go test -v -race -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "✅ 测试完成，覆盖率报告: coverage.html"

## fmt: 格式化代码
fmt:
	@echo "格式化代码..."
	@gofmt -w .
	@echo "✅ 代码已格式化"

## vet: 运行 go vet
vet:
	@echo "运行 go vet..."
	@go vet ./...
	@echo "✅ vet 检查通过"

## lint: 运行 golangci-lint
lint:
	@echo "运行 golangci-lint..."
	@golangci-lint run
	@echo "✅ lint 检查通过"

## clean: 清理构建文件
clean:
	@echo "清理构建文件..."
	@rm -rf $(DIST_DIR)
	@rm -rf $(BUILD_DIR)
	@rm -f coverage.out coverage.html
	@echo "✅ 清理完成"

## docker: 使用 Docker 编译
docker:
	@echo "使用 Docker 编译..."
	@docker build -f Dockerfile.build -t aethertunnel-builder .
	@docker run --rm -v $(PWD)/dist:/output aethertunnel-builder
	@echo "✅ Docker 编译完成"

## release: 创建发布包
release: build
	@echo "创建发布包..."
	@mkdir -p release
	@for dir in $(DIST_DIR)/*; do \
		base=$$(basename $$dir); \
		mkdir -p release/$(PROJECT_NAME)-$(VERSION)-$$base; \
		cp $$dir release/$(PROJECT_NAME)-$(VERSION)-$$base/; \
		cp server.toml.example release/$(PROJECT_NAME)-$(VERSION)-$$base/; \
		cp client.toml.example release/$(PROJECT_NAME)-$(VERSION)-$$base/; \
		cp README.md release/$(PROJECT_NAME)-$(VERSION)-$$base/; \
		cd release && tar -czf $(PROJECT_NAME)-$(VERSION)-$$base.tar.gz $(PROJECT_NAME)-$(VERSION)-$$base && rm -rf $(PROJECT_NAME)-$(VERSION)-$$base; \
	done
	@echo "✅ 发布包已创建: release/"

## check: 检查依赖和配置
check:
	@echo "检查 Go 版本..."
	@go version
	@echo ""
	@echo "检查依赖..."
	@go mod verify
	@go mod tidy
	@echo ""
	@echo "✅ 检查完成"

## deps: 更新依赖
deps:
	@echo "更新依赖..."
	@go get -u ./...
	@go mod tidy
	@echo "✅ 依赖已更新"

## version: 显示版本信息
version:
	@echo "项目: $(PROJECT_NAME)"
	@echo "版本: $(VERSION)"
	@echo "构建时间: $(BUILD_TIME)"
	@echo "Git 提交: $(GIT_COMMIT)"

## install: 安装到本地
install: build-local
	@echo "安装到本地..."
	@cp $(DIST_DIR)/$(PROJECT_NAME)-server $$(go env GOPATH)/bin/
	@cp $(DIST_DIR)/$(PROJECT_NAME)-client $$(go env GOPATH)/bin/
	@echo "✅ 安装完成"

## uninstall: 从本地卸载
uninstall:
	@echo "从本地卸载..."
	@rm -f $$(go env GOPATH)/bin/$(PROJECT_NAME)-server
	@rm -f $$(go env GOPATH)/bin/$(PROJECT_NAME)-client
	@echo "✅ 卸载完成"
