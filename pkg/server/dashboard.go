package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// StartDashboard 启动 Web 面板
func StartDashboard(port int, cfg *config.Config) error {
	// 创建文件服务器
	fs := http.FileServer(http.Dir("../../web/dashboard"))

	// 创建路由
	mux := http.NewServeMux()
	mux.Handle("/", fs)
	mux.HandleFunc("/index.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeFile(w, r, "web/dashboard/index.html")
	})
	mux.HandleFunc("/server.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeFile(w, r, "web/dashboard/server.html")
	})
	mux.HandleFunc("/client.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeFile(w, r, "web/dashboard/client.html")
	})

	// API 路由
	mux.HandleFunc("/api/status", handleAPIStatus)
	mux.HandleFunc("/api/config", handleAPIConfig)

	// 启动服务器
	addr := fmt.Sprintf(":%d", port)
	log.Printf("Dashboard starting on %s", addr)

	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("Dashboard failed to start: %v", err)
		}
	}()

	return nil
}

// handleAPIStatus 处理 API 状态请求
func handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	status := map[string]interface{}{
		"status":     "running",
		"version":    "0.1.1-alpha",
		"build_time": "2026-02-21",
		"git_commit": "latest",
		"uptime":     "unknown",
	}

	json.NewEncoder(w).Encode(status)
}

// handleAPIConfig 处理 API 配置请求
func handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// 返回示例配置
	config := map[string]interface{}{
		"server": map[string]string{
			"bind_addr":  "0.0.0.0",
			"bind_port":  "7001",
			"auth_token": "your-auth-token-here",
		},
		"dashboard": map[string]interface{}{
			"enabled": true,
			"port":    7500,
		},
	}

	json.NewEncoder(w).Encode(config)
}
