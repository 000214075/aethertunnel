package vpn

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/obfuscation"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// VPN represents a virtual private network manager
type VPN struct {
	mu              sync.RWMutex
	cfg             *config.Config
	encryption      *crypto.Encryption
	obfuscator      *obfuscation.Obfuscation
	tunnels         map[string]*Tunnel
	clients         map[string]*ConnectedClient
	routes          map[string]*Route
	listener        net.Listener
	performance     PerformanceOptimizerInterface
	connPool        *ConnectionPool
	stats           *VPNStats
}

// Tunnel represents a VPN tunnel
type Tunnel struct {
	ID          string
	Name        string
	LocalIP     net.IP
	RemoteIP    net.IP
	Netmask     net.IPMask
	Clients     map[string]*ConnectedClient
	Routes      map[string]*Route
	Encryption  *crypto.Encryption
	Protocol    string
	Obfuscation bool
	mu          sync.RWMutex
}

// ConnectedClient represents a connected VPN client (服务端视角)
type ConnectedClient struct {
	ID           string
	Name         string
	IP           net.IP
	MAC          string
	TunnelID     string
	Connected    bool
	LastSeen     int64
	SendBytes    uint64
	ReceiveBytes uint64
	Latency      int64
	mu           sync.RWMutex
}

// Route represents a VPN route
type Route struct {
	ID        string
	Network   *net.IPNet
	NextHop   net.IP
	Interface string
	Metric    int
	Enabled   bool
	mu        sync.RWMutex
}

// NewVPN creates a new VPN manager
func NewVPN(cfg *config.Config, encryption *crypto.Encryption) *VPN {
	// Create obfuscator if enabled
	var obfuscator *obfuscation.Obfuscation
	if cfg.Obfuscation.Enabled {
		obfuscator = obfuscation.NewObfuscation(encryption)
	}

	return &VPN{
		cfg:             cfg,
		encryption:      encryption,
		obfuscator:      obfuscator,
		tunnels:         make(map[string]*Tunnel),
		clients:         make(map[string]*ConnectedClient),
		routes:          make(map[string]*Route),
		performance:     nil, // Will be set later
		connPool:        NewConnectionPool(cfg.VPN.MaxPoolSize, 30*time.Minute),
		stats:           NewVPNStats(),
	}
}

