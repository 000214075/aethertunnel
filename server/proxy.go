package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// ProxyManager 代理管理器
type ProxyManager struct {
	ctx     context.Context
	server  *Server
	proxies map[string]*Proxy // name -> Proxy
	mu      sync.RWMutex
}

// Proxy 代理实例
type Proxy struct {
	name       string
	proxyType  string
	listener   net.Listener
	localAddr  string
	remoteAddr string
	control    *Control
	ctx        context.Context
	cancel     context.CancelFunc
}

// NewProxyManager 创建代理管理器
func NewProxyManager(ctx context.Context, server *Server) *ProxyManager {
	return &ProxyManager{
		ctx:     ctx,
		server:  server,
		proxies: make(map[string]*Proxy),
	}
}

// CreateProxy 创建代理
func (pm *ProxyManager) CreateProxy(control *Control, proxyType, name string, localPort, remotePort int) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// 检查是否已存在
	if _, exists := pm.proxies[name]; exists {
		return fmt.Errorf("proxy '%s' already exists", name)
	}

	// 创建代理
	pxy := &Proxy{
		name:      name,
		proxyType: proxyType,
		control:   control,
		ctx:       context.Background(),
	}

	// 启动监听
	switch proxyType {
	case "tcp", "http", "https":
		addr := fmt.Sprintf("%s:%d", pm.server.config.Proxy.BindAddr, remotePort)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to listen on %s: %w", addr, err)
		}
		pxy.listener = ln
		pxy.remoteAddr = addr

	default:
		return fmt.Errorf("unsupported proxy type: %s", proxyType)
	}

	pxy.localAddr = fmt.Sprintf("127.0.0.1:%d", localPort)
	pxy.cancel = func() {}

	// 添加到管理器
	pm.proxies[name] = pxy

	// 添加到控制
	control.AddProxy(name, pxy)

	// 启动代理处理循环
	ctx, cancel := context.WithCancel(pm.ctx)
	pxy.ctx = ctx
	pxy.cancel = cancel

	go pxy.run()

	// 记录审计日志
	pm.server.audit.LogProxyCreate(
		control.clientID,
		control.runID,
		name,
		proxyType,
		pxy.remoteAddr,
	)

	fmt.Printf("Proxy '%s' (type: %s) created, listening on %s, forwarding to %s\n",
		name, proxyType, pxy.remoteAddr, pxy.localAddr)

	return nil
}

// RemoveProxy 移除代理
func (pm *ProxyManager) RemoveProxy(name string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pxy, ok := pm.proxies[name]
	if !ok {
		return fmt.Errorf("proxy '%s' not found", name)
	}

	// 停止代理
	pxy.cancel()
	pxy.listener.Close()

	// 从控制移除
	if pxy.control != nil {
		pxy.control.RemoveProxy(name)
	}

	// 从管理器移除
	delete(pm.proxies, name)

	fmt.Printf("Proxy '%s' removed\n", name)
	return nil
}

// GetProxy 获取代理
func (pm *ProxyManager) GetProxy(name string) (*Proxy, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	pxy, ok := pm.proxies[name]
	return pxy, ok
}

// GetAllProxies 获取所有代理
func (pm *ProxyManager) GetAllProxies() []*Proxy {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	proxies := make([]*Proxy, 0, len(pm.proxies))
	for _, pxy := range pm.proxies {
		proxies = append(proxies, pxy)
	}
	return proxies
}

// Close 关闭所有代理
func (pm *ProxyManager) Close() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, pxy := range pm.proxies {
		pxy.cancel()
		pxy.listener.Close()
	}
	pm.proxies = make(map[string]*Proxy)
}

// Proxy 方法

func (p *Proxy) run() {
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
			conn, err := p.listener.Accept()
			if err != nil {
				select {
				case <-p.ctx.Done():
					return
				default:
					fmt.Printf("Proxy '%s' accept error: %v\n", p.name, err)
					continue
				}
			}

			go p.handleConnection(conn)
		}
	}
}

func (p *Proxy) handleConnection(userConn net.Conn) {
	defer userConn.Close()

	srcAddr := userConn.RemoteAddr().String()
	fmt.Printf("Proxy '%s': user connection from %s\n", p.name, srcAddr)

	// 记录审计日志
	p.control.mu.RLock()
	clientID := p.control.clientID
	runID := p.control.runID
	p.control.mu.RUnlock()

	p.control.sessions.UpdateActiveTime(runID)
	p.control.lastPing = time.Now() // 更新心跳时间

	p.server.audit.LogConnection(clientID, runID, p.name, srcAddr)

	// 连接到客户端
	// 在实际实现中，这里应该通过控制连接请求工作连接
	// 这里简化处理，直接连接到本地服务
	localConn, err := net.Dial("tcp", p.localAddr)
	if err != nil {
		fmt.Printf("Proxy '%s': failed to connect to local service: %v\n", p.name, err)
		return
	}
	defer localConn.Close()

	// 双向转发数据
	go func() {
		io.Copy(localConn, userConn)
	}()
	io.Copy(userConn, localConn)

	fmt.Printf("Proxy '%s': connection closed\n", p.name)
}

func (p *Proxy) Close() {
	p.cancel()
	p.listener.Close()
}
