package webrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/ice"
)

// PeerConnectionManager 管理 WebRTC 点对点连接
type PeerConnectionManager struct {
	api                 *webrtc.API
	peerConnection      *webrtc.PeerConnection
	dataChannel         *webrtc.DataChannel
	config              Config
	mu                  sync.RWMutex
	onConnected         func()
	onDisconnected      func()
	onData              func([]byte)
	onSignal            func(webrtc.SessionDescription) webrtc.SessionDescription
	signalCh            chan SignalMessage
	connState           ConnectionState
	connStateCh         chan ConnectionState
	incomingDataChannel chan *webrtc.DataChannel
	stunServers         []string
	turnServers         []string
}

// Config WebRTC 配置
type Config struct {
	ICEServers          []ICEServer
	EnableDataChannel   bool
	DataChannelLabel    string
	EnableICE           bool
	ICECandidateType    webrtc.ICECandidateType
	ICETransportPolicy  webrtc.ICETransportPolicy
	MaxMessageSize      uint16
	Ordered             bool
	Retransmits         uint16
	AutoRelayDisabled   bool // 禁用中继，强制P2P
	STUNServers         []string
	TURNServers         []string
}

// ICEServer ICE 服务器配置
type ICEServer struct {
	URLs           []string
	Username       string
	Credential     string
	CredentialType webrtc.ICECredentialType
}

// SignalMessage 信令消息
type SignalMessage struct {
	Type         string                  `json:"type"`
	SessionID    string                  `json:"session_id"`
	SDP          webrtc.SessionDescription `json:"sdp,omitempty"`
	ICECandidate webrtc.ICECandidate     `json:"ice_candidate,omitempty"`
	Timestamp    int64                   `json:"timestamp"`
}

// ConnectionState 连接状态
type ConnectionState int

const (
	Disconnected ConnectionState = iota
	Connecting
	Connected
	Failed
	Closed
)

func (s ConnectionState) String() string {
	return [...]string{"Disconnected", "Connecting", "Connected", "Failed", "Closed"}[s]
}

// NewPeerConnectionManager 创建新的 WebRTC 连接管理器
func NewPeerConnectionManager(config Config) (*PeerConnectionManager, error) {
	// 设置 STUN/TURN 服务器
	stunServers := config.STUNServers
	if len(stunServers) == 0 {
		stunServers = []string{
			"stun:stun.l.google.com:19302",
			"stun:stun1.l.google.com:19302",
			"stun:global.stun.twilio.com:3478",
		}
	}

	// 构建 ICE 服务器配置
	iceServers := make([]webrtc.ICEServer, 0, len(config.ICEServers)+len(stunServers)+len(config.TURNServers))

	// 添加 STUN 服务器
	for _, stun := range stunServers {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs: []string{stun},
		})
	}

	// 添加用户配置的 ICE 服务器
	for _, iceSrv := range config.ICEServers {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:           iceSrv.URLs,
			Username:       iceSrv.Username,
			Credential:     iceSrv.Credential,
			CredentialType: iceSrv.CredentialType,
		})
	}

	// 添加 TURN 服务器
	for _, turn := range config.TURNServers {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       []string{turn},
			Credential: "", // 需要配置
			Username:   "",
		})
	}

	// 创建 WebRTC API
	mediaEngine := &webrtc.MediaEngine{}
	mediaEngine.RegisterDefaultCodecs()

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	// 创建 PeerConnection 配置
	rtcConfig := webrtc.Configuration{
		ICEServers:   iceServers,
		ICETransportPolicy: webrtc.ICETransportPolicy(config.ICETransportPolicy),
	}

	// 如果禁用中继，强制使用 relay 之外的候选者
	if config.AutoRelayDisabled {
		rtcConfig.ICETransportPolicy = webrtc.ICETransportPolicyAll
	}

	peerConnection, err := api.NewPeerConnection(rtcConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create peer connection: %w", err)
	}

	manager := &PeerConnectionManager{
		api:                 api,
		peerConnection:      peerConnection,
		config:              config,
		signalCh:            make(chan SignalMessage, 100),
		connStateCh:         make(chan ConnectionState, 10),
		incomingDataChannel: make(chan *webrtc.DataChannel, 10),
		stunServers:         stunServers,
		turnServers:         config.TURNServers,
		connState:           Disconnected,
	}

	// 设置 ICE 候选者回调
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		manager.sendSignal(SignalMessage{
			Type:         "ice_candidate",
			ICECandidate: *candidate,
			Timestamp:    time.Now().Unix(),
		})
	})

	// 设置连接状态变化回调
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		var connState ConnectionState
		switch state {
		case webrtc.PeerConnectionStateNew, webrtc.PeerConnectionStateConnecting:
			connState = Connecting
		case webrtc.PeerConnectionStateConnected:
			connState = Connected
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed:
			connState = Failed
		case webrtc.PeerConnectionStateClosed:
			connState = Closed
		}

		manager.mu.Lock()
		manager.connState = connState
		manager.mu.Unlock()

		manager.connStateCh <- connState

		if connState == Connected && manager.onConnected != nil {
			manager.onConnected()
		} else if connState == Failed && manager.onDisconnected != nil {
			manager.onDisconnected()
		}
	})

	// 设置数据通道回调
	peerConnection.OnDataChannel(func(dc *webrtc.DataChannel) {
		manager.incomingDataChannel <- dc
	})

	// 如果启用数据通道，创建默认数据通道
	if config.EnableDataChannel && config.DataChannelLabel != "" {
		manager.createDataChannel(config.DataChannelLabel)
	}

	return manager, nil
}

