package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/net"
	"github.com/aethertunnel/aethertunnel/pkg/util"
)

const (
	Version = "1.0.0"
)

type Client struct {
	config    *config.ClientConfig
	signer    *crypto.Ed25519Signer
	control   *Control
	proxies   []*Proxy
	ctx       context.Context
	cancel    context.CancelFunc
}

type Control struct {
	conn     net.Conn
	runID    string
	clientID string
	isTLS    bool
	auth     *util.LoginAuth
	ctx      context.Context
	cancel   context.CancelFunc
}

type Proxy struct {
	name      string
	proxyType string
	localAddr string
	localPort int
	control   *Control
	ctx       context.Context
	cancel    context.CancelFunc
}

func NewClient(cfg *config.ClientConfig) (*Client, error) {
	ctx, cancel := context.WithCancel(context.Background())

	cli := &Client{
		config: cfg,
		ctx:    ctx,
		cancel: cancel,
	}

	// 生成或加载密钥对
	if err := cli.setupKeys(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to setup keys: %w", err)
	}

	// 创建控制连接
	control, err := cli.connect()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	cli.control = control

	// 创建代理
	cli.proxies = make([]*Proxy, 0, len(cfg.Proxies))
	for _, pxyCfg := range cfg.Proxies {
		pxy := &Proxy{
			name:      pxyCfg.Name,
			proxyType: pxyCfg.Type,
			localAddr: pxyCfg.LocalIP,
			localPort: pxyCfg.LocalPort,
			control:   control,
			ctx:       ctx,
			cancel:    cancel,
		}
		cli.proxies = append(cli.proxies, pxy)
	}

	return cli, nil
}

func (c *Client) setupKeys() error {
	// 生成密钥对
	privateKey, publicKey, err := crypto.GenerateEd25519KeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate keys: %w", err)
	}

	signer, err := crypto.NewEd25519Signer(privateKey)
	if err != nil {
		return fmt.Errorf("failed to create signer: %w", err)
	}

	c.signer = signer

	// 保存密钥（实际应该从配置加载）
	keysDir := filepath.Join(".", "keys")
	if err := os.MkdirAll(keysDir, 0755); err != nil {
		return fmt.Errorf("failed to create keys directory: %w", err)
	}

	privateKeyFile := filepath.Join(keysDir, "client_private.key")
	publicKeyFile := filepath.Join(keysDir, "client_public.key")

	if err := os.WriteFile(privateKeyFile, privateKey, 0600); err != nil {
		return fmt.Errorf("failed to save private key: %w", err)
	}

	if err := os.WriteFile(publicKeyFile, publicKey, 0644); err != nil {
		return fmt.Errorf("failed to save public key: %w", err)
	}

	log.Printf("Generated key pair: %s, %s", privateKeyFile, publicKeyFile)
	return nil
}

func (c *Client) connect() (*Control, error) {
	addr := fmt.Sprintf("%s:%d", c.config.Client.ServerAddr, c.config.Client.ServerPort)

	var conn net.Conn
	var err error

	// 创建连接
	if c.config.TLS.Enabled {
		tlsConfig, err := net.CreateTLSConfig(
			c.config.TLS.CertFile,
			c.config.TLS.KeyFile,
			c.config.TLS.CAFile,
			false, // 服务端不需要客户端证书（这里简化）
			true,  // 是客户端
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create TLS config: %w", err)
		}

		conn, err = tls.Dial("tcp", addr, tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("TLS dial failed: %w", err)
		}
	} else {
		conn, err = net.DialTimeout(addr, 10*time.Second)
		if err != nil {
			return nil, fmt.Errorf("dial failed: %w", err)
		}
	}

	ctx, cancel := context.WithCancel(c.ctx)

	// 创建认证器
	authConfig := &util.AuthConfig{
		Token:          c.config.Client.AuthToken,
		EnableTLS:      c.config.TLS.Enabled,
		EnableSignature: true,
		SignatureGrace: 30 * time.Second,
	}
	auth, _ := util.NewLoginAuth(authConfig)

	control := &Control{
		conn:   conn,
		isTLS:  c.config.TLS.Enabled,
		auth:   auth,
		ctx:    ctx,
		cancel: cancel,
	}

	// 发送登录消息
	if err := control.sendLogin(c.signer, c.config.Client); err != nil {
		conn.Close()
		cancel()
		return nil, fmt.Errorf("login failed: %w", err)
	}

	// 启动控制循环
	go control.run()

	return control, nil
}

