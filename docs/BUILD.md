# AetherTunnel 构建文档

本文档说明如何为所有主流服务器系统编译 AetherTunnel。

---

## 📋 支持的平台

| 系统 | 架构 | 平台标识 | 状态 |
|------|------|---------|------|
| **Linux** | amd64 (x86_64) | linux/amd64 | ✅ |
| **Linux** | arm64 (AArch64) | linux/arm64 | ✅ |
| **Linux** | arm v7 (ARM) | linux/arm/v7 | ✅ |
| **Linux** | 386 (x86) | linux/386 | ✅ |
| **Linux** | ppc64le (PowerPC) | linux/ppc64le | ✅ |
| **Linux** | s390x (IBM Z) | linux/s390x | ✅ |
| **Linux** | mips64 | linux/mips64 | ✅ |
| **Linux** | mips64le | linux/mips64le | ✅ |
| **Windows** | amd64 (x86_64) | windows/amd64 | ✅ |
| **Windows** | arm64 | windows/arm64 | ✅ |
| **macOS** | amd64 (Intel) | darwin/amd64 | ✅ |
| **macOS** | arm64 (Apple M1/M2) | darwin/arm64 | ✅ |
| **FreeBSD** | amd64 (x86_64) | freebsd/amd64 | ✅ |
| **FreeBSD** | arm64 | freebsd/arm64 | ✅ |

---

## 🛠️ 编译要求

### 必需
- **Go 编译器**: 1.21 或更高版本
- **Git**: 用于版本控制

### 可选
- **upx**: 二进制压缩工具
- **Docker**: 用于容器化编译

---

## 🚀 快速开始

### 方法 1: 使用构建脚本（推荐）

```bash
# 1. 克隆仓库
git clone https://github.com/aethertunnel/aethertunnel.git
cd aethertunnel

# 2. 运行构建脚本
chmod +x scripts/build.sh
./scripts/build.sh
```

**输出**: 所有平台的二进制文件将生成在 `dist/` 目录中。

### 方法 2: 手动编译单个平台

```bash
# 编译 Linux amd64 服务端
GOOS=linux GOARCH=amd64 go build -o dist/aethertunnel-server-linux-amd64 ./server

# 编译 Linux amd64 客户端
GOOS=linux GOARCH=amd64 go build -o dist/aethertunnel-client-linux-amd64 ./client

# 编译 Windows amd64 服务端
GOOS=windows GOARCH=amd64 go build -o dist/aethertunnel-server-windows-amd64.exe ./server
```

### 方法 3: 使用 Docker 编译

```bash
# 1. 构建镜像
docker build -f Dockerfile.build -t aethertunnel-builder .

# 2. 运行编译
docker run --rm -v $(pwd)/dist:/output aethertunnel-builder

# 3. 二进制文件将在 dist/ 目录中
```

### 方法 4: 使用 GitHub Actions

1. 推送代码到 GitHub
2. GitHub Actions 将自动编译所有平台
3. 在 Actions 页面下载编译好的二进制文件

---

## 📝 编译选项

### 编译参数

```bash
go build -ldflags="-s -w" -o output-file source-path
```

**参数说明**:
- `-ldflags="-s -w"` - 去除调试信息，减小文件大小
- `-o` - 指定输出文件名
- `source-path` - 源代码路径

### 嵌入版本信息

```bash
VERSION=v0.1.0
BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
GIT_COMMIT=$(git rev-parse --short HEAD)

go build \
  -ldflags="-s -w \
    -X main.Version=$VERSION \
    -X main.BuildTime=$BUILD_TIME \
    -X main.GitCommit=$GIT_COMMIT" \
  -o output-file \
  source-path
```

### 压缩二进制文件

```bash
# 使用 gzip 压缩
gzip -9 -k output-file

# 使用 xz 压缩（更高压缩率）
xz -9 -k output-file

# 使用 upx 压缩（可执行文件压缩）
upx --best --lzma output-file
```

---

## 🐧 Linux 编译

### 标准 Linux

```bash
# Linux x86_64 (amd64)
GOOS=linux GOARCH=amd64 go build -o aethertunnel-server-linux-amd64 ./server

# Linux ARM64
GOOS=linux GOARCH=arm64 go build -o aethertunnel-server-linux-arm64 ./server

# Linux ARM v7
GOOS=linux GOARCH=arm GOARM=7 go build -o aethertunnel-server-linux-armv7 ./server
```

### 嵌入式 Linux

```bash
# MIPS64 (big endian)
GOOS=linux GOARCH=mips64 go build -o aethertunnel-server-linux-mips64 ./server

# MIPS64LE (little endian)
GOOS=linux GOARCH=mips64le go build -o aethertunnel-server-linux-mips64le ./server

# PowerPC 64LE
GOOS=linux GOARCH=ppc64le go build -o aethertunnel-server-linux-ppc64le ./server

# IBM Z (s390x)
GOOS=linux GOARCH=s390x go build -o aethertunnel-server-linux-s390x ./server
```

---

## 🪟 Windows 编译

