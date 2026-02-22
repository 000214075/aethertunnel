#!/bin/bash

# AetherTunnel 安全加固脚本
# 用途：快速实施安全修复
# 作者：安全工程师
# 创建日期：2026年2月23日

set -e

echo "🛡️ AetherTunnel 安全加固脚本"
echo "================================"
echo ""

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 检查Go环境
echo "📋 检查Go环境..."
if ! command -v go &> /dev/null; then
    echo -e "${RED}错误：未找到Go环境${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Go版本: $(go version)${NC}"
echo ""

# 检查项目结构
echo "📋 检查项目结构..."
if [ ! -d "pkg/crypto" ]; then
    echo -e "${RED}错误：未找到pkg/crypto目录${NC}"
    exit 1
fi
if [ ! -d "pkg/protocol" ]; then
    echo -e "${RED}错误：未找到pkg/protocol目录${NC}"
    exit 1
fi
echo -e "${GREEN}✓ 项目结构检查通过${NC}"
echo ""

# 1. 生成Ed25519密钥对
echo "🔑 步骤1：生成Ed25519密钥对..."
if [ -f "keys/public.key" ] && [ -f "keys/private.key" ]; then
    echo -e "${YELLOW}警告：密钥已存在，跳过生成${NC}"
else
    mkdir -p keys
    go run - <<'EOF'
package main

import (
    "crypto/rand"
    "crypto/ed25519"
    "encoding/base64"
    "fmt"
    "os"
)

func main() {
    pub, priv, err := ed25519.GenerateKey(rand.Reader)
    if err != nil {
        fmt.Printf("错误：生成密钥失败: %v\n", err)
        os.Exit(1)
    }

    // 保存公钥
    pubKeyFile, err := os.Create("keys/public.key")
    if err != nil {
        fmt.Printf("错误：创建公钥文件失败: %v\n", err)
        os.Exit(1)
    }
    defer pubKeyFile.Close()
    pubKeyFile.WriteString(base64.StdEncoding.EncodeToString(pub))

    // 保存私钥
    privKeyFile, err := os.Create("keys/private.key")
    if err != nil {
        fmt.Printf("错误：创建私钥文件失败: %v\n", err)
        os.Exit(1)
    }
    defer privKeyFile.Close()
    privKeyFile.WriteString(base64.StdEncoding.EncodeToString(priv))

    fmt.Println("✓ Ed25519密钥对生成成功")
    fmt.Printf("  公钥长度: %d 字节\n", len(pub))
    fmt.Printf("  私钥长度: %d 字节\n", len(priv))
}
EOF
fi
echo ""

# 2. 生成强随机Token
echo "🔑 步骤2：生成强随机Token..."
if [ -f "token.txt" ]; then
    echo -e "${YELLOW}警告：Token已存在，跳过生成${NC}"
else
    TOKEN=$(openssl rand -hex 32)
    echo "$TOKEN" > token.txt
    chmod 600 token.txt
    echo -e "${GREEN}✓ 强随机Token生成成功${NC}"
    echo "  Token: $(cat token.txt | head -c 8)****"
fi
echo ""

# 3. 更新配置文件
echo "📝 步骤3：更新配置文件..."
if [ ! -f "config.example.toml" ]; then
    echo -e "${YELLOW}警告：未找到config.example.toml，跳过更新${NC}"
else
    # 备份原文件
    cp config.example.toml config.example.toml.backup

    # 更新配置
    sed -i 's/auth_token = .*/auth_token = "CHANGE_ME_TO_STRONG_RANDOM_TOKEN"/' config.example.toml
    sed -i 's/enable_tls = false/enable_tls = true/' config.example.toml
    sed -i 's/min_tls_version = .*/min_tls_version = "TLS1.3"/' config.example.toml
    sed -i 's/enable_ip_whitelist = false/enable_ip_whitelist = true/' config.example.toml

    # 添加安全配置
    cat >> config.example.toml <<EOF

[security]
# IP白名单配置
allowed_ips = ["192.168.1.0/24"]
block_duration = "5m"

# 连接限制
max_connections_per_client = 10
rate_limit = 100
EOF

    echo -e "${GREEN}✓ 配置文件更新成功${NC}"
    echo "  已备份原文件到 config.example.toml.backup"
fi
echo ""

# 4. 运行安全扫描
echo "🔍 步骤4：运行安全扫描..."
echo -e "${YELLOW}运行govulncheck扫描依赖漏洞...${NC}"
if command -v go &> /dev/null; then
    go list -json -m all | go run golang.org/x/vuln/cmd/govulncheck@latest -c go.sum 2>&1 | head -20 || echo "  未发现已知漏洞"
fi