func (c *Client) Run() error {
	log.Printf("AetherTunnel Client v%s starting...", Version)
	log.Printf("Connected to server: %s:%d", c.config.Client.ServerAddr, c.config.Client.ServerPort)

	// 注册所有代理
	for _, pxy := range c.proxies {
		if err := c.registerProxy(pxy); err != nil {
			log.Printf("Failed to register proxy '%s': %v", pxy.name, err)
			continue
		}
	}

	// 等待退出信号
	c.waitForShutdown()

	return nil
}

func (c *Client) registerProxy(pxy *Proxy) error {
	// 发送代理注册消息
	msg := fmt.Sprintf("NEW_PROXY:%s:%s:%s:%d\n",
		pxy.name, pxy.proxyType, pxy.localAddr, pxy.localPort)

	if _, err := c.control.conn.Write([]byte(msg)); err != nil {
		return fmt.Errorf("failed to send proxy message: %w", err)
	}

	log.Printf("Registered proxy '%s' (type: %s)", pxy.name, pxy.proxyType)
	return nil
}

func (c *Client) waitForShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	log.Printf("Received signal %v, shutting down...", sig)

	c.Shutdown()
}

func (c *Client) Shutdown() {
	log.Println("Shutting down client...")

	c.cancel()

	if c.control != nil {
		c.control.Close()
	}

	for _, pxy := range c.proxies {
		pxy.Close()
	}

	log.Println("Client shutdown complete")
}

// Control methods

func (ctl *Control) sendLogin(signer *crypto.Ed25519Signer, clientCfg config.ClientSection) error {
	timestamp := time.Now().Unix()
	signature := signer.SignTimestamp(timestamp)

	// 生成 clientID
	if clientCfg.ClientID == "" {
		clientCfg.ClientID = util.GenerateSessionID()
	}

	loginMsg := fmt.Sprintf("LOGIN:%s:%s:%s:%s:%s:%d:%x\n",
		signer.GetPublicKey(),
		clientCfg.User,
		clientCfg.ClientID,
		clientCfg.AuthToken,
		timestamp,
		signature,
	)

	if _, err := ctl.conn.Write([]byte(loginMsg)); err != nil {
		return fmt.Errorf("failed to send login: %w", err)
	}

	// 读取响应
	buf := make([]byte, 1024)
	n, err := ctl.conn.Read(buf)
	if err != nil {
		return fmt.Errorf("failed to read login response: %w", err)
	}

	response := string(buf[:n])
	if len(response) >= 8 && response[:8] == "LOGIN_OK" {
		fmt.Sscanf(response, "LOGIN_OK:%s", &ctl.runID)
		ctl.clientID = clientCfg.ClientID
		log.Printf("Login successful, runID: %s", ctl.runID)
		return nil
	}

	return fmt.Errorf("login failed: %s", response)
}

func (ctl *Control) run() {
	// 启动心跳
	go ctl.heartbeatSender()

	// 消息处理循环
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctl.ctx.Done():
			return
		default:
			n, err := ctl.conn.Read(buf)
			if err != nil {
				log.Printf("Control connection error: %v", err)
				ctl.Close()
				return
			}

			ctl.handleMessage(buf[:n])
		}
	}
}

func (ctl *Control) handleMessage(data []byte) {
	// 简化的消息处理
	fmt.Printf("Received from server: %s\n", string(data))

	// 检查心跳响应
	if len(data) >= 4 && string(data[:4]) == "PONG" {
		// 心跳响应处理
		return
	}

	// 处理其他消息...
}

func (ctl *Control) heartbeatSender() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctl.ctx.Done():
			return
		case <-ticker.C:
			timestamp := time.Now().Unix()
			// 使用 HMAC 签名（简化）
			pingMsg := fmt.Sprintf("PING:%d\n", timestamp)
			if _, err := ctl.conn.Write([]byte(pingMsg)); err != nil {
				log.Printf("Failed to send heartbeat: %v", err)
				ctl.Close()
				return
			}
		}
	}
}

func (ctl *Control) Close() {
	ctl.cancel()
	ctl.conn.Close()
}

// Proxy methods

func (p *Proxy) Close() {
	// 代理清理逻辑
}

func main() {
	configPath := "client.toml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	// 加载配置
	cfg, err := config.LoadClientConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// 验证配置
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid config: %v", err)
	}

	// 创建并启动客户端
	cli, err := NewClient(cfg)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	if err := cli.Run(); err != nil {
		log.Fatalf("Client error: %v", err)
	}
}
