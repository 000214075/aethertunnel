package main

import (
    "encoding/json"
    "fmt"
    "log"
    "net"
    "os"
    "os/signal"
    "sync"
    "syscall"
    "time"

    "github.com/aethertunnel/aethertunnel/pkg/config"
    "github.com/aethertunnel/aethertunnel/pkg/crypto"
    "github.com/aethertunnel/aethertunnel/pkg/protocol"
)

var (
    version  = "0.1.1-alpha"
    buildTime = "2026-02-21"
    gitCommit = "latest"
)

func main() {
    fmt.Printf("AetherTunnel Client v%s\n", version)
    fmt.Printf("Build Time: %s\n", buildTime)
    fmt.Printf("Git Commit: %s\n", gitCommit)

    if len(os.Args) < 2 {
        fmt.Printf("Usage: %s <config-file>\n", os.Args[0])
        fmt.Println("\nConfig file example:")
        fmt.Println(client_simple_example)
        os.Exit(1)
    }

    configFile := os.Args[1]
    cfg, err := config.LoadClient(configFile)
    if err != nil {
        log.Fatalf("Failed to load config: %v", err)
    }

    // 创建加密器
    encryption := crypto.NewEncryption(cfg.Client.AuthToken)

    // 连接到服务器
    for {
        conn, err := connectToServer(cfg, encryption)
        if err != nil {
            log.Printf("Failed to connect: %v", err)
            time.Sleep(5 * time.Second)
            continue
        }

        log.Printf("Connected to server: %s", cfg.Client.ServerAddr)

        // 启动心跳
        done := make(chan struct{})
        go startHeartbeat(conn, encryption, done)

        // 处理代理
        handleProxies(conn, cfg, encryption, done)

        // 等待断开
        <-done
        log.Println("Connection lost, reconnecting...")
    }
}

func connectToServer(cfg *config.Config, encryption *crypto.Encryption) (net.Conn, error) {
    conn, err := net.DialTimeout("tcp", cfg.Client.ServerAddr, 10*time.Second)
    if err != nil {
        return nil, err
    }

    // 发送认证消息
    authMsg := protocol.NewAuthMessage(cfg.Client.AuthToken)
    authMsg.Type = protocol.MessageTypeAuth
    authMsg.Payload = []byte(cfg.Client.AuthToken)

    if err := protocol.WriteMessage(conn, authMsg); err != nil {
        conn.Close()
        return nil, err
    }

    // 读取认证响应
    msg, err := protocol.ReadMessage(conn)
    if err != nil {
        conn.Close()
        return nil, err
    }

    if msg.Type != protocol.MessageTypeAuth {
        conn.Close()
        return nil, fmt.Errorf("unexpected message type: %d", msg.Type)
    }

    if string(msg.Payload) != "OK" {
        conn.Close()
        return nil, fmt.Errorf("authentication failed: %s", string(msg.Payload))
    }

    return conn, nil
}

func startHeartbeat(conn net.Conn, encryption *crypto.Encryption, done chan struct{}) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-done:
            return
        case <-ticker.C:
            heartbeat := protocol.NewHeartbeatMessage()
            if err := protocol.WriteMessage(conn, heartbeat); err != nil {
                log.Printf("Failed to send heartbeat: %v", err)
                return
            }
        }
    }
}

func handleProxies(conn net.Conn, cfg *config.Config, encryption *crypto.Encryption, done chan struct{}) {
    for _, proxy := range cfg.Proxies {
        log.Printf("Starting proxy: %s (%s:%d -> :%d)", 
            proxy.Name, proxy.LocalIP, proxy.LocalPort, proxy.RemotePort)
        
        go func(p *config.ProxyConfig) {
            localConn, err := net.Listen("tcp", 
                fmt.Sprintf("%s:%d", p.LocalIP, p.LocalPort))
            if err != nil {
                log.Printf("Failed to listen: %v", err)
                return
            }

            defer localConn.Close()
            log.Printf("Proxy %s started", p.Name)

            // 转发数据
            forwardData(conn, localConn)
        }(proxy)
    }
}

func forwardData(clientConn, localConn net.Conn) {
    buffer := make([]byte, 4096)

    for {
        n, err := clientConn.Read(buffer)
        if err != nil {
            break
        }

        if _, err := localConn.Write(buffer[:n]); err != nil {
            break
        }
    }
}

// client_simple_example 客户端简单配置示例
const client_simple_example = `[client]
# 服务器地址
server_addr = "127.0.0.1:7001"

# 认证令牌
auth_token = "your-auth-token-here"

# 代理配置
[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 2222

[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = 8080
remote_port = 8081

[[proxies]]
name = "database"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 3306
remote_port = 3307

[[proxies]]
name = "dns"
type = "udp"
local_ip = "127.0.0.1"
local_port = 53
remote_port = 53
`