// createDataChannel 创建数据通道
func (m *PeerConnectionManager) createDataChannel(label string) error {
	ordered := true
	if m.config.Ordered != nil {
		ordered = *m.config.Ordered
	}

	dataChannel, err := m.peerConnection.CreateDataChannel(label, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &m.config.Retransmits,
	})
	if err != nil {
		return fmt.Errorf("failed to create data channel: %w", err)
	}

	m.dataChannel = dataChannel
	m.setupDataChannelHandlers(dataChannel)

	return nil
}

// setupDataChannelHandlers 设置数据通道处理器
func (m *PeerConnectionManager) setupDataChannelHandlers(dc *webrtc.DataChannel) {
	dc.OnOpen(func() {
		log.Printf("Data channel '%s' opened", dc.Label())
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if m.onData != nil {
			m.onData(msg.Data)
		}
	})

	dc.OnClose(func() {
		log.Printf("Data channel '%s' closed", dc.Label())
		m.mu.Lock()
		if m.dataChannel == dc {
			m.dataChannel = nil
		}
		m.mu.Unlock()
	})
}

// CreateOffer 创建 Offer
func (m *PeerConnectionManager) CreateOffer() (*webrtc.SessionDescription, error) {
	offer, err := m.peerConnection.CreateOffer(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create offer: %w", err)
	}

	err = m.peerConnection.SetLocalDescription(offer)
	if err != nil {
		return nil, fmt.Errorf("failed to set local description: %w", err)
	}

	return &offer, nil
}

// CreateAnswer 创建 Answer
func (m *PeerConnectionManager) CreateAnswer() (*webrtc.SessionDescription, error) {
	answer, err := m.peerConnection.CreateAnswer(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create answer: %w", err)
	}

	err = m.peerConnection.SetLocalDescription(answer)
	if err != nil {
		return nil, fmt.Errorf("failed to set local description: %w", err)
	}

	return &answer, nil
}

// SetRemoteDescription 设置远程描述
func (m *PeerConnectionManager) SetRemoteDescription(desc webrtc.SessionDescription) error {
	return m.peerConnection.SetRemoteDescription(desc)
}

// AddICECandidate 添加 ICE 候选者
func (m *PeerConnectionManager) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	return m.peerConnection.AddICECandidate(candidate)
}

// Send 发送数据
func (m *PeerConnectionManager) Send(data []byte) error {
	m.mu.RLock()
	dc := m.dataChannel
	m.mu.RUnlock()

	if dc == nil {
		return fmt.Errorf("data channel not available")
	}

	return dc.Send(data)
}

// SendText 发送文本数据
func (m *PeerConnectionManager) SendText(text string) error {
	return m.Send([]byte(text))
}

// ReadFrom 从连接读取数据（实现 io.Reader 接口）
func (m *PeerConnectionManager) ReadFrom(p []byte) (n int, err error) {
	// 等待数据通道消息
	// 这里简化实现，实际应该使用消息队列
	return 0, io.EOF
}

// WriteTo 向连接写入数据（实现 io.Writer 接口）
func (m *PeerConnectionManager) WriteTo(p []byte) (n int, err error) {
	return len(p), m.Send(p)
}

