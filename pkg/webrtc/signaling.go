package webrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

// SignalingServer WebRTC 信令服务器（WebSocket实现）
type SignalingServer struct {
	addr       string
	server     *http.Server
	upgrader   websocket.Upgrader
	clients    map[string]*SignalingClient
	clientsMu  sync.RWMutex
	sessions   map[string]string
	sessionsMu sync.RWMutex
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// SignalingClient 信令客户端
type SignalingClient struct {
	sessionID   string
	conn        *websocket.Conn
	manager     *PeerConnectionManager
	sendCh      chan []byte
	mu          sync.RWMutex
	connectedAt time.Time
}

// SignalingMessage 信令消息
type SignalingMessage struct {
	Type      string                        `json:"type"`
	SessionID string                        `json:"session_id"`
	TargetID  string                        `json:"target_id,omitempty"`
	Payload   map[string]interface{}        `json:"payload,omitempty"`
	SDP       *webrtc.SessionDescription   `json:"sdp,omitempty"`
	ICE       *webrtc.ICECandidateInit     `json:"ice,omitempty"`
	Timestamp int64                        `json:"timestamp"`
}

// NewSignalingServer 创建信令服务器
func NewSignalingServer(addr string) *SignalingServer {
	ctx, cancel := context.WithCancel(context.Background())

	return &SignalingServer{
		addr: addr,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // 生产环境需要更严格的检查
			},
		},
		clients:    make(map[string]*SignalingClient),
		sessions:   make(map[string]string),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Start 启动信令服务器
func (s *SignalingServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/signal", s.handleWebSocket)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/peers", s.handlePeers)

	s.server = &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	log.Printf("Signaling server listening on %s", s.addr)

	err := s.server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("failed to start signaling server: %w", err)
	}

	return nil
}

// Stop 停止信令服务器
func (s *SignalingServer) Stop() error {
	s.cancel()
	s.wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return s.server.Shutdown(ctx)
}

// handleWebSocket 处理 WebSocket 连接
func (s *SignalingServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection: %v", err)
		return
	}

	// 获取会话 ID
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		sessionID = generateSessionID()
	}

	client := &SignalingClient{
		sessionID:   sessionID,
		conn:        conn,
		sendCh:      make(chan []byte, 100),
		connectedAt: time.Now(),
	}

	// 注册客户端
	s.clientsMu.Lock()
	s.clients[sessionID] = client
	s.clientsMu.Unlock()

	log.Printf("Client connected: %s (total: %d)", sessionID, len(s.clients))

	// 启动发送和接收协程
	s.wg.Add(1)
	go s.sendMessages(client)

	s.wg.Add(1)
	go s.receiveMessages(client)
}

