package util

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/crypto"
)

var (
	ErrInvalidToken      = fmt.Errorf("invalid token")
	ErrInvalidSignature  = fmt.Errorf("invalid signature")
	ErrInvalidTimestamp  = fmt.Errorf("invalid timestamp")
	ErrSignatureExpired  = fmt.Errorf("signature expired")
	ErrClientBlocked     = fmt.Errorf("client blocked")
	ErrConnectionLimit   = fmt.Errorf("connection limit exceeded")
)

// AuthConfig 认证配置
type AuthConfig struct {
	Token          string
	EnableTLS      bool
	EnableSignature bool
	SignatureGrace time.Duration // 签名允许的时间差
	MaxConnections int
}

// LoginAuth 登录认证器
type LoginAuth struct {
	config      *AuthConfig
	tokenHash   []byte
	verifier    *crypto.Ed25519Verifier
	hmacSigner  *crypto.HMACSigner
}

// NewLoginAuth 创建登录认证器
func NewLoginAuth(config *AuthConfig) (*LoginAuth, error) {
	auth := &LoginAuth{
		config:     config,
		tokenHash:  crypto.HashString(config.Token),
		hmacSigner: crypto.NewHMACSigner([]byte(config.Token)),
	}

	if config.EnableSignature {
		// 默认验证器，实际使用时应该从配置加载公钥
		// 这里先使用 HMAC 作为备选方案
	}

	return auth, nil
}

// SetPublicKey 设置用于验证的公钥
func (a *LoginAuth) SetPublicKey(publicKey []byte) error {
	verifier, err := crypto.NewEd25519Verifier(publicKey)
	if err != nil {
		return err
	}
	a.verifier = verifier
	return nil
}

// VerifyToken 验证 token
func (a *LoginAuth) VerifyToken(token string) bool {
	tokenHash := crypto.HashString(token)
	return crypto.ConstantTimeCompare(a.tokenHash, tokenHash)
}

// VerifySignature 验证签名
func (a *LoginAuth) VerifySignature(timestamp int64, signature []byte) bool {
	if a.verifier != nil {
		return a.verifier.VerifyTimestampWithGrace(
			timestamp,
			time.Now().Unix(),
			a.config.SignatureGrace,
			signature,
		)
	}

	// 如果没有设置公钥验证器，使用 HMAC
	return a.hmacSigner.VerifyTimestamp(timestamp, signature)
}

// VerifyLogin 验证登录信息
func (a *LoginAuth) VerifyLogin(token string, timestamp int64, signature []byte) error {
	// 验证 token
	if !a.VerifyToken(token) {
		return ErrInvalidToken
	}

	// 检查时间戳
	now := time.Now().Unix()
	if timestamp > now+a.config.SignatureGrace.Seconds() ||
		timestamp < now-a.config.SignatureGrace.Seconds() {
		return ErrInvalidTimestamp
	}

	// 验证签名
	if a.config.EnableSignature && len(signature) > 0 {
		if !a.VerifySignature(timestamp, signature) {
			return ErrInvalidSignature
		}
	}

	return nil
}

// ConnectionLimiter 连接限制器
type ConnectionLimiter struct {
	maxConnections int
	currentCount   map[string]int // 按客户端 ID 计数
}

// NewConnectionLimiter 创建连接限制器
func NewConnectionLimiter(maxConnections int) *ConnectionLimiter {
	return &ConnectionLimiter{
		maxConnections: maxConnections,
		currentCount:   make(map[string]int),
	}
}

// Increment 增加连接计数
func (l *ConnectionLimiter) Increment(clientID string) error {
	l.currentCount[clientID]++

	if l.maxConnections > 0 && l.currentCount[clientID] > l.maxConnections {
		l.currentCount[clientID]--
		return ErrConnectionLimit
	}

	return nil
}

// Decrement 减少连接计数
func (l *ConnectionLimiter) Decrement(clientID string) {
	if l.currentCount[clientID] > 0 {
		l.currentCount[clientID]--
	}
}

// GetCount 获取当前连接数
func (l *ConnectionLimiter) GetCount(clientID string) int {
	return l.currentCount[clientID]
}

// IPBlocker IP 封禁器
type IPBlocker struct {
	blockedIPs map[string]time.Time // IP -> 解封时间
}

// NewIPBlocker 创建 IP 封禁器
func NewIPBlocker() *IPBlocker {
	return &IPBlocker{
		blockedIPs: make(map[string]time.Time),
	}
}

// Block 封禁 IP
func (b *IPBlocker) Block(ip string, duration time.Duration) {
	b.blockedIPs[ip] = time.Now().Add(duration)
}

// Unblock 解封 IP
func (b *IPBlocker) Unblock(ip string) {
	delete(b.blockedIPs, ip)
}

