package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// SimpleConfig 简化的配置结构
type SimpleConfig struct {
	Server struct {
		BindAddr  string `toml:"bind_addr"`
		BindPort  int    `toml:"bind_port"`
		AuthToken string `toml:"auth_token"`
	} `toml:"server"`
	Dashboard struct {
		Enabled bool `toml:"enabled"`
		Port    int  `toml:"port"`
	} `toml:"dashboard"`
}

var (
	version  = "v1.0.2"
	buildTime = "2026-02-21T08:14:57Z"
	gitCommit = "eeb217d"
)

func main() {
	// 打印版本信息
	fmt.Printf("AetherTunnel Server v%s\n", version)
	fmt.Printf("Build Time: %s\n", buildTime)
	fmt.Printf("Git Commit: %s\n", gitCommit)

	// 创建简化的配置
	cfg := &SimpleConfig{
		Server: struct {
			BindAddr  string `toml:"bind_addr"`
			BindPort  int    `toml:"bind_port"`
			AuthToken string `toml:"auth_token"`
		}{
			BindAddr:  "0.0.0.0",
			BindPort:  7001,
			AuthToken: "your-auth-token-here",
		},
		Dashboard: struct {
			Enabled bool `toml:"enabled"`
			Port    int  `toml:"port"`
		}{
			Enabled: true,
			Port:    7500,
		},
	}

	fmt.Printf("Server started on %s:%d\n", cfg.Server.BindAddr, cfg.Server.BindPort)
	fmt.Printf("Auth Token: %s\n", maskToken(cfg.Server.AuthToken))

	// 创建监听器
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Server.BindAddr, cfg.Server.BindPort))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	// 优雅关闭处理
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-done
		log.Println("\nShutting down server...")
		listener.Close()
	}()

	// 主循环
	connections := 0
	for {
		select {
		case <-done:
			log.Printf("Server shutdown complete. Connections: %d", connections)
			return
		default:
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("Accept error: %v", err)
				time.Sleep(time.Second)
				continue
			}

			connections++
			log.Printf("New connection from %s (total: %d)", conn.RemoteAddr(), connections)

			go func() {
				handleConnection(conn)
			}()
		}
	}
}

// handleConnection 处理连接
func handleConnection(conn net.Conn) {
	defer conn.Close()

	log.Printf("Handling connection from: %s", conn.RemoteAddr())

	// 简单的握手协议
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		log.Printf("Read error: %v", err)
		return
	}

	message := string(buf[:n])
	log.Printf("Received message: %s", message)

	// 发送响应
	response := "Hello from AetherTunnel Server v1.0.2"
	conn.Write([]byte(response))

	log.Printf("Connection from %s closed", conn.RemoteAddr())
}

// maskToken 隐藏认证令牌的一部分
func maskToken(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:4] + "****" + token[len(token)-4:]
}