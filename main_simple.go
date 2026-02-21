package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/server"
)

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

	// 加载配置
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <config-file>\n", os.Args[0])
		fmt.Println("\nConfig file example:")
		fmt.Println(server.GetServerSimpleExample())
		os.Exit(1)
	}

	configFile := os.Args[1]
	cfg, err := config.LoadServer(configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// 创建加密器
	encryption := crypto.NewEncryption(cfg.Server.AuthToken)

	// 创建代理管理器
	proxyManager := server.NewProxyManager(cfg, encryption)

	// 创建监听器
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Server.BindAddr, cfg.Server.BindPort))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	log.Printf("Server started on %s:%d", cfg.Server.BindAddr, cfg.Server.BindPort)
	log.Printf("Auth Token: %s", maskToken(cfg.Server.AuthToken))

	// 启动 Web 面板（如果启用）
	if cfg.Dashboard.Enabled {
		go func() {
			if err := server.StartDashboard(cfg.Dashboard.Port, cfg); err != nil {
				log.Printf("Failed to start dashboard: %v", err)
			}
		}()
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
				proxyManager.HandleConnection(conn)
			}()
		}
	}
}

// maskToken 隐藏认证令牌的一部分
func maskToken(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:4] + "****" + token[len(token)-4:]
}