// IsBlocked 检查 IP 是否被封禁
func (b *IPBlocker) IsBlocked(ip string) bool {
	unblockTime, ok := b.blockedIPs[ip]
	if !ok {
		return false
	}

	if time.Now().After(unblockTime) {
		// 自动解封
		delete(b.blockedIPs, ip)
		return false
	}

	return true
}

// CleanExpired 清理已过期的封禁
func (b *IPBlocker) CleanExpired() {
	now := time.Now()
	for ip, unblockTime := range b.blockedIPs {
		if now.After(unblockTime) {
			delete(b.blockedIPs, ip)
		}
	}
}

// AuditLogger 审计日志记录器
type AuditLogger struct {
	enabled bool
	logger  func(event AuditEvent)
}

// AuditEvent 审计事件
type AuditEvent struct {
	Timestamp   time.Time   `json:"timestamp"`
	EventType   string      `json:"event_type"`   // login, logout, proxy_create, proxy_close, connection, error
	ClientID    string      `json:"client_id"`
	RunID       string      `json:"run_id"`
	IP          string      `json:"ip"`
	User        string      `json:"user"`
	ProxyName   string      `json:"proxy_name,omitempty"`
	Details     interface{} `json:"details,omitempty"`
}

// NewAuditLogger 创建审计日志记录器
func NewAuditLogger(enabled bool, logger func(AuditEvent)) *AuditLogger {
	return &AuditLogger{
		enabled: enabled,
		logger:  logger,
	}
}

// Log 记录审计事件
func (a *AuditLogger) Log(event AuditEvent) {
	if !a.enabled {
		return
	}

	event.Timestamp = time.Now()
	if a.logger != nil {
		a.logger(event)
	}
}

// LogLogin 记录登录事件
func (a *AuditLogger) LogLogin(clientID, runID, ip, user string, success bool, errorMsg string) {
	a.Log(AuditEvent{
		EventType: "login",
		ClientID:  clientID,
		RunID:     runID,
		IP:        ip,
		User:      user,
		Details: map[string]interface{}{
			"success": success,
			"error":   errorMsg,
		},
	})
}

// LogProxyCreate 记录代理创建事件
func (a *AuditLogger) LogProxyCreate(clientID, runID, proxyName, proxyType string, remoteAddr string) {
	a.Log(AuditEvent{
		EventType: "proxy_create",
		ClientID:  clientID,
		RunID:     runID,
		ProxyName: proxyName,
		Details: map[string]interface{}{
			"proxy_type":  proxyType,
			"remote_addr": remoteAddr,
		},
	})
}

// LogConnection 记录连接事件
func (a *AuditLogger) LogConnection(clientID, runID, proxyName, srcAddr string) {
	a.Log(AuditEvent{
		EventType: "connection",
		ClientID:  clientID,
		RunID:     runID,
		ProxyName: proxyName,
		Details: map[string]interface{}{
			"src_addr": srcAddr,
		},
	})
}

// SessionManager 会话管理器
type SessionManager struct {
	sessions map[string]*Session
}

// Session 会话信息
type Session struct {
	ClientID     string
	RunID        string
	User         string
	IP           string
	LoginTime    time.Time
	LastActive   time.Time
	PublicKey    []byte
	Metadata     map[string]string
}

// NewSessionManager 创建会话管理器
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
	}
}

// AddSession 添加会话
func (m *SessionManager) AddSession(runID string, session *Session) {
	session.LoginTime = time.Now()
	session.LastActive = time.Now()
	if session.Metadata == nil {
		session.Metadata = make(map[string]string)
	}
	m.sessions[runID] = session
}

// GetSession 获取会话
func (m *SessionManager) GetSession(runID string) (*Session, bool) {
	session, ok := m.sessions[runID]
	return session, ok
}

// RemoveSession 移除会话
func (m *SessionManager) RemoveSession(runID string) {
	delete(m.sessions, runID)
}

// UpdateActiveTime 更新活动时间
func (m *SessionManager) UpdateActiveTime(runID string) {
	if session, ok := m.sessions[runID]; ok {
		session.LastActive = time.Now()
	}
}

// GetAllSessions 获取所有会话
func (m *SessionManager) GetAllSessions() []*Session {
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// GenerateSessionID 生成会话 ID
func GenerateSessionID() string {
	data := make([]byte, 32)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])[:16]
}

// SignData 签名数据（用于响应）
func SignData(data interface{}, key ed25519.PrivateKey) ([]byte, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal data: %w", err)
	}

	signer, err := crypto.NewEd25519Signer(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	return signer.Sign(jsonData), nil
}

// VerifyData 验证数据签名
func VerifyData(data interface{}, signature []byte, publicKey []byte) (bool, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return false, fmt.Errorf("failed to marshal data: %w", err)
	}

	verifier, err := crypto.NewEd25519Verifier(publicKey)
	if err != nil {
		return false, fmt.Errorf("failed to create verifier: %w", err)
	}

	return verifier.Verify(jsonData, signature), nil
}
