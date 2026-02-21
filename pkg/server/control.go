package server

import (
    "encoding/json"
    "fmt"
    "log"
    "net"
    "os"
    "sync"
    "time"

    "github.com/aethertunnel/aethertunnel/pkg/config"
    "github.com/aethertunnel/aethertunnel/pkg/crypto"
    "github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// ControlConnection 控制连接
type ControlConnection struct {
    conn           net.Conn
    remoteAddr     string
    authenticated  bool
    mu              sync.RWMutex
}

func NewControlConnection(conn net.Conn) *ControlConnection {
    return &ControlConnection{
        conn:          conn,
        remoteAddr:     conn.RemoteAddr().String(),
        authenticated: false,
    }
}

// ControlManager 控制管理器
type ControlManager struct {
    connections map[string]*ControlConnection
    config       *config.Config
    encryption   *crypto.Encryption
    mu           sync.RWMutex
}

func NewControlManager(cfg *config.Config, encryption *crypto.Encryption) *ControlManager {
    return &ControlManager{
        connections: make(map[string]*ControlConnection),
        config:       cfg,
        encryption:   encryption,
    }
}

// HandleConnection 处理新的控制连接
func (cm *ControlManager) HandleConnection(conn net.Conn) {
    connID := conn.RemoteAddr().String()

    cm.mu.Lock()
    defer cm.mu.Unlock()

    // 超过最大连接数
    maxConnections := 100
    if len(cm.connections) >= maxConnections {
        log.Printf("Too many connections, rejecting: %s", connID)
        errMsg := protocol.NewErrorMessage("too many connections")
        if err := protocol.WriteMessage(conn, errMsg); err != nil {
            log.Printf("Failed to write error message: %v", err)
        }
        conn.Close()
        return
    }

    // 添加到连接池
    connObj := NewControlConnection(conn)
    cm.connections[connID] = connObj

    log.Printf("New control connection: %s (total: %d)", connID, len(cm.connections))

    // 启动连接处理
    go cm.handleConnection(connObj)
}

// handleConnection 处理连接
func (cm *ControlManager) handleConnection(conn *ControlConnection) {
    defer conn.conn.Close()

    // 读取第一个消息（应该是认证）
    msg, err := protocol.ReadMessage(conn.conn)
    if err != nil {
        log.Printf("Failed to read message: %v", err)
        return
    }

    log.Printf("Received message type: %d from %s", msg.Type, conn.remoteAddr)

    switch msg.Type {
    case protocol.MessageTypeAuth:
        // 处理认证
        cm.handleAuth(conn, msg.Payload)

    case protocol.MessageTypeHeartbeat:
        // 心跳消息
        cm.handleHeartbeat(conn)

    case protocol.MessageTypeProxy, protocol.MessageTypeData:
        // 数据消息（不应该出现在控制连接中）
        log.Printf("Unexpected data message on control connection: %s", conn.remoteAddr)
        errMsg := protocol.NewErrorMessage("unexpected message type")
        if err := protocol.WriteMessage(conn.conn, errMsg); err != nil {
            log.Printf("Failed to write error message: %v", err)
        }

    default:
        log.Printf("Unknown message type: %d", msg.Type)
    }
}

// handleAuth 处理认证
func (cm *ControlManager) handleAuth(conn *ControlConnection, payload []byte) {
    token := string(payload)

    log.Printf("Auth attempt from %s: %s", conn.remoteAddr, maskToken(token))

    // 验证令牌
    if token != cm.config.Server.AuthToken {
        log.Printf("Invalid auth token: %s", token)
        
        // 发送认证失败消息
        errMsg := protocol.NewErrorMessage("invalid auth token")
        if err := protocol.WriteMessage(conn.conn, errMsg); err != nil {
            log.Printf("Failed to write error message: %v", err)
        }
        return
    }

    // 标记为已认证
    conn.authenticated = true
    log.Printf("Client %s authenticated successfully", conn.remoteAddr)

    // 发送认证成功消息
    successMsg := protocol.NewAuthMessage("OK")
    if err := protocol.WriteMessage(conn.conn, successMsg); err != nil {
        log.Printf("Failed to write auth success message: %v", err)
        return
    }
}

// handleHeartbeat 处理心跳
func (cm *ControlManager) handleHeartbeat(conn *ControlConnection) {
    if !conn.authenticated {
        log.Printf("Heartbeat from unauthenticated client: %s", conn.remoteAddr)
        return
    }

    log.Printf("Heartbeat from %s", conn.remoteAddr)
}

// GetConnection 获取连接
func (cm *ControlManager) GetConnection(id string) (*ControlConnection, bool) {
    cm.mu.RLock()
    defer cm.mu.RUnlock()

    conn, exists := cm.connections[id]
    return conn, exists
}

// RemoveConnection 移除连接
func (cm *ControlManager) RemoveConnection(id string) error {
    cm.mu.Lock()
    defer cm.mu.Unlock()

    conn, exists := cm.connections[id]
    if !exists {
        return fmt.Errorf("connection %s not found", id)
    }

    // 关闭连接
    conn.conn.Close()

    // 删除
    delete(cm.connections, id)

    log.Printf("Removed connection: %s (remaining: %d)", id, len(cm.connections))

    return nil
}

// Broadcast 向所有客户端广播消息
func (cm *ControlManager) Broadcast(msg *protocol.Message) error {
    cm.mu.RLock()
    defer cm.mu.RUnlock()

    var errs []error

    for id, conn := range cm.connections {
        if !conn.authenticated {
            continue
        }

        if err := protocol.WriteMessage(conn.conn, msg); err != nil {
            log.Printf("Failed to broadcast to %s: %v", id, err)
            errs = append(errs, err)
        }
    }

    if len(errs) > 0 {
        return fmt.Errorf("failed to broadcast to %d/%d clients", len(errs), len(cm.connections))
    }

    return nil
}

// GetStats 获取连接统计
func (cm *ControlManager) GetStats() map[string]interface{} {
    cm.mu.RLock()
    defer cm.mu.RUnlock()

    total := len(cm.connections)
    authenticated := 0

    for _, conn := range cm.connections {
        if conn.authenticated {
            authenticated++
        }
    }

    return map[string]interface{}{
        "total":         total,
        "authenticated": authenticated,
        "unauthenticated": total - authenticated,
    }
}

// ListConnections 列出所有连接
func (cm *ControlManager) ListConnections() []map[string]interface{} {
    cm.mu.RLock()
    defer cm.mu.RUnlock()

    list := make([]map[string]interface{}, 0, len(cm.connections))

    for id, conn := range cm.connections {
        list = append(list, map[string]interface{}{
            "id":            id,
            "remote_addr":   conn.remoteAddr,
            "authenticated": conn.authenticated,
        })
    }

    return list
}

// GetServerSimpleExample 获取服务端简单配置示例
func GetServerSimpleExample() string {
    return `[server]
# 绑定地址（0.0.0.0 表示监听所有接口）
bind_addr = "0.0.0.0"

# 绑定端口
bind_port = 7001

# 认证令牌（客户端连接时使用）
auth_token = "your-auth-token-here"

# Web 面板配置
[dashboard]
enabled = true
port = 7500
`
}