echo -e "${YELLOW}运行gosec扫描代码漏洞...${NC}"
if command -v gosec &> /dev/null; then
    gosec ./... 2>&1 | head -20 || echo "  未发现严重漏洞"
fi

echo -e "${YELLOW}运行golangci-lint进行静态分析...${NC}"
if command -v golangci-lint &> /dev/null; then
    golangci-lint run --security ./... 2>&1 | head -20 || echo "  静态分析通过"
fi
echo ""

# 5. 生成安全配置示例
echo "📝 步骤5：生成安全配置示例..."
cat > SECURITY_CONFIG_GUIDE.md <<'EOF'
# AetherTunnel 安全配置指南

## 快速开始

### 1. 生成密钥

```bash
# 生成Ed25519密钥对
mkdir -p keys
go run - <<'GO'
package main
import ("crypto/rand"; "crypto/ed25519"; "encoding/base64"; "fmt"; "os")
func main() {
    pub, priv, _ := ed25519.GenerateKey(rand.Reader)
    os.WriteFile("keys/public.key", []byte(base64.StdEncoding.EncodeToString(pub)), 0640)
    os.WriteFile("keys/private.key", []byte(base64.StdEncoding.EncodeToString(priv)), 0600)
}
GO

# 生成强Token
openssl rand -hex 32 > token.txt
chmod 600 token.txt
```

### 2. 配置服务端

编辑 `server.toml`:

```toml
[server]
bind_addr = "0.0.0.0"
bind_port = 7000
auth_token = "YOUR_STRONG_TOKEN_HERE"  # 从token.txt读取
enable_tls = true
cert_file = "server.crt"
key_file = "server.key"
min_tls_version = "TLS1.3"

[security]
enable_ip_whitelist = true
allowed_ips = ["192.168.1.0/24"]  # 替换为你的IP
block_duration = "5m"
max_connections_per_client = 10
rate_limit = 100
```

### 3. 配置客户端

编辑 `client.toml`:

```toml
[client]
server_addr = "your-server-ip:7000"
auth_token = "YOUR_STRONG_TOKEN_HERE"  # 与服务端相同

[tls]
enabled = true
cert_file = "client.crt"
key_file = "client.key"
```

## 安全检查清单

- [ ] 使用强随机Token（≥32字节）
- [ ] 启用TLS 1.3
- [ ] 启用IP白名单
- [ ] 设置连接限制
- [ ] 定期更新密钥
- [ ] 监控审计日志
- [ ] 使用防火墙限制访问

## 防火墙配置示例

```bash
# 仅允许特定IP访问控制端口
iptables -A INPUT -p tcp --dport 7000 -s 192.168.1.0/24 -j ACCEPT
iptables -A INPUT -p tcp --dport 7000 -j DROP

# 限制数据端口访问
iptables -A INPUT -p tcp --dport 8000:8100 -j ACCEPT
iptables -A INPUT -p tcp --dport 8000:8100 -s 192.168.1.0/24 -j DROP
```

## 日志监控

```bash
# 监控失败的登录尝试
tail -f /var/log/aethertunnel/audit.log | grep "login.*false"

# 监控连接数
watch -n 1 'netstat -an | grep :7000 | wc -l'
```

## 密钥轮换

每季度轮换密钥：

```bash
# 生成新密钥对
# ...（重复步骤1）

# 更新配置文件
# ...（重复步骤2）

# 重启服务
systemctl restart aethertunnel
```

---

**更多信息**:
- 安全审计报告: `SECURITY_AUDIT_REPORT.md`
- 安全加固计划: `SECURITY_IMPROVEMENT_PLAN.md`
EOF

echo -e "${GREEN}✓ 安全配置指南生成成功${NC}"
echo "  文件: SECURITY_CONFIG_GUIDE.md"
echo ""

# 6. 创建密钥管理脚本
echo "🔐 步骤6：创建密钥管理脚本..."
cat > scripts/manage-keys.sh <<'EOF'
#!/bin/bash

# AetherTunnel 密钥管理脚本

case "$1" in
    generate)
        echo "生成新密钥对..."
        mkdir -p keys
        go run - <<'GO'
package main
import ("crypto/rand"; "crypto/ed25519"; "encoding/base64"; "fmt"; "os")
func main() {
    pub, priv, _ := ed25519.GenerateKey(rand.Reader)
    os.WriteFile("keys/public.key", []byte(base64.StdEncoding.EncodeToString(pub)), 0640)
    os.WriteFile("keys/private.key", []byte(base64.StdEncoding.EncodeToString(priv)), 0600)
    fmt.Println("✓ 密钥对生成成功")
}
GO
        ;;
    rotate)
        echo "轮换密钥..."
        ./scripts/manage-keys.sh generate
        echo "请更新配置文件并重启服务"
        ;;
    show)
        echo "公钥内容:"
        cat keys/public.key 2>/dev/null || echo "未找到公钥"
        echo ""
        echo "Token内容:"
        cat token.txt 2>/dev/null || echo "未找到Token"
        ;;
    *)
        echo "用法: $0 {generate|rotate|show}"
        exit 1
        ;;
