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
	"github.com/aethertunnel/aethertunnel/pkg/vpn"
)

// VPNClient represents a VPN client
type VPNClient struct {
	mu             sync.RWMutex
	cfg            *config.Config
	encryption     *crypto.Encryption
	obfuscator     *obfuscation.Obfuscation
	conn           net.Conn
	tunnelID       string
	clientID       string
	localIP        net.IP
	remoteIP       net.IP
	routes         map[string]*Route
	running        bool
	performance    *vpn.PerformanceOptimizer
	stats          *VPNStats
}

// NewVPNClient creates a new VPN client
func NewVPNClient(cfg *config.Config, encryption *crypto.Encryption) *VPNClient {
	client := &VPNClient{
		cfg:         cfg,
		encryption:  encryption,
		routes:      make(map[string]*Route),
		running:     false,
		performance: vpn.NewPerformanceOptimizer(),
		stats:       NewVPNStats(),
	}

	// Create obfuscator if enabled
	if cfg.Obfuscation.Enabled {
		client.obfuscator = obfuscation.NewObfuscation(encryption)
	}

	return client
}

// Connect connects to the VPN server
func (c *VPNClient) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		return fmt.Errorf("VPN client is already running")
	}

	// Generate client ID
	c.clientID = fmt.Sprintf("client_%d", time.Now().Unix())

	// Connect to VPN server
	vpnAddr := fmt.Sprintf("%s:%d", c.cfg.Client.ServerAddr, c.cfg.VPN.Port)
	
	// Use connection pool if performance optimization is enabled
	var conn net.Conn
	var err error
	if c.cfg.VPN.EnablePerformance && c.performance != nil {
		conn, err = c.performance.GetConnection(vpnAddr, 10*time.Second)
		if err != nil {
			return fmt.Errorf("failed to connect to VPN server: %v", err)
		}
	} else {
		conn, err = net.DialTimeout("tcp", vpnAddr, 10*time.Second)
		if err != nil {
			return fmt.Errorf("failed to connect to VPN server: %v", err)
		}
	}
	c.conn = conn

	log.Printf("VPN client connected to: %s", vpnAddr)

	// Update stats
	c.stats.IncrementActiveConnections()
	c.stats.IncrementActiveTunnels()

	// Perform VPN handshake
	tunnelID, localIP, err := c.performVPNHandshake()
	if err != nil {
		c.conn.Close()
		if c.performance != nil {
			c.performance.RecordFailure()
		}
		return fmt.Errorf("VPN handshake failed: %v", err)
	}

	if c.performance != nil {
		c.performance.RecordSuccess()
	}

	c.tunnelID = tunnelID
	c.localIP = localIP

	log.Printf("VPN handshake successful. Tunnel ID: %s, Local IP: %s", tunnelID, localIP)

	// Start VPN packet handler
	c.running = true
	go c.handleVPNPackets()

	// Send keepalive
	go c.sendKeepalive()

	return nil
}

// performVPNHandshake performs VPN handshake
func (c *VPNClient) performVPNHandshake() (string, net.IP, error) {
	// Create handshake message
	handshake := VPNHandshake{
		ClientID:   c.clientID,
		TunnelID:   "default", // Default tunnel
		AuthToken:  c.cfg.VPN.AuthToken,
		ClientInfo: fmt.Sprintf("AetherTunnel Client v0.1.1"),
	}

	// Marshal handshake
	handshakeData, err := json.Marshal(handshake)
	if err != nil {
		return "", nil, fmt.Errorf("failed to marshal handshake: %v", err)
	}

	// Encrypt handshake
	encryptedHandshake, err := c.encryption.Encrypt(handshakeData)
	if err != nil {
		return "", nil, fmt.Errorf("failed to encrypt handshake: %v", err)
	}

	// Create protocol message
	handshakeMsg := protocol.NewVPNHandshakeMessage()
	handshakeMsg.Type = protocol.MessageTypeVPNHandshake
	handshakeMsg.Payload = encryptedHandshake

	// Send handshake
	if err := protocol.WriteMessage(c.conn, handshakeMsg); err != nil {
		return "", nil, fmt.Errorf("failed to send handshake: %v", err)
	}

	// Read response
	respMsg, err := protocol.ReadMessage(c.conn)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read response: %v", err)
	}

	if respMsg.Type != protocol.MessageTypeVPNHandshake {
		return "", nil, fmt.Errorf("unexpected response type: %d", respMsg.Type)
	}

	// Decrypt response
	decryptedResp, err := c.encryption.Decrypt(respMsg.Payload)
	if err != nil {
		return "", nil, fmt.Errorf("failed to decrypt response: %v", err)
	}

	// Parse response
	var resp VPNHandshakeResponse
	if err := json.Unmarshal(decryptedResp, &resp); err != nil {
		return "", nil, fmt.Errorf("failed to parse response: %v", err)
	}

	if resp.Status != "OK" {
		return "", nil, fmt.Errorf("handshake failed: %s", resp.Message)
	}

	return resp.TunnelID, resp.LocalIP, nil
}