// Close 关闭连接
func (m *PeerConnectionManager) Close() error {
	if m.peerConnection == nil {
		return nil
	}

	// 关闭数据通道
	m.mu.Lock()
	if m.dataChannel != nil {
		m.dataChannel.Close()
		m.dataChannel = nil
	}
	m.mu.Unlock()

	// 关闭连接
	return m.peerConnection.Close()
}

// GetConnectionState 获取连接状态
func (m *PeerConnectionManager) GetConnectionState() ConnectionState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connState
}

// GetStats 获取连接统计信息
func (m *PeerConnectionManager) GetStats() (map[string]interface{}, error) {
	stats := m.peerConnection.GetStats()
	result := make(map[string]interface{})

	for _, stat := range stats {
		result[stat.ID()] = stat
	}

	return result, nil
}

// SetOnConnected 设置连接成功回调
func (m *PeerConnectionManager) SetOnConnected(fn func()) {
	m.onConnected = fn
}

// SetOnDisconnected 设置断开连接回调
func (m *PeerConnectionManager) SetOnDisconnected(fn func()) {
	m.onDisconnected = fn
}

// SetOnData 设置数据接收回调
func (m *PeerConnectionManager) SetOnData(fn func([]byte)) {
	m.onData = fn
}

// SetOnSignal 设置信令回调
func (m *PeerConnectionManager) SetOnSignal(fn func(webrtc.SessionDescription) webrtc.SessionDescription) {
	m.onSignal = fn
}

// sendSignal 发送信令消息
func (m *PeerConnectionManager) sendSignal(msg SignalMessage) {
	if m.onSignal == nil {
		return
	}

	m.signalCh <- msg
}

// SignalChan 获取信令通道
func (m *PeerConnectionManager) SignalChan() <-chan SignalMessage {
	return m.signalCh
}

// ConnectionStateChan 获取连接状态通道
func (m *PeerConnectionManager) ConnectionStateChan() <-chan ConnectionState {
	return m.connStateCh
}

// ICECandidateTypeString ICE 候选者类型字符串
func ICECandidateTypeString(t webrtc.ICECandidateType) string {
	switch t {
	case webrtc.ICECandidateTypeHost:
		return "host"
	case webrtc.ICECandidateTypeSrflx:
		return "srflx"
	case webrtc.ICECandidateTypePrflx:
		return "prflx"
	case webrtc.ICECandidateTypeRelay:
		return "relay"
	default:
		return "unknown"
	}
}

// SignalServer WebRTC 信令服务器
type SignalServer struct {
	peers      map[string]*PeerConnectionManager
	peersMu    sync.RWMutex
	signalChan chan SignalMessage
	messages   map[string][]SignalMessage
	messagesMu sync.RWMutex
}

// NewSignalServer 创建信令服务器
func NewSignalServer() *SignalServer {
	return &SignalServer{
		peers:      make(map[string]*PeerConnectionManager),
		signalChan: make(chan SignalMessage, 1000),
		messages:   make(map[string][]SignalMessage),
	}
}

// RegisterPeer 注册对等方
func (s *SignalServer) RegisterPeer(sessionID string, peer *PeerConnectionManager) {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()

	s.peers[sessionID] = peer

	// 发送缓存的信令消息
	s.messagesMu.RLock()
	messages := s.messages[sessionID]
	s.messagesMu.RUnlock()

	for _, msg := range messages {
		peer.signalCh <- msg
	}

	// 清空缓存
	s.messagesMu.Lock()
	delete(s.messages, sessionID)
	s.messagesMu.Unlock()
}

// UnregisterPeer 注销对等方
func (s *SignalServer) UnregisterPeer(sessionID string) {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()

	delete(s.peers, sessionID)
}

// HandleSignal 处理信令消息
func (s *SignalServer) HandleSignal(msg SignalMessage) {
	s.peersMu.RLock()
	peer, ok := s.peers[msg.SessionID]
	s.peersMu.RUnlock()

	if ok {
		peer.signalCh <- msg
		return
	}

	// 缓存消息
	s.messagesMu.Lock()
	s.messages[msg.SessionID] = append(s.messages[msg.SessionID], msg)
	s.messagesMu.Unlock()
}

// GetPeer 获取对等方
func (s *SignalServer) GetPeer(sessionID string) (*PeerConnectionManager, bool) {
	s.peersMu.RLock()
	defer s.peersMu.RUnlock()
	peer, ok := s.peers[sessionID]
	return peer, ok
}

