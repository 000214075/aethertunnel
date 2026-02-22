package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// Proxy 代理配置
type Proxy struct {
	Name       string
	Type       string
	LocalIP    string
	LocalPort  int
	RemotePort int
}

// ProxyManager 代理管理器
type ProxyManager struct {
	proxies    map[string]*Proxy
	config     *config.Config
	encryption *crypto.Encryption
	mu         sync.RWMutex
}

// NewProxyManager 创建代理管理器
func NewProxyManager(cfg *config.Config, encryption *crypto.Encryption) *ProxyManager {
	pm := &ProxyManager{
		proxies:    make(map[string]*Proxy),
		config:     cfg,
		encryption: encryption,
	}

	// 加载代理配置
	for _, proxy := range cfg.Proxies {
		pm.proxies[proxy.Name] = &Proxy{
			Name:       proxy.Name,
			Type:       proxy.Type,
			LocalIP:    proxy.LocalIP,
			LocalPort:  proxy.LocalPort,
			RemotePort: proxy.RemotePort,
		}
	}

	return pm
}

// HandleConnection 处理连接
func (pm *ProxyManager) HandleConnection(conn net.Conn) {
	remoteAddr := conn.RemoteAddr().String()
	log.Printf("Handling connection from %s", remoteAddr)

	// 读取第一个消息
	msg, err := protocol.ReadMessage(conn)
	if err != nil {
		log.Printf("Failed to read message from %s: %v", remoteAddr, err)
		conn.Close()
		return
	}

	log.Printf("Received message type: %d from %s", msg.Type, remoteAddr)

	switch msg.Type {
	case protocol.MessageTypeAuth:
		// 处理认证
		pm.handleAuth(conn, msg.Payload)

	case protocol.MessageTypeHeartbeat:
		// 处理心跳
		pm.handleHeartbeat(conn)

	case protocol.MessageTypeProxy:
		// 处理代理请求
		pm.handleProxyRequest(conn, msg.Payload)

	default:
		log.Printf("Unknown message type: %d from %s", msg.Type, remoteAddr)
		conn.Close()
	}
}

// handleAuth 处理认证
func (pm *ProxyManager) handleAuth(conn net.Conn, payload []byte) {
	token := string(payload)

	// 验证令牌
	if token != pm.config.Server.AuthToken {
		log.Printf("Invalid auth token from %s", conn.RemoteAddr())

		// 发送认证失败消息
		errMsg := protocol.NewErrorMessage("invalid auth token")
		if err := protocol.WriteMessage(conn, errMsg); err != nil {
			log.Printf("Failed to write error message: %v", err)
		}
		return
	}

	log.Printf("Client %s authenticated successfully", conn.RemoteAddr())

	// 发送认证成功消息
	successMsg := protocol.NewAuthMessage("OK")
	if err := protocol.WriteMessage(conn, successMsg); err != nil {
		log.Printf("Failed to write auth success message: %v", err)
	}
}

// handleHeartbeat 处理心跳
func (pm *ProxyManager) handleHeartbeat(conn net.Conn) {
	log.Printf("Heartbeat from %s", conn.RemoteAddr())
}

// handleProxyRequest 处理代理请求
func (pm *ProxyManager) handleProxyRequest(conn net.Conn, payload []byte) {
	var proxyReq struct {
		Name string `json:"name"`
	}

	if err := json.Unmarshal(payload, &proxyReq); err != nil {
		log.Printf("Failed to unmarshal proxy request: %v", err)
		return
	}

	proxy, exists := pm.proxies[proxyReq.Name]
	if !exists {
		log.Printf("Proxy %s not found", proxyReq.Name)
		return
	}

	log.Printf("Proxy request for %s (type: %s)", proxy.Name, proxy.Type)

	// 创建到目标服务器的连接
	targetAddr := fmt.Sprintf("127.0.0.1:%d", proxy.RemotePort)
	targetConn, err := net.DialTimeout("tcp", targetAddr, 30*time.Second)
	if err != nil {
		log.Printf("Failed to connect to target %s: %v", targetAddr, err)

		// 发送错误消息
		errMsg := protocol.NewErrorMessage(err.Error())
		if err := protocol.WriteMessage(conn, errMsg); err != nil {
			log.Printf("Failed to write error message: %v", err)
		}
		return
	}

	// 发送成功消息（使用代理消息类型）
	successMsg := protocol.NewProxyMessage([]byte("proxy established"))
	if err := protocol.WriteMessage(conn, successMsg); err != nil {
		log.Printf("Failed to write success message: %v", err)
		targetConn.Close()
		return
	}

	// 开始在两个连接之间复制数据
	go pm.copyData(conn, targetConn)
	go pm.copyData(targetConn, conn)
}

// copyData 在两个连接之间复制数据
func (pm *ProxyManager) copyData(src, dst net.Conn) {
	defer src.Close()
	defer dst.Close()

	// 创建缓冲区
	buf := make([]byte, 8192)

	for {
		// 从源连接读取数据
		n, err := src.Read(buf)
		if err != nil {
			log.Printf("Read error: %v", err)
			return
		}

		if n > 0 {
			// 向目标连接写入数据
			_, err := dst.Write(buf[:n])
			if err != nil {
				log.Printf("Write error: %v", err)
				return
			}
		}
	}
}

// GetProxyConfig 获取代理配置
func (pm *ProxyManager) GetProxyConfig(name string) *Proxy {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	return pm.proxies[name]
}

// AddProxy 添加代理
func (pm *ProxyManager) AddProxy(proxy *Proxy) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.proxies[proxy.Name] = proxy
}

// RemoveProxy 移除代理
func (pm *ProxyManager) RemoveProxy(name string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	delete(pm.proxies, name)
}

// GetProxies 获取所有代理
func (pm *ProxyManager) GetProxies() map[string]*Proxy {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	proxies := make(map[string]*Proxy)
	for name, proxy := range pm.proxies {
		proxies[name] = proxy
	}

	return proxies
}