```bash
# Windows x86_64 (amd64)
GOOS=windows GOARCH=amd64 go build -o aethertunnel-server-windows-amd64.exe ./server

# Windows ARM64
GOOS=windows GOARCH=arm64 go build -o aethertunnel-server-windows-arm64.exe ./server
```

**注意事项**:
- Windows 二进制文件必须以 `.exe` 结尾
- 可能需要安装 MinGW 或其他工具链

---

## 🍎 macOS 编译

```bash
# macOS Intel (amd64)
GOOS=darwin GOARCH=amd64 go build -o aethertunnel-server-darwin-amd64 ./server

# macOS Apple Silicon (arm64)
GOOS=darwin GOARCH=arm64 go build -o aethertunnel-server-darwin-arm64 ./server
```

**注意事项**:
- macOS 编译需要 macOS 系统
- 交叉编译需要安装适当的 SDK

---

## 🐟 FreeBSD 编译

```bash
# FreeBSD x86_64 (amd64)
GOOS=freebsd GOARCH=amd64 go build -o aethertunnel-server-freebsd-amd64 ./server

# FreeBSD ARM64
GOOS=freebsd GOARCH=arm64 go build -o aethertunnel-server-freebsd-arm64 ./server
```

**注意事项**:
- 需要在 FreeBSD 系统上编译，或使用交叉编译工具链

---

## ✅ 编译验证

### 检查二进制文件

```bash
# 显示文件信息
file aethertunnel-server-linux-amd64

# 显示文件大小
ls -lh aethertunnel-server-linux-amd64

# 显示符号表（如果有）
nm aethertunnel-server-linux-amd64
```

### 生成校验和

```bash
# 生成 SHA256 校验和
sha256sum aethertunnel-server-linux-amd64 > SHA256SUMS.txt

# 验证校验和
sha256sum -c SHA256SUMS.txt
```

### 测试运行

```bash
# Linux
./aethertunnel-server-linux-amd64 --version

# Windows
aethertunnel-server-windows-amd64.exe --version
```

---

## 🔧 故障排查

### 问题 1: 找不到 Go 编译器

```bash
# 检查 Go 版本
go version

# 如果未安装，访问 https://golang.org/dl/
```

### 问题 2: 交叉编译失败

```bash
# 确保设置了正确的 GOOS 和 GOARCH
export GOOS=linux
export GOARCH=amd64
go build ./server
```

### 问题 3: Windows 编译失败

```bash
# 可能需要 CGO
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./server
```

### 问题 4: 文件过大

```bash
# 使用 upx 压缩（可执行文件压缩）
upx --best --lzma aethertunnel-server-linux-amd64
```

---

## 📦 打包和发布

### 创建发布包

```bash
#!/bin/bash

VERSION=v0.1.0
ARCH=amd64

# 创建目录
mkdir -p release/aethertunnel-${VERSION}-linux-${ARCH}

# 复制文件
cp aethertunnel-server-linux-amd64 release/aethertunnel-${VERSION}-linux-${ARCH}/
cp aethertunnel-client-linux-amd64 release/aethertunnel-${VERSION}-linux-${ARCH}/
cp server.toml.example release/aethertunnel-${VERSION}-linux-${ARCH}/
cp client.toml.example release/aethertunnel-${VERSION}-linux-${ARCH}/
cp README.md release/aethertunnel-${VERSION}-linux-${ARCH}/

# 创建 tarball
cd release
tar -czf aethertunnel-${VERSION}-linux-${ARCH}.tar.gz aethertunnel-${VERSION}-linux-${ARCH}
```

### 发布到 GitHub

1. 创建新的 Release
2. 上传所有平台的二进制文件
3. 上传 SHA256SUMS.txt
4. 添加 Release Notes

---

## 📊 性能优化

### 编译优化

```bash
# 启用优化
go build -ldflags="-s -w" ./server

# 使用 upx 压缩
upx --best --lzma aethertunnel-server
```

### 运行时优化

```bash
# 使用更高优先级运行（Linux）
nice -n -10 ./aethertunnel-server

# 设置 CPU 亲和性
taskset -c 0,1 ./aethertunnel-server
```

---

## 🚢 持续集成

### GitHub Actions

项目已配置 GitHub Actions 自动编译：

```yaml
name: Build AetherTunnel
on:
  push:
    tags:
      - 'v*'
```

### 自动化流程

1. 推送代码
2. 自动触发编译
3. 编译所有平台
4. 生成校验和
5. 自动创建 Release

---

## 🎯 最佳实践

1. **版本控制**: 始终使用语义化版本（如 v1.0.0）
2. **校验和**: 始终提供 SHA256 校验和
3. **压缩**: 使用 gzip 或 xz 压缩二进制文件
4. **测试**: 编译后在目标平台上测试
5. **文档**: 提供详细的编译和安装文档

---

## 📚 相关文档

- [快速开始](QUICK_START.md)
- [使用指南](docs/USAGE.md)
- [配置指南](docs/DASHBOARD_CONFIG.md)
- [API 文档](docs/API.md)

---

<div align="center">

**🎉 祝编译顺利！**

如有问题，请查看故障排查或提交 Issue。

Made with ❤️ by AetherTunnel Team

</div>
