package server

import (
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net"
    "os"
    "sync"
    "time"

    "github.com/aethertunnel/aethertunnel/pkg/config"
    "github.com/aethertunnel/aethertunnel/pkg/crypto"
    "github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// Proxy 代理配置
type Proxy struct {
    Name     string
    Type     string
    LocalIP  string
    LocalPort int
    RemotePort int
    LocalConn net.Listener
    mu       sync.RWMutex
}

// ProxyManager 代理管理器
type ProxyManager struct {
    proxies   map[string]*Proxy
    config    *config.Config
    encryption *crypto.Encryption
    mu        sync.RWMutex
}

// NewProxyManager 创建代理管理器
func NewProxyManager(cfg *config.Config, encryption *crypto.Encryption) *ProxyManager {
    return &ProxyManager{
        proxies:   make(map[string]*Proxy),
        config:    cfg,
        encryption: encryption,
    }
}

// HandleConnection 处理新的连接
func (pm *ProxyManager) HandleConnection(conn net.Conn) {
    // 设置超时
    if err := conn.SetReadDeadline(time.Now().Add(10 * time.Minute)); err != nil {
        log.Printf("Failed to set read deadline: %v", err)
        conn.Close()
        return
    }

    // 读取握手消息
    msg, err := protocol.ReadMessage(conn)
    if err != nil {
        log.Printf("Failed to read handshake message: %v", err)
        conn.Close()
        return
    }

    log.Printf("Received message type: %d", msg.Type)

    switch msg.Type {
    case protocol.MessageTypeAuth:
        // 处理认证
        pm.handleAuth(conn, msg.Payload)

    case protocol.MessageTypeHeartbeat:
        // 心跳消息（保持连接）
        log.Printf("Received heartbeat from %s", conn.RemoteAddr())

    case protocol.MessageTypeProxy, protocol.MessageTypeData:
        // 处理代理数据
        pm.handleProxy(conn, msg.Payload)

    default:
        log.Printf("Unknown message type: %d", msg.Type)
        conn.Close()
    }
}

// handleAuth 处理认证
func (pm *ProxyManager) handleAuth(conn net.Conn, payload []byte) {
    token := string(payload)

    pm.mu.RLock()
    defer pm.mu.RUnlock()

    if token != pm.config.Server.AuthToken {
        log.Printf("Invalid auth token: %s", token)
        
        // 发送错误消息
        errMsg := protocol.NewErrorMessage("invalid auth token")
        if err := protocol.WriteMessage(conn, errMsg); err != nil {
            log.Printf("Failed to write error message: %v", err)
        }
        
        conn.Close()
        return
    }

    log.Printf("Client authenticated successfully")

    // 发送认证成功消息
    successMsg := protocol.NewAuthMessage("OK")
    if err := protocol.WriteMessage(conn, successMsg); err != nil {
        log.Printf("Failed to write auth success message: %v", err)
        return
    }
}

// handleProxy 处理代理数据
func (pm *ProxyManager) handleProxy(conn net.Conn, payload []byte) {
    // 尝试解析为 JSON
    var proxyConfig struct {
        Name     string `json:"name"`
        Type     string `json:"type"`
        LocalIP  string `json:"local_ip"`
        LocalPort int    `json:"local_port"`
        RemotePort int    `json:"remote_port"`
    }

    if err := json.Unmarshal(payload, &proxyConfig); err != nil {
        // 不是 JSON 格式，可能是原始数据
        log.Printf("Non-JSON proxy payload: %s", string(payload))
        
        // 发送数据到所有代理
        pm.mu.RLock()
        for _, proxy := range pm.proxies {
            go pm.forwardData(conn, proxy)
        }
        pm.mu.RUnlock()
        
        return
    }

    // 代理名称匹配
    pm.mu.RLock()
    proxy, exists := pm.proxies[proxyConfig.Name]
    pm.mu.RUnlock()

    if !exists {
        log.Printf("Proxy not found: %s", proxyConfig.Name)
        
        // 发送错误消息
        errMsg := protocol.NewErrorMessage("proxy not found")
        if err := protocol.WriteMessage(conn, errMsg); err != nil {
            log.Printf("Failed to write error message: %v", err)
        }
        return
    }

    log.Printf("Forwarding to proxy: %s (%s:%d)", proxy.Name, proxy.LocalIP, proxy.LocalPort)

    // 转发数据
    go pm.forwardData(conn, proxy)
}

// forwardData 转发数据到本地服务
func (pm *ProxyManager) forwardData(conn net.Conn, proxy *Proxy) {
    // 等待本地连接
    localConn, err := net.DialTimeout("tcp", 
        fmt.Sprintf("%s:%d", proxy.LocalIP, proxy.LocalPort), 
        5*time.Second)
    if err != nil {
        log.Printf("Failed to dial local service %s:%d: %v", proxy.LocalIP, proxy.LocalPort, err)
        return
    }
    defer localConn.Close()

    // 转发数据
    buffer := make([]byte, 4096)
    
    for {
        n, err := conn.Read(buffer)
        if err != nil {
            log.Printf("Read error from client connection: %v", err)
            break
        }
        
        if n == 0 {
            break
        }

        _, err = localConn.Write(buffer[:n])
        if err != nil {
            log.Printf("Write error to local service: %v", err)
            break
        }
    }

    log.Printf("Proxy %s connection closed", proxy.Name)
}

// GetProxy 获取代理
func (pm *ProxyManager) GetProxy(name string) (*Proxy, bool) {
    pm.mu.RLock()
    defer pm.mu.RUnlock()

    proxy, exists := pm.proxies[name]
    return proxy, exists
}

// AddProxy 添加代理
func (pm *ProxyManager) AddProxy(name, proxyType, localIP string, localPort, remotePort int) (*Proxy, error) {
    pm.mu.Lock()
    defer pm.mu.Unlock()

    if _, exists := pm.proxies[name]; exists {
        return nil, fmt.Errorf("proxy %s already exists", name)
    }

    proxy := &Proxy{
        Name:      name,
        Type:      proxyType,
        LocalIP:   localIP,
        LocalPort:  localPort,
        RemotePort: remotePort,
    }

    pm.proxies[name] = proxy
    return proxy, nil
}

// RemoveProxy 移除代理
func (pm *ProxyManager) RemoveProxy(name string) error {
    pm.mu.Lock()
    defer pm.mu.Unlock()

    if proxy, exists := pm.proxies[name]; exists {
        // 关闭本地监听器
        if proxy.LocalConn != nil {
            proxy.LocalConn.Close()
        }
        
        delete(pm.proxies, name)
        return nil
    }

    return fmt.Errorf("proxy %s not found", name)
}

// ListProxies 列出所有代理
func (pm *ProxyManager) ListProxies() map[string]*Proxy {
    pm.mu.RLock()
    defer pm.mu.RUnlock()

    // 返回副本
    proxies := make(map[string]*Proxy)
    for k, v := range pm.proxies {
        proxies[k] = v
    }
    
    return proxies
}

// StartProxy 启动代理
func (pm *ProxyManager) StartProxy(proxy *Proxy) error {
    proxy.mu.Lock()
    defer proxy.mu.Unlock()

    // 创建监听器
    listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", proxy.LocalIP, proxy.LocalPort))
    if err != nil {
        return fmt.Errorf("failed to listen on %s:%d: %w", proxy.LocalIP, proxy.LocalPort, err)
    }

    proxy.LocalConn = listener
    log.Printf("Proxy %s started on %s:%d", proxy.Name, proxy.LocalIP, proxy.LocalPort)

    // 启动监听循环
    go func() {
        for {
            conn, err := listener.Accept()
            if err != nil {
                log.Printf("Proxy %s accept error: %v", proxy.Name, err)
                break
            }

            log.Printf("Proxy %s new connection from %s", proxy.Name, conn.RemoteAddr())

            // 处理连接
            go pm.handleProxyConnection(conn, proxy)
        }
    }()

    return nil
}

// StopProxy 停止代理
func (pm *ProxyManager) StopProxy(proxy *Proxy) error {
    proxy.mu.Lock()
    defer proxy.mu.Unlock()

    if proxy.LocalConn == nil {
        return fmt.Errorf("proxy %s is not running", proxy.Name)
    }

    log.Printf("Stopping proxy %s", proxy.Name)
    proxy.LocalConn.Close()
    proxy.LocalConn = nil

    return nil
}

// handleProxyConnection 处理代理的本地连接
func (pm *ProxyManager) handleProxyConnection(conn net.Conn, proxy *Proxy) {
    defer conn.Close()

    log.Printf("Proxy %s new local connection from %s", proxy.Name, conn.RemoteAddr())

    // 转发数据（简化版本，只支持单向转发）
    buffer := make([]byte, 4096)

    for {
        n, err := conn.Read(buffer)
        if err != nil {
            break
        }
        
        if n == 0 {
            break
        }

        // 这里应该转发到实际的服务，但为了简化，我们只是记录
        log.Printf("Proxy %s received %d bytes", proxy.Name, n)
    }

    log.Printf("Proxy %s local connection closed", proxy.Name)
}
