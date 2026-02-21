package server

import (
    "fmt"
    "log"
    "net/http"
    "github.com/aethertunnel/aethertunnel/pkg/config"
)

func RunServer(cfg *config.ServerConfig) error {
    log.Printf("AetherTunnel Server starting on %s:%d\n", cfg.BindAddr, cfg.BindPort)

    // 创建控制管理器
    cm := NewControlManager(cfg, nil)

    // 创建HTTP服务器
    mux := http.NewServeMux()
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte("AetherTunnel Server"))
    })

    // 启动HTTP服务器
    addr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.BindPort)
    log.Printf("Server listening on %s\n", addr)
    if err := http.ListenAndServe(addr, mux); err != nil {
        return fmt.Errorf("failed to start server: %w", err)
    }

    return nil
}