// handleVPNPackets handles incoming VPN packets
func (c *VPNClient) handleVPNPackets() {
	for c.running {
		// Read packet
		msg, err := protocol.ReadMessage(c.conn)
		if err != nil {
			log.Printf("Failed to read VPN packet: %v", err)
			if c.performance != nil {
				c.performance.RecordFailure()
			}
			break
		}

		// Measure latency
		startTime := time.Now()

		// Handle packet based on type
		switch msg.Type {
		case protocol.MessageTypeVPNData:
			c.handleVPNData(msg)
		case protocol.MessageTypeVPNControl:
			c.handleVPNControl(msg)
		case protocol.MessageTypeVPNKeepalive:
			c.handleVPNKeepalive(msg)
		}

		// Update performance stats
		if c.performance != nil {
			c.performance.RecordSuccess()
			latency := time.Since(startTime)
			c.performance.UpdateStats(len(msg.Payload), latency)
		}
	}

	c.running = false
	log.Println("VPN packet handler stopped")
}

// handleVPNData handles VPN data packet
func (c *VPNClient) handleVPNData(msg *protocol.Message) {
	// Decrypt payload
	decrypted, err := c.encryption.Decrypt(msg.Payload)
	if err != nil {
		log.Printf("Failed to decrypt VPN data: %v", err)
		return
	}

	// If obfuscation is enabled, deobfuscate the packet
	if c.obfuscator != nil {
		obfuscatedPacket, err := c.obfuscator.DeobfuscatePacket(decrypted)
		if err != nil {
			log.Printf("Failed to deobfuscate packet: %v", err)
			return
		}

		// Process deobfuscated data
		c.processVPNData(obfuscatedPacket)
	} else {
		// Process raw data
		c.processVPNDataRaw(decrypted)
	}
}

// handleVPNControl handles VPN control packet
func (c *VPNClient) handleVPNControl(msg *protocol.Message) {
	// Decrypt control message
	decrypted, err := c.encryption.Decrypt(msg.Payload)
	if err != nil {
		log.Printf("Failed to decrypt VPN control: %v", err)
		return
	}

	// Parse control message
	var control VPNControlMessage
	if err := json.Unmarshal(decrypted, &control); err != nil {
		log.Printf("Failed to parse control message: %v", err)
		return
	}

	// Handle control message
	switch control.Command {
	case "route_add":
		c.handleRouteAdd(control.Data)
	case "route_del":
		c.handleRouteDel(control.Data)
	case "disconnect":
		c.Disconnect()
	}
}

// handleVPNKeepalive handles VPN keepalive packet
func (c *VPNClient) handleVPNKeepalive(msg *protocol.Message) {
	// Send keepalive response
	keepaliveMsg := protocol.NewVPNKeepaliveMessage()
	if err := protocol.WriteMessage(c.conn, keepaliveMsg); err != nil {
		log.Printf("Failed to send keepalive: %v", err)
	}
}

// processVPNData processes VPN data packet
func (c *VPNClient) processVPNData(packet *obfuscation.ObfuscatedPacket) {
	// Extract data based on packet type
	switch packet.Type {
	case 1: // Data packet
		// Forward to local interface
		c.forwardToLocal(packet.Payload)
	case 2: // Control packet
		// Process control information
		c.processControlData(packet.Payload)
	}
}

// processVPNDataRaw processes raw VPN data
func (c *VPNClient) processVPNDataRaw(data []byte) {
	// Create obfuscated packet for processing
	packet := &obfuscation.ObfuscatedPacket{
		Type:    1,
		Payload: data,
	}
	c.processVPNData(packet)
}

// forwardToLocal forwards data to local interface
func (c *VPNClient) forwardToLocal(data []byte) {
	// In a real VPN client, this would forward to TUN/TAP interface
	// For this implementation, we'll log the data
	log.Printf("Forwarding %d bytes to local interface", len(data))
}

// processControlData processes control data
func (c *VPNClient) processControlData(data []byte) {
	var control VPNControlMessage
	if err := json.Unmarshal(data, &control); err != nil {
		log.Printf("Failed to parse control data: %v", err)
		return
	}

	switch control.Command {
	case "ip_assign":
		var ipAssign IPAssignMessage
		if err := json.Unmarshal(control.Data, &ipAssign); err != nil {
			log.Printf("Failed to parse IP assignment: %v", err)
			return
		}
		c.mu.Lock()
		c.remoteIP = ipAssign.IP
		c.mu.Unlock()
		log.Printf("IP assigned: %s", ipAssign.IP)
	}
}