// Start starts the VPN server
func (v *VPN) Start() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Start VPN listener based on protocol
	var err error
	switch v.cfg.VPN.Protocol {
	case "tcp":
		v.listener, err = net.Listen("tcp", fmt.Sprintf("%s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port))
		if err != nil {
			return fmt.Errorf("failed to start TCP VPN listener: %v", err)
		}
		log.Printf("TCP VPN server started on %s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port)

	case "udp":
		v.listener, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(v.cfg.VPN.BindAddr), Port: v.cfg.VPN.Port})
		if err != nil {
			return fmt.Errorf("failed to start UDP VPN listener: %v", err)
		}
		log.Printf("UDP VPN server started on %s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port)

	case "sctp":
		v.listener, err = sctp.Listen("sctp", fmt.Sprintf("%s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port))
		if err != nil {
			return fmt.Errorf("failed to start SCTP VPN listener: %v", err)
		}
		log.Printf("SCTP VPN server started on %s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port)

	case "websocket":
		wsServer := protocol.NewWebSocketServer(protocol.DefaultWebSocketConfig(), v.handleWebSocketConnection)
		go func() {
			if err := wsServer.Start(fmt.Sprintf("%s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port)); err != nil {
				log.Printf("WebSocket VPN server failed: %v", err)
			}
		}()
		log.Printf("WebSocket VPN server started on %s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port)
		return nil

	case "http":
		httpServer := protocol.NewHTTPServer(protocol.DefaultHTTPConfig(), v.handleHTTPConnection)
		go func() {
			if err := httpServer.Start(fmt.Sprintf("%s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port)); err != nil {
				log.Printf("HTTP VPN server failed: %v", err)
			}
		}()
		log.Printf("HTTP VPN server started on %s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port)
		return nil

	default:
		return fmt.Errorf("unsupported VPN protocol: %s", v.cfg.VPN.Protocol)
	}

	// Enable performance optimization
	if v.cfg.VPN.EnablePerformance {
		v.performance.Enable()
		log.Printf("Performance optimization enabled")
	}

	// Start additional protocol listeners if enabled
	if v.cfg.VPN.EnableHTTPForward {
		go v.startHTTPForwarder()
		log.Printf("HTTP forwarding enabled")
	}
	if v.cfg.VPN.EnableSCTPForward {
		go v.startSCTPForwarder()
		log.Printf("SCTP forwarding enabled")
	}
	if v.cfg.VPN.EnableWSForward {
		go v.startWSForwarder()
		log.Printf("WebSocket forwarding enabled")
	}

	// Start handling VPN connections
	go v.handleVPNConnections()

	// Create default tunnel if enabled
	if v.cfg.VPN.Enabled {
		_, err := v.CreateTunnel(&CreateTunnelRequest{
			Name:        "default",
			LocalIP:     net.ParseIP(v.cfg.VPN.LocalIP),
			RemoteIP:    net.ParseIP(v.cfg.VPN.RemoteIP),
			Netmask:     net.IPv4Mask(255, 255, 255, 0),
			Protocol:    v.cfg.VPN.Protocol,
			Obfuscation: v.cfg.VPN.Obfuscation,
		})
		if err != nil {
			log.Printf("Failed to create default tunnel: %v", err)
		}
	}

	return nil
}

// handleVPNConnections handles incoming VPN connections
func (v *VPN) handleVPNConnections() {
	for {
		conn, err := v.listener.Accept()
		if err != nil {
			log.Printf("VPN accept error: %v", err)
			return
		}

		// Check rate limiter
		if v.performance != nil && !v.performance.AllowConnection() {
			log.Printf("Rate limit exceeded, dropping connection from: %s", conn.RemoteAddr())
			conn.Close()
			continue
		}

		go v.handleVPNConnection(conn)
	}
}

// handleVPNConnection handles a single VPN connection
func (v *VPN) handleVPNConnection(conn net.Conn) {
	defer conn.Close()

	log.Printf("VPN connection from: %s", conn.RemoteAddr())

	// Record connection in stats
	if v.stats != nil {
		v.stats.IncrementActiveConnections()
		defer v.stats.DecrementActiveConnections()
	}

	// Perform VPN handshake
	clientID, tunnelID, err := v.performVPNHandshake(conn)
	if err != nil {
		log.Printf("VPN handshake failed: %v", err)
		if v.performance != nil {
			v.performance.RecordFailure()
		}
		return
	}

	if v.performance != nil {
		v.performance.RecordSuccess()
	}

	// Register client
	client := v.registerVPNClient(clientID, tunnelID, conn)
	if client == nil {
		log.Printf("Failed to register VPN client: %s", clientID)
		return
	}

	log.Printf("VPN client registered: %s (ID: %s)", client.Name, client.ID)

	// Handle VPN data
	v.handleVPNData(conn, client)
}

// performVPNHandshake performs VPN handshake
func (v *VPN) performVPNHandshake(conn net.Conn) (string, string, error) {
	// Read handshake message
	msg, err := protocol.ReadMessage(conn)
	if err != nil {
		return "", "", fmt.Errorf("failed to read handshake: %v", err)
	}

	if msg.Type != protocol.MessageTypeVPNHandshake {
		return "", "", fmt.Errorf("unexpected message type: %d", msg.Type)
	}

	// Decrypt handshake payload
	decrypted, err := v.encryption.Decrypt(msg.Payload)
	if err != nil {
		return "", "", fmt.Errorf("failed to decrypt handshake: %v", err)
	}

	// Parse handshake data
	var handshake VPNHandshake
	if err := json.Unmarshal(decrypted, &handshake); err != nil {
		return "", "", fmt.Errorf("failed to parse handshake: %v", err)
	}

	// Validate auth token
	if handshake.AuthToken != v.cfg.VPN.AuthToken {
		return "", "", fmt.Errorf("invalid auth token")
	}

	return handshake.ClientID, handshake.TunnelID, nil
}

// registerVPNClient registers a VPN client
func (v *VPN) registerVPNClient(clientID, tunnelID string, conn net.Conn) *ConnectedClient {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Check if client already exists
	if client, exists := v.clients[clientID]; exists {
		client.mu.Lock()
		client.Connected = true
		client.LastSeen = time.Now().Unix()
		client.mu.Unlock()
		return client
	}

	// Create new client
	client := &ConnectedClient{
		ID:        clientID,
		Name:      fmt.Sprintf("Client_%s", clientID),
		IP:        v.allocateClientIP(tunnelID),
		TunnelID:  tunnelID,
		Connected: true,
		LastSeen:  time.Now().Unix(),
	}

	v.clients[clientID] = client

	// Add client to tunnel
	if tunnel, exists := v.tunnels[tunnelID]; exists {
		tunnel.mu.Lock()
		tunnel.Clients[clientID] = client
		tunnel.mu.Unlock()
	}

	return client
}

// allocateClientIP allocates an IP address for a client
func (v *VPN) allocateClientIP(tunnelID string) net.IP {
	tunnel, exists := v.tunnels[tunnelID]
	if !exists {
		return nil
	}

	// Simple IP allocation algorithm
	// In production, use a proper IPAM
	tunnel.mu.RLock()
	defer tunnel.mu.RUnlock()

	allocated := make(map[string]bool)
	for _, client := range tunnel.Clients {
		if client.IP != nil {
			allocated[client.IP.String()] = true
		}
	}

	// Find first available IP
	for i := 2; i < 255; i++ {
		testIP := net.IPv4(
			tunnel.RemoteIP[0],
			tunnel.RemoteIP[1],
			tunnel.RemoteIP[2],
			byte(i),
		)

		if !allocated[testIP.String()] {
			return testIP
		}
	}

	return nil
}

// CreateTunnel creates a new VPN tunnel
func (v *VPN) CreateTunnel(req *CreateTunnelRequest) (*Tunnel, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	tunnelID := fmt.Sprintf("tunnel_%s", req.Name)

	// Check if tunnel already exists
	if _, exists := v.tunnels[tunnelID]; exists {
		return nil, fmt.Errorf("tunnel already exists: %s", req.Name)
	}

	// Create tunnel
	tunnel := &Tunnel{
		ID:         tunnelID,
		Name:       req.Name,
		LocalIP:    req.LocalIP,
		RemoteIP:   req.RemoteIP,
		Netmask:    req.Netmask,
		Clients:    make(map[string]*ConnectedClient),
		Routes:     make(map[string]*Route),
		Encryption: v.encryption,
		Protocol:   req.Protocol,
		Obfuscation: req.Obfuscation,
	}

	v.tunnels[tunnelID] = tunnel

	// Create routes for this tunnel
	v.createTunnelRoutes(tunnel)

	log.Printf("VPN tunnel created: %s (ID: %s)", tunnel.Name, tunnel.ID)

	return tunnel, nil
}

// createTunnelRoutes creates routes for a tunnel
func (v *VPN) createTunnelRoutes(tunnel *Tunnel) {
	// Create route for tunnel network
	route := &Route{
		ID:        fmt.Sprintf("route_%s", tunnel.ID),
		Network:   &net.IPNet{IP: tunnel.RemoteIP, Mask: tunnel.Netmask},
		NextHop:   tunnel.LocalIP,
		Interface: fmt.Sprintf("tun_%s", tunnel.Name),
		Metric:    100,
		Enabled:   true,
	}

	tunnel.Routes[route.ID] = route
	v.routes[route.ID] = route
}

// DeleteTunnel deletes a VPN tunnel
func (v *VPN) DeleteTunnel(tunnelID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	tunnel, exists := v.tunnels[tunnelID]
	if !exists {
		return fmt.Errorf("tunnel not found: %s", tunnelID)
	}

	// Disconnect all clients
	for _, client := range tunnel.Clients {
		client.mu.Lock()
		client.Connected = false
		client.mu.Unlock()
	}

	// Remove routes
	for _, route := range tunnel.Routes {
		delete(v.routes, route.ID)
	}

	// Remove tunnel
	delete(v.tunnels, tunnelID)

	log.Printf("VPN tunnel deleted: %s (ID: %s)", tunnel.Name, tunnel.ID)

	return nil
}

// GetTunnel returns a tunnel by ID
func (v *VPN) GetTunnel(tunnelID string) (*Tunnel, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	tunnel, exists := v.tunnels[tunnelID]
	if !exists {
		return nil, fmt.Errorf("tunnel not found: %s", tunnelID)
	}

	return tunnel, nil
}

// ListTunnels returns all tunnels
func (v *VPN) ListTunnels() []*Tunnel {
	v.mu.RLock()
	defer v.mu.RUnlock()

	tunnels := make([]*Tunnel, 0, len(v.tunnels))
	for _, tunnel := range v.tunnels {
		tunnels = append(tunnels, tunnel)
	}

	return tunnels
}

// GetClient returns a client by ID
func (v *VPN) GetClient(clientID string) (*ConnectedClient, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	client, exists := v.clients[clientID]
	if !exists {
		return nil, fmt.Errorf("client not found: %s", clientID)
	}

	return client, nil
}

// ListClients returns all clients
func (v *VPN) ListClients() []*ConnectedClient {
	v.mu.RLock()
	defer v.mu.RUnlock()

	clients := make([]*ConnectedClient, 0, len(v.clients))
	for _, client := range v.clients {
		clients = append(clients, client)
	}

	return clients
}

// AddRoute adds a route to a tunnel
func (v *VPN) AddRoute(tunnelID string, route *Route) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	tunnel, exists := v.tunnels[tunnelID]
	if !exists {
		return fmt.Errorf("tunnel not found: %s", tunnelID)
	}

	routeID := fmt.Sprintf("route_%s_%s", tunnelID, route.Network.String())
	route.ID = routeID
	route.Enabled = true

	tunnel.Routes[routeID] = route
	v.routes[routeID] = route

	return nil
}

// DeleteRoute deletes a route
func (v *VPN) DeleteRoute(routeID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Find which tunnel this route belongs to
	for _, tunnel := range v.tunnels {
		if _, exists := tunnel.Routes[routeID]; exists {
			delete(tunnel.Routes, routeID)
			delete(v.routes, routeID)
			return nil
		}
	}

	return fmt.Errorf("route not found: %s", routeID)
}

// startHTTPForwarder starts HTTP protocol forwarder
func (v *VPN) startHTTPForwarder() {
	httpServer := protocol.NewHTTPServer(protocol.DefaultHTTPConfig(), v.handleHTTPConnection)
	addr := fmt.Sprintf("%s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port+1)
	if err := httpServer.Start(addr); err != nil {
		log.Printf("Failed to start HTTP forwarder: %v", err)
	} else {
		log.Printf("HTTP forwarder started on %s", addr)
	}
}

// startSCTPForwarder starts SCTP protocol forwarder
func (v *VPN) startSCTPForwarder() {
	listener, err := sctp.Listen("sctp", fmt.Sprintf("%s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port+2))
	if err != nil {
		log.Printf("Failed to start SCTP forwarder: %v", err)
		return
	}
	log.Printf("SCTP forwarder started on %s", listener.Addr())

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("SCTP forwarder accept error: %v", err)
			continue
		}
		go v.handleSCTPConnection(conn)
	}
}

// startWSForwarder starts WebSocket protocol forwarder
func (v *VPN) startWSForwarder() {
	wsServer := protocol.NewWebSocketServer(protocol.DefaultWebSocketConfig(), v.handleWebSocketConnection)
	addr := fmt.Sprintf("%s:%d", v.cfg.VPN.BindAddr, v.cfg.VPN.Port+3)
	if err := wsServer.Start(addr); err != nil {
		log.Printf("Failed to start WebSocket forwarder: %v", err)
	} else {
		log.Printf("WebSocket forwarder started on %s", addr)
	}
}

// handleHTTPConnection handles HTTP protocol connections for VPN
func (v *VPN) handleHTTPConnection(httpConn *protocol.HTTPConn) {
	defer httpConn.Close()

	log.Printf("HTTP VPN connection from: %s", httpConn.Addr())

	// Perform VPN handshake over HTTP
	clientID, tunnelID, err := v.performVPNHandshakeOverHTTP(httpConn)
	if err != nil {
		log.Printf("HTTP VPN handshake failed: %v", err)
		return
	}

	// Register client
	client := v.registerVPNClient(clientID, tunnelID, httpConn)
	if client == nil {
		log.Printf("Failed to register VPN client: %s", clientID)
		return
	}

	log.Printf("HTTP VPN client registered: %s (ID: %s)", client.Name, client.ID)

	// Handle VPN data over HTTP
	v.handleVPNDataOverHTTP(httpConn, client)
}

// handleSCTPConnection handles SCTP protocol connections for VPN
func (v *VPN) handleSCTPConnection(sctpConn net.Conn) {
	defer sctpConn.Close()

	log.Printf("SCTP VPN connection from: %s", sctpConn.RemoteAddr())

	// Perform VPN handshake over SCTP
	clientID, tunnelID, err := v.performVPNHandshake(sctpConn)
	if err != nil {
		log.Printf("SCTP VPN handshake failed: %v", err)
		return
	}

	// Register client
	client := v.registerVPNClient(clientID, tunnelID, sctpConn)
	if client == nil {
		log.Printf("Failed to register VPN client: %s", clientID)
		return
	}

	log.Printf("SCTP VPN client registered: %s (ID: %s)", client.Name, client.ID)

	// Handle VPN data over SCTP
	v.handleVPNData(sctpConn, client)
}

// handleWebSocketConnection handles WebSocket protocol connections for VPN
func (v *VPN) handleWebSocketConnection(wsConn *protocol.WebSocketConn) {
	defer wsConn.Close()

	log.Printf("WebSocket VPN connection from: %s", wsConn.Addr())

	// Perform VPN handshake over WebSocket
	clientID, tunnelID, err := v.performVPNHandshakeOverWebSocket(wsConn)
	if err != nil {
		log.Printf("WebSocket VPN handshake failed: %v", err)
		return
	}

	// Register client
	client := v.registerVPNClient(clientID, tunnelID, wsConn)
	if client == nil {
		log.Printf("Failed to register VPN client: %s", clientID)
		return
	}

	log.Printf("WebSocket VPN client registered: %s (ID: %s)", client.Name, client.ID)

	// Handle VPN data over WebSocket
	v.handleVPNDataOverWebSocket(wsConn, client)
}

// performVPNHandshakeOverHTTP performs VPN handshake over HTTP protocol
func (v *VPN) performVPNHandshakeOverHTTP(httpConn *protocol.HTTPConn) (string, string, error) {
	// Read handshake from HTTP request body
	handshakeData, err := httpConn.Read()
	if err != nil {
		return "", "", fmt.Errorf("failed to read HTTP handshake: %v", err)
	}

	// Parse handshake
	var handshake VPNHandshake
	if err := json.Unmarshal(handshakeData, &handshake); err != nil {
		return "", "", fmt.Errorf("failed to parse HTTP handshake: %v", err)
	}

	// Validate auth token
	if handshake.AuthToken != v.cfg.VPN.AuthToken {
		return "", "", fmt.Errorf("invalid auth token")
	}

	return handshake.ClientID, handshake.TunnelID, nil
}

// performVPNHandshakeOverWebSocket performs VPN handshake over WebSocket protocol
func (v *VPN) performVPNHandshakeOverWebSocket(wsConn *protocol.WebSocketConn) (string, string, error) {
	// Read handshake from WebSocket
	handshakeData, err := wsConn.Read()
	if err != nil {
		return "", "", fmt.Errorf("failed to read WebSocket handshake: %v", err)
	}

	// Parse handshake
	var handshake VPNHandshake
	if err := json.Unmarshal(handshakeData, &handshake); err != nil {
		return "", "", fmt.Errorf("failed to parse WebSocket handshake: %v", err)
	}

	// Validate auth token
	if handshake.AuthToken != v.cfg.VPN.AuthToken {
		return "", "", fmt.Errorf("invalid auth token")
	}

	return handshake.ClientID, handshake.TunnelID, nil
}

// handleVPNDataOverHTTP handles VPN data over HTTP protocol
func (v *VPN) handleVPNDataOverHTTP(httpConn *protocol.HTTPConn, client *ConnectedClient) {
	for {
		// Read VPN data from HTTP
		data, err := httpConn.Read()
		if err != nil {
			log.Printf("Failed to read HTTP VPN data: %v", err)
			client.mu.Lock()
			client.Connected = false
			client.mu.Unlock()
			return
		}

		// Process VPN packet
		var packet VPNPacket
		if err := json.Unmarshal(data, &packet); err != nil {
			log.Printf("Failed to parse HTTP VPN packet: %v", err)
			continue
		}

		// Handle packet based on type
		switch packet.Type {
		case "data":
			v.handleVPNDataPacket(client, packet.Data)
		case "keepalive":
			v.handleVPNKeepalive(client)
		case "route":
			v.handleVPNRoutePacket(client, packet.Route)
		}
	}
}

// handleVPNDataOverWebSocket handles VPN data over WebSocket protocol
func (v *VPN) handleVPNDataOverWebSocket(wsConn *protocol.WebSocketConn, client *ConnectedClient) {
	for {
		// Read VPN data from WebSocket
		data, err := wsConn.Read()
		if err != nil {
			log.Printf("Failed to read WebSocket VPN data: %v", err)
			client.mu.Lock()
			client.Connected = false
			client.mu.Unlock()
			return
		}

		// Process VPN packet
		var packet VPNPacket
		if err := json.Unmarshal(data, &packet); err != nil {
			log.Printf("Failed to parse WebSocket VPN packet: %v", err)
			continue
		}

		// Handle packet based on type
		switch packet.Type {
		case "data":
			v.handleVPNDataPacket(client, packet.Data)
		case "keepalive":
			v.handleVPNKeepalive(client)
		case "route":
			v.handleVPNRoutePacket(client, packet.Route)
		}
	}
}

// Stop stops the VPN server
func (v *VPN) Stop() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.listener != nil {
		v.listener.Close()
	}

	// Disconnect all clients
	for _, client := range v.clients {
		client.mu.Lock()
		client.Connected = false
		client.mu.Unlock()
	}

	return nil
}

// VPNHandshake represents VPN handshake message
type VPNHandshake struct {
	ClientID   string `json:"client_id"`
	TunnelID   string `json:"tunnel_id"`
	AuthToken  string `json:"auth_token"`
	ClientInfo string `json:"client_info"`
}

// CreateTunnelRequest represents tunnel creation request
type CreateTunnelRequest struct {
	Name        string    `json:"name"`
	LocalIP     net.IP    `json:"local_ip"`
	RemoteIP    net.IP    `json:"remote_ip"`
	Netmask     net.IPMask `json:"netmask"`
	Protocol    string    `json:"protocol"`
	Obfuscation bool      `json:"obfuscation"`
}

// Add this to handle VPN data
func (v *VPN) handleVPNData(conn net.Conn, client *ConnectedClient) {
	for {
		// Read encrypted packet
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			log.Printf("Failed to read VPN data: %v", err)
			client.mu.Lock()
			client.Connected = false
			client.mu.Unlock()
			return
		}

		// Measure latency for performance stats
		startTime := time.Now()

		// Decrypt packet
		decrypted, err := v.encryption.Decrypt(msg.Payload)
		if err != nil {
			log.Printf("Failed to decrypt VPN packet: %v", err)
			if v.performance != nil {
				v.performance.RecordFailure()
			}
			continue
		}

		// Process VPN packet
		var packet VPNPacket
		if err := json.Unmarshal(decrypted, &packet); err != nil {
			log.Printf("Failed to parse VPN packet: %v", err)
			continue
		}

		// Handle packet based on type
		switch packet.Type {
		case "data":
			v.handleVPNDataPacket(client, packet.Data)
		case "keepalive":
			v.handleVPNKeepalive(client)
		case "route":
			v.handleVPNRoutePacket(client, packet.Route)
		}

		// Update performance stats
		if v.performance != nil {
			latency := time.Since(startTime)
			v.performance.UpdateStats(len(packet.Data), latency)
			v.performance.RecordSuccess()
		}

		// Update VPN stats
		if v.stats != nil {
			v.stats.AddBytesReceived(uint64(len(packet.Data)))
		}
	}
}

// handleVPNDataPacket handles VPN data packet
func (v *VPN) handleVPNDataPacket(client *ConnectedClient, data []byte) {
	// Update client stats
	client.mu.Lock()
	client.ReceiveBytes += uint64(len(data))
	client.LastSeen = time.Now().Unix()
	client.mu.Unlock()

	// Forward data to destination based on routing
	// This is where the actual VPN routing happens
}

// handleVPNKeepalive handles VPN keepalive packet
func (v *VPN) handleVPNKeepalive(client *ConnectedClient) {
	client.mu.Lock()
	client.LastSeen = time.Now().Unix()
	client.mu.Unlock()

	// Send keepalive response
	keepaliveResp := VPNPacket{
		Type: "keepalive",
		Data: []byte("OK"),
	}

	// In production, this would send back to client
}

// handleVPNRoutePacket handles VPN route packet
func (v *VPN) handleVPNRoutePacket(client *ConnectedClient, route *Route) {
	// Client is requesting route update
	route.mu.Lock()
	route.Enabled = true
	route.mu.Unlock()

	log.Printf("Route added from client %s: %s", client.ID, route.Network)
}

// VPNPacket represents a VPN packet
type VPNPacket struct {
	Type   string          `json:"type"`
	Data   []byte          `json:"data"`
	Route  *Route          `json:"route"`
	Source string          `json:"source"`
	Dest   string          `json:"dest"`
}