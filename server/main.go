package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/util"
)

const (
	Version = "1.0.0"
)

type Server struct {
	config     *config.ServerConfig
	ln         net.Listener
	tlsLn      net.Listener
	controlMgr *ControlManager
	proxyMgr   *ProxyManager
	auth       *util.LoginAuth
	limiter    *util.ConnectionLimiter
	blocker    *util.IPBlocker
	audit      *util.AuditLogger
	sessions   *util.SessionManager
	httpServer *http.Server
	ctx        context.Context
	cancel     context.CancelFunc
}

func NewServer(cfg *config.ServerConfig) (*Server, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// 创建监听器
	addr := fmt.Sprintf("%s:%d", cfg.Server.BindAddr, cfg.Server.BindPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	svr := &Server{
		config:   cfg,
		ln:       ln,
		auth:     createAuth(cfg),
		limiter:  util.NewConnectionLimiter(cfg.Security.MaxConnectionsPerClient),
		blocker:  util.NewIPBlocker(),
		sessions: util.NewSessionManager(),
		ctx:      ctx,
		cancel:   cancel,
	}

	// 创建审计日志
	if cfg.Security.EnableAuditLog {
		svr.audit = util.NewAuditLogger(true, svr.logAuditEvent)
	}

	// 创建 TLS 监听器（如果启用）
	if cfg.TLS.Enabled {
		tlsConfig, err := net.CreateTLSConfig(
			cfg.TLS.CertFile,
			cfg.TLS.KeyFile,
			cfg.TLS.CAFile,
			cfg.TLS.ClientAuth,
			false,
		)
		if err != nil {
			ln.Close()
			cancel()
			return nil, fmt.Errorf("failed to create TLS config: %w", err)
		}

		svr.tlsLn, err = tls.Listen("tcp", addr, tlsConfig)
		if err != nil {
			ln.Close()
			cancel()
			return nil, fmt.Errorf("failed to create TLS listener: %w", err)
		}
	}

	// 创建管理器
	svr.controlMgr = NewControlManager(ctx, svr.auth, svr.limiter, svr.blocker, svr.audit, svr.sessions)
	svr.proxyMgr = NewProxyManager(ctx, svr)

	// 创建仪表板（如果启用）
	if cfg.Dashboard.Enabled {
		svr.httpServer = svr.createDashboardServer()
	}

	return svr, nil
}

func createAuth(cfg *config.ServerConfig) *util.LoginAuth {
	authConfig := &util.AuthConfig{
		Token:          cfg.Server.AuthToken,
		EnableTLS:      cfg.TLS.Enabled,
		EnableSignature: true,
		SignatureGrace: time.Duration(cfg.Security.HeartbeatTimeout) * time.Second,
		MaxConnections: cfg.Security.MaxConnectionsPerClient,
	}

	auth, _ := util.NewLoginAuth(authConfig)
	return auth
}

func (s *Server) createDashboardServer() *http.Server {
	mux := http.NewServeMux()

	// 基础路由
	mux.HandleFunc("/", s.dashboardHandler)
	mux.HandleFunc("/api/health", s.healthHandler)
	mux.HandleFunc("/api/proxies", s.proxiesHandler)
	mux.HandleFunc("/api/clients", s.clientsHandler)

	addr := fmt.Sprintf(":%d", s.config.Dashboard.Port)
	return &http.Server{
		Addr:    addr,
		Handler: mux,
	}
}

func (s *Server) Run() error {
	log.Printf("AetherTunnel Server v%s starting...", Version)
	log.Printf("Listening on %s:%d", s.config.Server.BindAddr, s.config.Server.BindPort)
	if s.config.TLS.Enabled {
		log.Printf("TLS enabled, listening on secure port")
	}
	if s.config.Dashboard.Enabled {
		log.Printf("Dashboard enabled on port %d", s.config.Dashboard.Port)
	}

	// 启动仪表板
	if s.httpServer != nil {
		go func() {
			log.Printf("Dashboard server started")
			if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("Dashboard server error: %v", err)
			}
		}()
	}

	// 启动控制连接接受器
	if s.config.TLS.Enabled && s.tlsLn != nil {
		go s.acceptConnections(s.tlsLn, true)
	}
	go s.acceptConnections(s.ln, false)

	// 启动清理任务
	go s.cleanupTasks()

	// 等待退出信号
	s.waitForShutdown()

	return nil
}