// handleHealth 健康检查
func (s *SignalingServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.clientsMu.RLock()
	peerCount := len(s.clients)
	s.clientsMu.RUnlock()

	response := map[string]interface{}{
		"status":    "healthy",
		"peers":     peerCount,
		"timestamp": time.Now().Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handlePeers 列出所有连接的对等方
func (s *SignalingServer) handlePeers(w http.ResponseWriter, r *http.Request) {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	peers := make([]map[string]interface{}, 0, len(s.clients))
	for _, client := range s.clients {
		peers = append(peers, map[string]interface{}{
			"session_id":    client.sessionID,
			"connected_at":  client.connectedAt.Format(time.RFC3339),
			"uptime":        time.Since(client.connectedAt).String(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"peers": peers,
		"count": len(peers),
	})
}

// sendMessages 发送消息到客户端
func (s *SignalingServer) sendMessages(client *SignalingClient) {
	defer s.wg.Done()

	for {
		select {
		case msg, ok := <-client.sendCh:
			if !ok {
				return
			}

			err := client.conn.WriteMessage(websocket.TextMessage, msg)
			if err != nil {
				log.Printf("Failed to send message to %s: %v", client.sessionID, err)
				return
			}

		case <-s.ctx.Done():
			return
		}
	}
}

// receiveMessages 从客户端接收消息
func (s *SignalingServer) receiveMessages(client *SignalingClient) {
	defer s.wg.Done()
	defer s.unregisterClient(client.sessionID)

	for {
		select {
		case <-s.ctx.Done():
			return

		default:
			messageType, message, err := client.conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Printf("Error reading from %s: %v", client.sessionID, err)
				}
				return
			}

			if messageType == websocket.TextMessage {
				s.handleMessage(client, message)
			}
		}
	}
}

// handleMessage 处理收到的消息
func (s *SignalingServer) handleMessage(client *SignalingClient, message []byte) {
	var msg SignalingMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		log.Printf("Failed to parse message from %s: %v", client.sessionID, err)
		return
	}

	msg.SessionID = client.sessionID
	msg.Timestamp = time.Now().Unix()

	switch msg.Type {
	case "offer", "answer", "ice_candidate":
		// 转发消息到目标客户端
		if msg.TargetID != "" {
			s.forwardMessage(msg.TargetID, msg)
		}

	case "register_peer":
		s.registerPeerSession(msg.SessionID, msg.TargetID)

	case "unregister_peer":
		s.unregisterPeerSession(msg.SessionID)

	case "ping":
		// 响应 ping
		response := SignalingMessage{
			Type:      "pong",
			SessionID: msg.SessionID,
			Timestamp: time.Now().Unix(),
		}
		client.sendCh <- marshalMessage(response)

	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

// forwardMessage 转发消息到目标客户端
func (s *SignalingServer) forwardMessage(targetID string, msg SignalingMessage) {
	s.clientsMu.RLock()
	client, ok := s.clients[targetID]
	s.clientsMu.RUnlock()

	if !ok {
		log.Printf("Target client not found: %s", targetID)
		return
	}

	client.sendCh <- marshalMessage(msg)
}

// registerPeerSession 注册对等方会话
func (s *SignalingServer) registerPeerSession(sessionID, peerID string) {
	s.sessionsMu.Lock()
	s.sessions[sessionID] = peerID
	s.sessionsMu.Unlock()

	log.Printf("Peer registered: %s <-> %s", sessionID, peerID)
}

// unregisterPeerSession 注销对等方会话
func (s *SignalingServer) unregisterPeerSession(sessionID string) {
	s.sessionsMu.Lock()
	delete(s.sessions, sessionID)
	s.sessionsMu.Unlock()
}

// unregisterClient 注销客户端
func (s *SignalingServer) unregisterClient(sessionID string) {
	s.clientsMu.Lock()
	client, ok := s.clients[sessionID]
	if ok {
		close(client.sendCh)
		delete(s.clients, sessionID)
	}
	s.clientsMu.Unlock()

	s.unregisterPeerSession(sessionID)

	if ok {
		log.Printf("Client disconnected: %s (remaining: %d)", sessionID, len(s.clients))
	}
}

// Broadcast 广播消息到所有客户端
func (s *SignalingServer) Broadcast(msg SignalingMessage) {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	msg.Timestamp = time.Now().Unix()
	data := marshalMessage(msg)

	for _, client := range s.clients {
		select {
		case client.sendCh <- data:
		default:
			log.Printf("Send buffer full for client: %s", client.sessionID)
		}
	}
}

// SendTo 发送消息到指定客户端
func (s *SignalingServer) SendTo(sessionID string, msg SignalingMessage) error {
	s.clientsMu.RLock()
	client, ok := s.clients[sessionID]
	s.clientsMu.RUnlock()

	if !ok {
		return fmt.Errorf("client not found: %s", sessionID)
	}

	msg.Timestamp = time.Now().Unix()
	data := marshalMessage(msg)

	select {
	case client.sendCh <- data:
		return nil
	default:
		return fmt.Errorf("send buffer full")
	}
}

// GetPeerCount 获取连接的对等方数量
func (s *SignalingServer) GetPeerCount() int {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return len(s.clients)
}

// GetSessionInfo 获取会话信息
func (s *SignalingServer) GetSessionInfo(sessionID string) (map[string]interface{}, bool) {
	s.clientsMu.RLock()
	client, ok := s.clients[sessionID]
	s.clientsMu.RUnlock()

	if !ok {
		return nil, false
	}

	return map[string]interface{}{
		"session_id":   client.sessionID,
		"connected_at": client.connectedAt,
		"uptime":       time.Since(client.connectedAt).String(),
	}, true
}

// marshalMessage 编组消息
func marshalMessage(msg SignalingMessage) []byte {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal message: %v", err)
		return nil
	}
	return data
}

// generateSessionID 生成会话 ID
func generateSessionID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// DirectSignaling 直接信令（不通过服务器，用于已知的对等方）
type DirectSignaling struct {
	peerID      string
	offerSDP    *webrtc.SessionDescription
	answerSDP   *webrtc.SessionDescription
	iceChan     chan *webrtc.ICECandidateInit
	mu          sync.RWMutex
	onOffer     func(*webrtc.SessionDescription)
	onAnswer    func(*webrtc.SessionDescription)
	onICE       func(*webrtc.ICECandidateInit)
}

// NewDirectSignaling 创建直接信令
func NewDirectSignaling(peerID string) *DirectSignaling {
	return &DirectSignaling{
		peerID:  peerID,
		iceChan: make(chan *webrtc.ICECandidateInit, 100),
	}
}

// SetOffer 设置 Offer
func (ds *DirectSignaling) SetOffer(sdp *webrtc.SessionDescription) {
	ds.mu.Lock()
	ds.offerSDP = sdp
	ds.mu.Unlock()

	if ds.onOffer != nil {
		ds.onOffer(sdp)
	}
}

// GetOffer 获取 Offer
func (ds *DirectSignaling) GetOffer() *webrtc.SessionDescription {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.offerSDP
}

// SetAnswer 设置 Answer
func (ds *DirectSignaling) SetAnswer(sdp *webrtc.SessionDescription) {
	ds.mu.Lock()
	ds.answerSDP = sdp
	ds.mu.Unlock()

	if ds.onAnswer != nil {
		ds.answerSDP = sdp
	}
}

// GetAnswer 获取 Answer
func (ds *DirectSignaling) GetAnswer() *webrtc.SessionDescription {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.answerSDP
}

// AddICECandidate 添加 ICE 候选者
func (ds *DirectSignaling) AddICECandidate(candidate *webrtc.ICECandidateInit) {
	ds.iceChan <- candidate

	if ds.onICE != nil {
		ds.onICE(candidate)
	}
}

// ICEChannel 获取 ICE 候选者通道
func (ds *DirectSignaling) ICEChannel() <-chan *webrtc.ICECandidateInit {
	return ds.iceChan
}

// SetOnOffer 设置 Offer 回调
func (ds *DirectSignaling) SetOnOffer(fn func(*webrtc.SessionDescription)) {
	ds.onOffer = fn
}

// SetOnAnswer 设置 Answer 回调
func (ds *DirectSignaling) SetOnAnswer(fn func(*webrtc.SessionDescription)) {
	ds.onAnswer = fn
}

// SetOnICE 设置 ICE 候选者回调
func (ds *DirectSignaling) SetOnICE(fn func(*webrtc.ICECandidateInit)) {
	ds.onICE = fn
}

// Close 关闭直接信令
func (ds *DirectSignaling) Close() {
	close(ds.iceChan)
}

// SignalingClientForServer 服务器端信令客户端
type SignalingClientForServer struct {
	serverURL  string
	sessionID  string
	conn       *websocket.Conn
	sendCh     chan []byte
	recvCh     chan SignalingMessage
	mu         sync.RWMutex
	connected  bool
	onMessage  func(SignalingMessage)
}

// NewSignalingClientForServer 创建服务器端信令客户端
func NewSignalingClientForServer(serverURL string) *SignalingClientForServer {
	return &SignalingClientForServer{
		serverURL: serverURL,
		sessionID: generateSessionID(),
		sendCh:    make(chan []byte, 100),
		recvCh:    make(chan SignalingMessage, 100),
		connected: false,
	}
}

// Connect 连接到信令服务器
func (sc *SignalingClientForServer) Connect() error {
	conn, _, err := websocket.DefaultDialer.Dial(sc.serverURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to signaling server: %w", err)
	}

	sc.mu.Lock()
	sc.conn = conn
	sc.connected = true
	sc.mu.Unlock()

	// 启动发送和接收协程
	go sc.sendMessages()
	go sc.receiveMessages()

	return nil
}

// Disconnect 断开连接
func (sc *SignalingClientForServer) Disconnect() error {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.conn != nil {
		sc.connected = false
		close(sc.sendCh)
		return sc.conn.Close()
	}
	return nil
}

// Send 发送消息
func (sc *SignalingClientForServer) Send(msg SignalingMessage) error {
	sc.mu.RLock()
	connected := sc.connected
	sc.mu.RUnlock()

	if !connected {
		return fmt.Errorf("not connected")
	}

	msg.SessionID = sc.sessionID
	msg.Timestamp = time.Now().Unix()
	data := marshalMessage(msg)

	select {
	case sc.sendCh <- data:
		return nil
	default:
		return fmt.Errorf("send buffer full")
	}
}

// sendMessages 发送消息
func (sc *SignalingClientForServer) sendMessages() {
	for msg := range sc.sendCh {
		sc.mu.RLock()
		conn := sc.conn
		sc.mu.RUnlock()

		if conn == nil {
			continue
		}

		err := conn.WriteMessage(websocket.TextMessage, msg)
		if err != nil {
			log.Printf("Failed to send message: %v", err)
			return
		}
	}
}

// receiveMessages 接收消息
func (sc *SignalingClientForServer) receiveMessages() {
	for {
		sc.mu.RLock()
		conn := sc.conn
		sc.mu.RUnlock()

		if conn == nil {
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Failed to read message: %v", err)
			return
		}

		var msg SignalingMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Failed to parse message: %v", err)
			continue
		}

		if sc.onMessage != nil {
			sc.onMessage(msg)
		}

		select {
		case sc.recvCh <- msg:
		default:
			log.Printf("Receive buffer full, dropping message")
		}
	}
}

// SetOnMessage 设置消息接收回调
func (sc *SignalingClientForServer) SetOnMessage(fn func(SignalingMessage)) {
	sc.mu.Lock()
	sc.onMessage = fn
	sc.mu.Unlock()
}

// ReceiveChannel 获取接收通道
func (sc *SignalingClientForServer) ReceiveChannel() <-chan SignalingMessage {
	return sc.recvCh
}

// GetSessionID 获取会话 ID
func (sc *SignalingClientForServer) GetSessionID() string {
	return sc.sessionID
}

// IsConnected 检查是否已连接
func (sc *SignalingClientForServer) IsConnected() bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.connected
}