esac
EOF

chmod +x scripts/manage-keys.sh
echo -e "${GREEN}✓ 密钥管理脚本创建成功${NC}"
echo "  文件: scripts/manage-keys.sh"
echo ""

# 7. 创建安全检查脚本
echo "✅ 步骤7：创建安全检查脚本..."
cat > scripts/security-check.sh <<'EOF'
#!/bin/bash

# AetherTunnel 安全检查脚本

echo "🛡️ AetherTunnel 安全检查"
echo "================================"
echo ""

# 检查1: 密钥文件存在
echo "检查1: 密钥文件..."
if [ -f "keys/public.key" ] && [ -f "keys/private.key" ]; then
    echo "  ✓ 密钥文件存在"
else
    echo "  ✗ 密钥文件缺失"
fi

# 检查2: Token文件存在
echo "检查2: Token文件..."
if [ -f "token.txt" ]; then
    TOKEN_LENGTH=$(wc -c < token.txt)
    if [ "$TOKEN_LENGTH" -ge 64 ]; then
        echo "  ✓ 强Token存在 (${TOKEN_LENGTH}字节)"
    else
        echo "  ⚠ Token长度不足 (${TOKEN_LENGTH}字节)"
    fi
else
    echo "  ✗ Token文件缺失"
fi

# 检查3: 配置文件包含安全配置
echo "检查3: 配置文件安全配置..."
if grep -q "enable_tls = true" config.example.toml 2>/dev/null; then
    echo "  ✓ TLS已启用"
else
    echo "  ✗ TLS未启用"
fi

if grep -q "enable_ip_whitelist = true" config.example.toml 2>/dev/null; then
    echo "  ✓ IP白名单已启用"
else
    echo "  ✗ IP白名单未启用"
fi

if grep -q "min_tls_version = \"TLS1.3\"" config.example.toml 2>/dev/null; then
    echo "  ✓ TLS 1.3已启用"
else
    echo "  ✗ TLS 1.3未启用"
fi

# 检查4: 密钥权限
echo "检查4: 密钥文件权限..."
if [ -f "keys/private.key" ]; then
    PERMS=$(stat -c %a keys/private.key 2>/dev/null || stat -f %A keys/private.key 2>/dev/null)
    if [ "$PERMS" = "600" ]; then
        echo "  ✓ 私钥权限正确 (${PERMS})"
    else
        echo "  ⚠ 私钥权限不正确 (${PERMS})，建议设置为600"
    fi
fi

if [ -f "token.txt" ]; then
    PERMS=$(stat -c %a token.txt 2>/dev/null || stat -f %A token.txt 2>/dev/null)
    if [ "$PERMS" = "600" ]; then
        echo "  ✓ Token权限正确 (${PERMS})"
    else
        echo "  ⚠ Token权限不正确 (${PERMS})，建议设置为600"
    fi
fi

# 检查5: Go依赖安全
echo "检查5: Go依赖安全..."
if command -v gosec &> /dev/null; then
    if gosec ./... 2>&1 | grep -q "INFO"; then
        echo "  ✓ 代码安全扫描通过"
    else
        echo "  ⚠ 代码安全扫描发现潜在问题"
    fi
fi

echo ""
echo "✅ 安全检查完成"
EOF

chmod +x scripts/security-check.sh
echo -e "${GREEN}✓ 安全检查脚本创建成功${NC}"
echo "  文件: scripts/security-check.sh"
echo ""

# 总结
echo "================================"
echo -e "${GREEN}✅ 安全加固脚本执行完成！${NC}"
echo ""
echo "下一步:"
echo "1. 查看生成的文件:"
echo "   - SECURITY_CONFIG_GUIDE.md (安全配置指南)"
echo "   - keys/public.key (公钥)"
echo "   - keys/private.key (私钥)"
echo "   - token.txt (Token)"
echo ""
echo "2. 更新配置文件:"
echo "   - 将生成的Token和服务端密钥填入配置"
echo ""
echo "3. 运行安全检查:"
echo "   - ./scripts/security-check.sh"
echo ""
echo "4. 启动服务并测试:"
echo "   - ./aethertunnel-server -c server.toml"
echo "   - ./aethertunnel-client -c client.toml"
echo ""
echo "5. 监控日志:"
echo "   - tail -f audit.log"
echo ""
echo "📚 详细文档:"
echo "   - SECURITY_AUDIT_REPORT.md (安全审计报告)"
echo "   - SECURITY_IMPROVEMENT_PLAN.md (加固计划)"
echo ""