func (s *Server) acceptConnections(ln net.Listener, isTLS bool) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return
			default:
				log.Printf("Accept error: %v", err)
				continue
			}
		}

		remoteAddr := conn.RemoteAddr().String()
		host, _, _ := net.SplitHostPort(remoteAddr)

		// 检查 IP 封禁
		if s.blocker.IsBlocked(host) {
			log.Printf("Connection from blocked IP %s rejected", host)
			conn.Close()
			continue
		}

		// 检查 IP 白名单
		if s.config.Security.EnableIPWhitelist {
			allowed := false
			for _, allowedIP := range s.config.Security.AllowedIPs {
				if host == allowedIP {
					allowed = true
					break
				}
			}
			if !allowed {
				log.Printf("Connection from IP %s not in whitelist", host)
				conn.Close()
				continue
			}
		}

		go s.handleConnection(conn, isTLS, host)
	}
}

func (s *Server) handleConnection(conn net.Conn, isTLS, clientIP string) {
	defer conn.Close()

	// 设置超时
	conn.SetDeadline(time.Now().Add(time.Duration(s.config.Security.ConnectionTimeout) * time.Second))

	// 读取第一个消息
	msg, err := s.readMessage(conn)
	if err != nil {
		log.Printf("Read message error: %v", err)
		return
	}

	// 处理登录消息
	if loginMsg, ok := msg.(*LoginMessage); ok {
		s.controlMgr.HandleLogin(conn, loginMsg, isTLS, clientIP)
	} else {
		log.Printf("Unexpected message type, expected Login")
	}
}

func (s *Server) readMessage(conn net.Conn) (interface{}, error) {
	// 简化的消息读取，实际应该使用 protocol 包
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}

	// 解析消息（这里简化处理）
	loginMsg := &LoginMessage{
		Version:   "1.0.0",
		Hostname:  "unknown",
		OS:        "unknown",
		Arch:      "unknown",
		User:      "guest",
		Token:     string(buf[:n]),
		Timestamp: time.Now().Unix(),
	}

	return loginMsg, nil
}

func (s *Server) cleanupTasks() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.blocker.CleanExpired()
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Server) waitForShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	log.Printf("Received signal %v, shutting down...", sig)

	s.Shutdown()
}

func (s *Server) Shutdown() {
	log.Println("Shutting down server...")

	s.cancel()

	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.httpServer.Shutdown(ctx)
	}

	if s.tlsLn != nil {
		s.tlsLn.Close()
	}
	s.ln.Close()

	s.controlMgr.Close()
	s.proxyMgr.Close()

	log.Println("Server shutdown complete")
}

// Dashboard handlers

func (s *Server) dashboardHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("AetherTunnel Dashboard\n"))
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) proxiesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	proxies := s.proxyMgr.GetAllProxies()
	w.Write([]byte(fmt.Sprintf(`{"count":%d}`, len(proxies))))
}

func (s *Server) clientsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	sessions := s.sessions.GetAllSessions()
	w.Write([]byte(fmt.Sprintf(`{"count":%d}`, len(sessions))))
}

func (s *Server) logAuditEvent(event util.AuditEvent) {
	logPath := s.config.Security.AuditLogFile
	if logPath == "" {
		return
	}

	// 确保日志目录存在
	logDir := filepath.Dir(logPath)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("Failed to create log directory: %v", err)
		return
	}

	// 写入日志（这里简化，实际应该使用更健壮的日志库）
	logEntry := fmt.Sprintf("[%s] %s client=%s user=%s ip=%s\n",
		event.Timestamp.Format(time.RFC3339),
		event.EventType,
		event.ClientID,
		event.User,
		event.IP,
	)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open audit log: %v", err)
		return
	}
	defer f.Close()

	f.WriteString(logEntry)
}

// LoginMessage 登录消息（简化版）
type LoginMessage struct {
	Version   string
	Hostname  string
	OS        string
	Arch      string
	User      string
	Token     string
	Timestamp int64
}

func main() {
	configPath := "server.toml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	// 加载配置
	cfg, err := config.LoadServerConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// 验证配置
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid config: %v", err)
	}

	// 创建并启动服务器
	svr, err := NewServer(cfg)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	if err := svr.Run(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
