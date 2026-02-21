#!/bin/bash
# AetherTunnel 简化跨平台编译脚本
# 暂时跳过有依赖问题的平台

# set -e  # 临时禁用，避免脚本在第一个错误时退出

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# 项目信息
PROJECT_NAME="aethertunnel"
VERSION="v1.0.2"
BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# 输出目录
DIST_DIR="./dist"
mkdir -p "$DIST_DIR"

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}  AetherTunnel 简化跨平台编译脚本${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""
echo -e "${GREEN}项目名称${NC}: $PROJECT_NAME"
echo -e "${GREEN}版本${NC}: $VERSION"
echo -e "${GREEN}构建时间${NC}: $BUILD_TIME"
echo -e "${GREEN}Git 提交${NC}: $GIT_COMMIT"
echo ""

# 检查 Go 环境
if ! command -v go &> /dev/null; then
    echo -e "${RED}错误: 未找到 Go 编译器${NC}"
    echo "请先安装 Go: https://golang.org/dl/"
    exit 1
fi

GO_VERSION=$(go version | awk '{print $3}')
echo -e "${GREEN}Go 版本${NC}: $GO_VERSION"
echo ""

# 简化的编译目标平台（暂时跳过有依赖问题的平台）
PLATFORMS=(
    # Windows
    "windows/amd64"
    
    # Linux
    "linux/amd64"
    
    # macOS
    "darwin/amd64"
)

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}  开始编译${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

# 统计
TOTAL=0
SUCCESS=0
FAILED=0
FAILED_PLATFORMS=()

# 编译函数
build_binary() {
    local target=$1
    local name=$2

    echo -e "${YELLOW}编译${NC}: $name"
    echo -e "  目标: $target"

    # 分离 GOOS 和 GOARCH
    IFS='/' read -ra parts <<< "$target"
    local goos=${parts[0]}
    local goarch=${parts[1]}

    # 设置输出文件名
    local output_name
    if [ "$goos" = "windows" ]; then
        output_name="${name}.exe"
    else
        output_name="${name}"
    fi

    # 输出路径
    local output_path="$DIST_DIR/${name}-${goos}-${goarch}"
    [ "$goos" = "windows" ] && output_path="${output_path}.exe"

    # 编译参数
    local ldflags="-s -w -X main.Version=$VERSION -X main.BuildTime=$BUILD_TIME -X main.GitCommit=$GIT_COMMIT"

    # 编译
    if CGO_ENABLED=0 go build -ldflags "$ldflags" -o "$output_path" ./main_minimal.go; then
        echo -e "  ${GREEN}✅ 成功${NC}: $output_path"
        ((SUCCESS++))
    else
        echo -e "  ${RED}❌ 失败${NC}: $target"
        ((FAILED++))
        FAILED_PLATFORMS+=("$target")
    fi

    echo ""
    ((TOTAL++))
}

# 确保脚本继续执行

# 编译服务端
echo -e "${BLUE}编译服务端...${NC}"
echo ""
for platform in "${PLATFORMS[@]}"; do
    build_binary "$platform" "${PROJECT_NAME}-server"
done

# 生成校验和
echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}  生成校验和${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

cd "$DIST_DIR"
if command -v sha256sum &> /dev/null; then
    sha256sum * > SHA256SUMS.txt
    echo -e "${GREEN}✅ SHA256 校验和已生成${NC}"
elif command -v shasum &> /dev/null; then
    shasum -a 256 * > SHA256SUMS.txt
    echo -e "${GREEN}✅ SHA256 校验和已生成${NC}"
else
    echo -e "${YELLOW}⚠️  警告: 未找到 sha256sum 或 shasum 工具${NC}"
fi

echo ""

# 编译报告
echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}  编译报告${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""
echo -e "总任务: $TOTAL"
echo -e "${GREEN}成功: $SUCCESS${NC}"
echo -e "${RED}失败: $FAILED${NC}"
echo ""

if [ $FAILED -gt 0 ]; then
    echo -e "${RED}失败的平台:${NC}"
    for platform in "${FAILED_PLATFORMS[@]}"; do
        echo "  - $platform"
    done
    echo ""
    exit 1
fi

# 列出编译的二进制文件
echo -e "${BLUE}编译的二进制文件:${NC}"
echo ""
ls -lh "$DIST_DIR"

echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  编译完成！${NC}"
echo -e "${GREEN}========================================${NC}"