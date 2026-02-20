package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/util"
)

// ControlManager 控制连接管理器
type ControlManager struct {
	ctx         context.Context
	auth        *util.LoginAuth
	limiter     *util.ConnectionLimiter
	blocker     *util.IPBlocker
	audit       *util.AuditLogger
	sessions    *util.SessionManager
	controls    map[string]*Control // runID -> Control
	mu          sync.RWMutex
}

// Control 控制连接
type Control struct {
	runID      string
	clientID   string
	user       string
	conn       net.Conn
	isTLS      bool
	clientIP   string
	publicKey  []byte
	loginTime  time.Time
	lastPing   time.Time
	proxies    map[string]*Proxy
	mu         sync.RWMutex
	ctx        context.Context
	cancel     context.CancelFunc
}

// NewControlManager 创建控制管理器
func NewControlManager(
	ctx context.Context,
	auth *util.LoginAuth,
	limiter *util.ConnectionLimiter,
	blocker *util.IPBlocker,
	audit *util.AuditLogger,
	sessions *util.SessionManager,
) *ControlManager {
	return &ControlManager{
		ctx:       ctx,
		auth:      auth,
		limiter:   limiter,
		blocker:   blocker,
		audit:     audit,
		sessions:  sessions,
		controls:  make(map[string]*Control),
	}
}

// HandleLogin 处理登录
func (m *ControlManager) HandleLogin(conn net.Conn, msg *LoginMessage, isTLS, clientIP string) {
	// 验证登录
	err := m.auth.VerifyLogin(msg.Token, msg.Timestamp, []byte{})
	if err != nil {
		m.audit.LogLogin("", "", clientIP, msg.User, false, err.Error())
		m.sendLoginError(conn, err)
		conn.Close()

		// 封禁失败的尝试
		if m.blocker != nil {
			m.blocker.Block(clientIP, 5*time.Minute)
		}
		return
	}

	// 生成或使用提供的 runID
	runID := msg.RunID
	if runID == "" {
		runID = generateRunID()
	}

	// 检查并替换现有控制
	m.mu.Lock()
	oldCtl, exists := m.controls[runID]
	if exists {
		oldCtl.Close()
		delete(m.controls, runID)
	}
	m.mu.Unlock()

	// 创建新的控制连接
	ctx, cancel := context.WithCancel(m.ctx)
	ctl := &Control{
		runID:     runID,
		clientID:  msg.ClientID,
		user:      msg.User,
		conn:      conn,
		isTLS:     isTLS,
		clientIP:  clientIP,
		publicKey: []byte{},
		loginTime: time.Now(),
		lastPing:  time.Now(),
		proxies:   make(map[string]*Proxy),
		ctx:       ctx,
		cancel:    cancel,
	}

	// 添加到管理器
	m.mu.Lock()
	m.controls[runID] = ctl
	m.mu.Unlock()

	// 添加到会话管理
	m.sessions.AddSession(runID, &util.Session{
		ClientID:  msg.ClientID,
		RunID:     runID,
		User:      msg.User,
		IP:        clientIP,
		PublicKey: []byte{},
	})

	// 记录审计日志
	m.audit.LogLogin(msg.ClientID, runID, clientIP, msg.User, true, "")

	// 发送登录成功响应
	m.sendLoginSuccess(conn, runID)

	// 启动控制循环
	go ctl.run()

	fmt.Printf("Client [%s] logged in from %s, runID: %s\n", msg.User, clientIP, runID)
}

func (m *ControlManager) sendLoginSuccess(conn net.Conn, runID string) {
	resp := fmt.Sprintf("LOGIN_OK:%s:%d\n", runID, time.Now().Unix())
	conn.Write([]byte(resp))
}

func (m *ControlManager) sendLoginError(conn net.Conn, err error) {
	resp := fmt.Sprintf("LOGIN_ERROR:%s\n", err.Error())
	conn.Write([]byte(resp))
}

// GetControl 获取控制连接
func (m *ControlManager) GetControl(runID string) (*Control, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ctl, ok := m.controls[runID]
	return ctl, ok
}

// RemoveControl 移除控制连接
func (m *ControlManager) RemoveControl(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ctl, ok := m.controls[runID]; ok {
		delete(m.controls, runID)
		m.sessions.RemoveSession(runID)
		m.limiter.Decrement(ctl.clientID)
	}
}

// Close 关闭所有控制连接
func (m *ControlManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ctl := range m.controls {
		ctl.Close()
	}
	m.controls = make(map[string]*Control)
}

// Control 方法

func (c *Control) run() {
	// 启动心跳检测
	go c.heartbeatChecker()

	// 消息处理循环
	buf := make([]byte, 4096)
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			n, err := c.conn.Read(buf)
			if err != nil {
				fmt.Printf("Control connection error for %s: %v\n", c.runID, err)
				c.Close()
				return
			}

			c.handleMessage(buf[:n])
		}
	}
}

func (c *Control) handleMessage(data []byte) {
	// 简化的消息处理
	// 实际实现应该解析协议消息

	// 检查心跳
	if len(data) > 4 && string(data[:4]) == "PING" {
		c.lastPing = time.Now()
		c.conn.Write([]byte("PONG\n"))
		return
	}

	// 处理代理创建等消息
	fmt.Printf("Received message from %s: %s\n", c.runID, string(data))
}

func (c *Control) heartbeatChecker() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if time.Since(c.lastPing) > 90*time.Second {
				fmt.Printf("Heartbeat timeout for %s\n", c.runID)
				c.Close()
				return
			}
		}
	}
}

func (c *Control) Close() {
	c.cancel()
	c.conn.Close()
}

func (c *Control) GetProxy(name string) (*Proxy, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	pxy, ok := c.proxies[name]
	return pxy, ok
}

func (c *Control) AddProxy(name string, pxy *Proxy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proxies[name] = pxy
}

func (c *Control) RemoveProxy(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.proxies, name)
}

// GenerateRunID 生成 runID
func generateRunID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