// handleRouteAdd handles route addition
func (c *VPNClient) handleRouteAdd(data []byte) {
	var route Route
	if err := json.Unmarshal(data, &route); err != nil {
		log.Printf("Failed to parse route: %v", err)
		return
	}

	c.mu.Lock()
	c.routes[route.ID] = &route
	c.mu.Unlock()

	log.Printf("Route added: %s -> %s", route.Network, route.NextHop)
}

// handleRouteDel handles route deletion
func (c *VPNClient) handleRouteDel(data []byte) {
	var route Route
	if err := json.Unmarshal(data, &route); err != nil {
		log.Printf("Failed to parse route: %v", err)
		return
	}

	c.mu.Lock()
	delete(c.routes, route.ID)
	c.mu.Unlock()

	log.Printf("Route deleted: %s", route.ID)
}

// sendKeepalive sends periodic keepalive messages
func (c *VPNClient) sendKeepalive() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for c.running {
		select {
		case <-ticker.C:
			keepaliveMsg := protocol.NewVPNKeepaliveMessage()
			if err := protocol.WriteMessage(c.conn, keepaliveMsg); err != nil {
				log.Printf("Failed to send keepalive: %v", err)
				c.Disconnect()
				return
			}
		}
	}
}

// SendData sends data through the VPN tunnel
func (c *VPNClient) SendData(data []byte) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.running {
		return fmt.Errorf("VPN client is not running")
	}

	// Record start time for latency measurement
	startTime := time.Now()

	// If obfuscation is enabled, obfuscate the packet
	var finalData []byte
	var err error

	if c.obfuscator != nil {
		finalData, err = c.obfuscator.ObfuscatePacket(data, c.cfg.Obfuscation.DefaultType)
		if err != nil {
			if c.performance != nil {
				c.performance.RecordFailure()
			}
			c.stats.IncrementObfuscationErrors()
			return fmt.Errorf("failed to obfuscate packet: %v", err)
		}
	} else {
		finalData = data
	}

	// Create VPN data message
	vpnMsg := protocol.NewVPNDataMessage()
	vpnMsg.Type = protocol.MessageTypeVPNData

	// Encrypt the data
	encrypted, err := c.encryption.Encrypt(finalData)
	if err != nil {
		if c.performance != nil {
			c.performance.RecordFailure()
		}
		c.stats.IncrementEncryptionErrors()
		return fmt.Errorf("failed to encrypt packet: %v", err)
	}
	vpnMsg.Payload = encrypted

	// Send the message
	if err := protocol.WriteMessage(c.conn, vpnMsg); err != nil {
		if c.performance != nil {
			c.performance.RecordFailure()
		}
		return err
	}

	// Update stats and performance metrics
	if c.performance != nil {
		c.performance.RecordSuccess()
		latency := time.Since(startTime)
		c.performance.UpdateStats(len(data), latency)
	}

	c.stats.AddBytesSent(uint64(len(data)))
	c.stats.IncrementPacketsSent()

	return nil
}

// Disconnect disconnects from the VPN server
func (c *VPNClient) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		c.running = false

		// Update stats
		c.stats.DecrementActiveConnections()
		c.stats.DecrementActiveTunnels()

		// Send disconnect message
		if c.conn != nil {
			disconnectMsg := protocol.NewVPNControlMessage()
			disconnectMsg.Type = protocol.MessageTypeVPNControl
			disconnectMsg.Payload = []byte("disconnect")

			protocol.WriteMessage(c.conn, disconnectMsg)
			
			// Return connection to pool if performance optimization is enabled
			if c.cfg.VPN.EnablePerformance && c.performance != nil {
				c.performance.PutConnection(c.conn)
			} else {
				c.conn.Close()
			}
			c.conn = nil
		}

		log.Println("VPN client disconnected")
	}
}

// IsConnected returns the VPN connection status
func (c *VPNClient) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running
}

// GetLocalIP returns the local VPN IP
func (c *VPNClient) GetLocalIP() net.IP {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.localIP
}

// GetRemoteIP returns the remote VPN IP
func (c *VPNClient) GetRemoteIP() net.IP {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.remoteIP
}

// GetRoutes returns the VPN routes
func (c *VPNClient) GetRoutes() map[string]*Route {
	c.mu.RLock()
	defer c.mu.RUnlock()

	routes := make(map[string]*Route)
	for id, route := range c.routes {
		routes[id] = route
	}

	return routes
}

// VPNHandshakeResponse represents VPN handshake response
type VPNHandshakeResponse struct {
	Status   string    `json:"status"`
	Message  string    `json:"message"`
	TunnelID string    `json:"tunnel_id"`
	LocalIP  net.IP    `json:"local_ip"`
	Routes   []Route   `json:"routes"`
}

// VPNControlMessage represents VPN control message
type VPNControlMessage struct {
	Command string `json:"command"`
	Data    []byte `json:"data"`
}

// IPAssignMessage represents IP assignment message
type IPAssignMessage struct {
	IP net.IP `json:"ip"`
}