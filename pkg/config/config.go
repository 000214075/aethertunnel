package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// ServerConfig 服务端配置
type ServerConfig struct {
	BindAddr                string `toml:"bind_addr"`
	BindPort                int    `toml:"bind_port"`
	AuthToken               string `toml:"auth_token"`
	EnableTLS               bool   `toml:"enable_tls"`
	CertFile                string `toml:"cert_file"`
	KeyFile                 string `toml:"key_file"`
	MaxConnections          int    `toml:"max_connections"`
	GracefulShutdownTimeout int    `toml:"graceful_shutdown_timeout"`
}

// ClientConfig 客户端配置
type ClientConfig struct {
	ServerAddr string `toml:"server_addr"`
	AuthToken  string `toml:"auth_token"`
}

// ProxyConfig 代理配置
type ProxyConfig struct {
	Name       string `toml:"name"`
	Type       string `toml:"type"`
	LocalIP    string `toml:"local_ip"`
	LocalPort  int    `toml:"local_port"`
	RemotePort int    `toml:"remote_port"`
}

// DashboardConfig Web 面板配置
type DashboardConfig struct {
	Enabled bool `toml:"enabled"`
	Port    int  `toml:"port"`
}

// VPNConfig VPN配置
type VPNConfig struct {
	Enabled            bool     `toml:"enabled"`
	BindAddr           string   `toml:"bind_addr"`
	Port               int      `toml:"port"`
	LocalIP            string   `toml:"local_ip"`
	RemoteIP           string   `toml:"remote_ip"`
	Netmask            string   `toml:"netmask"`
	Protocol           string   `toml:"protocol"` // tcp, udp, sctp, websocket, http
	Obfuscation        bool     `toml:"obfuscation"`
	AuthToken          string   `toml:"auth_token"`
	MaxPeers           int      `toml:"max_peers"`
	MTU                int      `toml:"mtu"`
	EnablePerformance  bool     `toml:"enable_performance"`  // 🆕 启用性能优化
	MaxPoolSize        int      `toml:"max_pool_size"`       // 🆕 连接池大小
	EnableCompression  bool     `toml:"enable_compression"`  // 🆕 启用压缩
	EnableQoS          bool     `toml:"enable_qos"`          // 🆕 启用QoS
	BandwidthLimit     string   `toml:"bandwidth_limit"`     // 🆕 带宽限制
	SupportedProtocols []string `toml:"supported_protocols"` // 🆕 支持的协议列表
	EnableHTTPForward  bool     `toml:"enable_http_forward"` // 🆕 启用HTTP转发
	EnableSCTPForward  bool     `toml:"enable_sctp_forward"` // 🆕 启用SCTP转发
	EnableWSForward    bool     `toml:"enable_ws_forward"`   // 🆕 启用WebSocket转发
	ProtocolTimeout    string   `toml:"protocol_timeout"`    // 🆕 协议超时
	ProtocolMaxSize    int      `toml:"protocol_max_size"`   // 🆕 协议最大消息大小
}

// ObfuscationConfig 数据混淆配置
type ObfuscationConfig struct {
	Enabled         bool     `toml:"enabled"`
	DefaultType     string   `toml:"default_type"`
	AllowedTypes    []string `toml:"allowed_types"`
	AdaptiveEnabled bool     `toml:"adaptive_enabled"`
	KeyRotation     int      `toml:"key_rotation"` // 密钥轮换时间（分钟）
	PacketPadding   bool     `toml:"packet_padding"`
	TrafficMorphing bool     `toml:"traffic_morphing"`
}

// Config 配置结构
type Config struct {
	Server      ServerConfig      `toml:"server"`
	Client      ClientConfig      `toml:"client"`
	Dashboard   DashboardConfig   `toml:"dashboard"`
	VPN         VPNConfig         `toml:"vpn"`
	Obfuscation ObfuscationConfig `toml:"obfuscation"`
	Proxies     []ProxyConfig     `toml:"proxies"`
}

// LoadServer 加载服务端配置
func LoadServer(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// 验证必需字段
	if cfg.Server.BindAddr == "" {
		return nil, fmt.Errorf("server.bind_addr is required")
	}
	if cfg.Server.BindPort <= 0 || cfg.Server.BindPort > 65535 {
		return nil, fmt.Errorf("server.bind_port must be between 1 and 65535")
	}
	if cfg.Server.AuthToken == "" {
		return nil, fmt.Errorf("server.auth_token is required")
	}

	return &cfg, nil
}

// LoadClient 加载客户端配置
func LoadClient(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// 验证必需字段
	if cfg.Client.ServerAddr == "" {
		return nil, fmt.Errorf("client.server_addr is required")
	}
	if cfg.Client.AuthToken == "" {
		return nil, fmt.Errorf("client.auth_token is required")
	}

	return &cfg, nil
}
