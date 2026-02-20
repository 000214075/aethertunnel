package config

import (
    "fmt"
    "os"
    
    "github.com/BurntSushi/toml"
)

// ServerConfig 服务端配置
type ServerConfig struct {
    BindAddr    string `toml:"bind_addr"`
    BindPort    int    `toml:"bind_port"`
    AuthToken   string `toml:"auth_token"`
    EnableTLS   bool   `toml:"enable_tls"`
    CertFile    string `toml:"cert_file"`
    KeyFile     string `toml:"key_file"`
}

// ClientConfig 客户端配置
type ClientConfig struct {
    ServerAddr string `toml:"server_addr"`
    AuthToken  string `toml:"auth_token"`
}

// ProxyConfig 代理配置
type ProxyConfig struct {
    Name      string `toml:"name"`
    Type      string `toml:"type"`
    LocalIP   string `toml:"local_ip"`
    LocalPort int    `toml:"local_port"`
    RemotePort int    `toml:"remote_port"`
}

// DashboardConfig Web 面板配置
type DashboardConfig struct {
    Enabled bool `toml:"enabled"`
    Port     int    `toml:"port"`
}

// Config 配置结构
type Config struct {
    Server    ServerConfig    `toml:"server"`
    Client    ClientConfig    `toml:"client"`
    Dashboard DashboardConfig `toml:"dashboard"`
    Proxies   []ProxyConfig   `toml:"proxies"`
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