// Run 运行信令服务器
func (s *SignalServer) Run(ctx context.Context) error {
	for {
		select {
		case msg := <-s.signalChan:
			s.HandleSignal(msg)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// P2PConn P2P 连接包装器
type P2PConn struct {
	manager *PeerConnectionManager
	session string
	reader  chan []byte
	closed  bool
	mu      sync.RWMutex
}

// NewP2PConn 创建 P2P 连接
func NewP2PConn(manager *PeerConnectionManager, session string) *P2PConn {
	conn := &P2PConn{
		manager: manager,
		session: session,
		reader:  make(chan []byte, 100),
		closed:  false,
	}

	manager.SetOnData(func(data []byte) {
		conn.mu.RLock()
		if !conn.closed {
			conn.reader <- data
		}
		conn.mu.RUnlock()
	})

	return conn
}

// Read 实现 net.Conn 接口
func (c *P2PConn) Read(b []byte) (n int, err error) {
	data, ok := <-c.reader
	if !ok {
		return 0, io.EOF
	}

	n = copy(b, data)
	if len(data) > len(b) {
		// 如果数据太大，只返回部分，剩余数据丢弃
	}
	return n, nil
}

// Write 实现 net.Conn 接口
func (c *P2PConn) Write(b []byte) (n int, err error) {
	return len(b), c.manager.Send(b)
}

// Close 实现 net.Conn 接口
func (c *P2PConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true
	close(c.reader)

	return c.manager.Close()
}

// LocalAddr 实现 net.Conn 接口
func (c *P2PConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
}

// RemoteAddr 实现 net.Conn 接口
func (c *P2PConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
}

// SetDeadline 实现 net.Conn 接口
func (c *P2PConn) SetDeadline(t time.Time) error {
	return nil
}

// SetReadDeadline 实现 net.Conn 接口
func (c *P2PConn) SetReadDeadline(t time.Time) error {
	return nil
}

// SetWriteDeadline 实现 net.Conn 接口
func (c *P2PConn) SetWriteDeadline(t time.Time) error {
	return nil
}

// ConnectionStats 连接统计信息
type ConnectionStats struct {
	State              string
	ICEState           string
	BytesSent          uint64
	BytesReceived      uint64
	RTT                time.Duration
	SelectedPair       string
	ActivePairs        []string
	DataChannels       []string
	ConnectionUptime   time.Duration
	SelectedCandidateType string
}

// GetConnectionStats 获取连接统计信息
func (m *PeerConnectionManager) GetConnectionStats() (*ConnectionStats, error) {
	stats, err := m.GetStats()
	if err != nil {
		return nil, err
	}

	result := &ConnectionStats{
		State:         m.connState.String(),
		ICEState:      m.peerConnection.ICEConnectionState().String(),
		DataChannels:  []string{},
		ActivePairs:   []string{},
	}

	// 解析统计数据
	for _, stat := range stats {
		if stat.Type() == webrtc.StatsTypeTransport {
			if transport, ok := stat.(*webrtc.StatsTransport); ok {
				result.BytesSent = transport.BytesSent
				result.BytesReceived = transport.BytesReceived
			}
		}

		if stat.Type() == webrtc.StatsTypeCandidatePair {
			if pair, ok := stat.(*webrtc.StatsCandidatePair); ok {
				if pair.State == "succeeded" {
					result.SelectedPair = pair.LocalCandidateID + " <-> " + pair.RemoteCandidateID
					result.RTT = time.Duration(pair.CurrentRoundTripTime * float64(time.Millisecond))

					// 获取选中的候选者类型
					for _, s := range stats {
						if s.ID() == pair.LocalCandidateID {
							if candidate, ok := s.(*webrtc.StatsICECandidate); ok {
								result.SelectedCandidateType = ICECandidateTypeString(webrtc.ICECandidateType(candidate.Type))
							}
							break
						}
					}
				}
				result.ActivePairs = append(result.ActivePairs, pair.LocalCandidateID+" <-> "+pair.RemoteCandidateID)
			}
		}

		if stat.Type() == webrtc.StatsTypeDataChannel {
			if dc, ok := stat.(*webrtc.StatsDataChannel); ok {
				result.DataChannels = append(result.DataChannels, dc.Label)
			}
		}
	}

	return result, nil
}

// JSONStats 获取 JSON 格式的统计信息
func (s *ConnectionStats) JSONStats() (string, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
