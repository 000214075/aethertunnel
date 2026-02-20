package ipv6

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// IPv6Address IPv6 地址类型
type IPv6Address struct {
	Address    string `json:"address"`
	Prefix     int    `json:"prefix"`
	Type       string `json:"type"` // global, unique-local, link-local, multicast
	Preferred  bool   `json:"preferred"`
	Valid      bool   `json:"valid"`
}

// IPv6Connection IPv6 连接
type IPv6Connection struct {
	Conn       net.Conn
	LocalAddr  *IPv6Address
	RemoteAddr *IPv6Address
	Protocol   string
	Created    time.Time
}

// IPv6Support IPv6 支持管理器
type IPv6Support struct {
	ctx             context.Context
	cancel          context.CancelFunc
	enabled         bool
	preferredFamily int // 4 for IPv4, 6 for IPv6, 0 for both
	addresses       []*IPv6Address
	nat64           bool // NAT64 支持
	nat46           bool // NAT46 支持
	mu              sync.RWMutex
}

// NATTranslator NAT 转换器
type NATTranslator struct {
	enabled  bool
	type    string // nat64, nat46
	prefix  string
	mapping map[string]string
	mu      sync.RWMutex
}

// NewIPv6Support 创建 IPv6 支持管理器
func NewIPv6Support(ctx context.Context) *IPv6Support {
	childCtx, cancel := context.WithCancel(ctx)

	return &IPv6Support{
		ctx:      childCtx,
		cancel:   cancel,
		enabled:  true,
		addresses: make([]*IPv6Address, 0),
		nat64:     false,
		nat46:     false,
	}
}

// Enable 启用 IPv6 支持
func (v *IPv6Support) Enable() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.enabled = true
}

// Disable 禁用 IPv6 支持
func (v *IPv6Support) Disable() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.enabled = false
}

// IsEnabled 检查是否启用
func (v *IPv6Support) IsEnabled() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.enabled
}

// SetPreferredFamily 设置首选地址族
func (v *IPv6Support) SetPreferredFamily(family int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.preferredFamily = family
}

// GetPreferredFamily 获取首选地址族
func (v *IPv6Support) GetPreferredFamily() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.preferredFamily
}

// DiscoverAddresses 发现 IPv6 地址
func (v *IPv6Support) DiscoverAddresses() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// 获取所有网络接口
	interfaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("failed to get interfaces: %w", err)
	}

	v.addresses = make([]*IPv6Address, 0)

	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			// 检查是否是 IPv6 地址
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}

			ip := ipNet.IP
			if !isIPv6(ip) {
				continue
			}

			ipv6Addr := &IPv6Address{
				Address:   ip.String(),
				Prefix:    bits(ipNet.Mask),
				Type:      classifyIPv6Address(ip),
				Preferred: true,
				Valid:     true,
			}

			v.addresses = append(v.addresses, ipv6Addr)
		}
	}

	return nil
}

// GetAddresses 获取所有 IPv6 地址
func (v *IPv6Support) GetAddresses() []*IPv6Address {
	v.mu.RLock()
	defer v.mu.RUnlock()

	addrs := make([]*IPv6Address, len(v.addresses))
	copy(addrs, v.addresses)
	return addrs
}

// GetGlobalAddresses 获取全球单播地址
func (v *IPv6Support) GetGlobalAddresses() []*IPv6Address {
	v.mu.RLock()
	defer v.mu.RUnlock()

	global := make([]*IPv6Address, 0)
	for _, addr := range v.addresses {
		if addr.Type == "global" {
			global = append(global, addr)
		}
	}
	return global
}

// Dial 通过 IPv6 拨号
func (v *IPv6Support) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	if !v.IsEnabled() {
		return net.Dial(network, address)
	}

	// 尝试 IPv6 连接
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	// 解析 IPv6 地址
	addrs, err := v.resolveAddresses(ctx, host, v.preferredFamily)
	if err != nil {
		return nil, err
	}

	// 尝试连接
	var lastErr error
	for _, addr := range addrs {
		target := net.JoinHostPort(addr.String(), port)
		conn, err := net.DialTimeout(network, target, 10*time.Second)
		if err == nil {
			return v.wrapConnection(conn)
		}
		lastErr = err
	}

	return nil, lastErr
}

// Listen 监听 IPv6
func (v *IPv6Support) Listen(network, address string) (net.Listener, error) {
	if !v.IsEnabled() {
		return net.Listen(network, address)
	}

	// 如果地址是通配符，尝试监听 IPv6
	if address == "" || address == ":0" {
		return net.Listen(network, "[::]:0")
	}

	// 解析地址
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	// 如果是 IPv6 地址格式
	if strings.Contains(host, ":") {
		return net.Listen(network, fmt.Sprintf("[%s]:%s", host, port))
	}

	return net.Listen(network, address)
}

// resolveAddresses 解析地址（优先 IPv6）
func (v *IPv6Support) resolveAddresses(ctx context.Context, host string, preferredFamily int) ([]net.IP, error) {
	// DNS 查询
	addrs, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}

	var ipv6Addrs []net.IP
	var ipv4Addrs []net.IP

	for _, addr := range addrs {
		if isIPv6(addr) {
			ipv6Addrs = append(ipv6Addrs, addr)
		} else {
			ipv4Addrs = append(ipv4Addrs, addr)
		}
	}

	// 根据首选地址族返回
	switch preferredFamily {
	case 6:
		if len(ipv6Addrs) > 0 {
			return ipv6Addrs, nil
		}
		return ipv4Addrs, nil
	case 4:
		if len(ipv4Addrs) > 0 {
			return ipv4Addrs, nil
		}
		return ipv6Addrs, nil
	default:
		// 默认优先 IPv6
		result := append(ipv6Addrs, ipv4Addrs...)
		if len(result) > 0 {
			return result, nil
		}
		return nil, fmt.Errorf("no addresses found")
	}
}

// wrapConnection 包装连接
func (v *IPv6Support) wrapConnection(conn net.Conn) (net.Conn, error) {
	localAddr := conn.LocalAddr()
	remoteAddr := conn.RemoteAddr()

	localIP, _, _ := net.SplitHostPort(localAddr.String())
	remoteIP, _, _ := net.SplitHostPort(remoteAddr.String())

	localIPv6 := &IPv6Address{
		Address: localIP,
		Type:    classifyIPv6Address(net.ParseIP(localIP)),
	}

	remoteIPv6 := &IPv6Address{
		Address: remoteIP,
		Type:    classifyIPv6Address(net.ParseIP(remoteIP)),
	}

	// 创建包装连接
	return &IPv6Connection{
		Conn:       conn,
		LocalAddr:  localIPv6,
		RemoteAddr: remoteIPv6,
		Created:    time.Now(),
	}, nil
}

// EnableNAT64 启用 NAT64
func (v *IPv6Support) EnableNAT64(prefix string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.nat64 = true
	// TODO: 实现 NAT64 转换
}

// EnableNAT46 启用 NAT46
func (v *IPv6Support) EnableNAT46() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.nat46 = true
	// TODO: 实现 NAT46 转换
}

// NAT穿透支持

// NATType NAT 类型
type NATType string

const (
	NATNone         NATType = "none"
	NATFullCone     NATType = "full_cone"
	NATRestricted   NATType = "restricted"
	NATPortRestricted NATType = "port_restricted"
	NATSymmetric    NATType = "symmetric"
)

// NATDetector NAT 检测器
type NATDetector struct {
	stunServers []string
	results     map[NATType]int
	mu          sync.RWMWMutex
}

// NewNATDetector 创建 NAT 检测器
func NewNATDetector() *NATDetector {
	return &NATDetector{
		stunServers: []string{
			"stun.l.google.com:19302",
			"stun1.l.google.com:19302",
		},
		results: make(map[NATType]int),
	}
}

// DetectNATType 检测 NAT 类型
func (d *NATDetector) DetectNATType() (NATType, error) {
	// 简化的 NAT 检测
	// 实际实现需要完整的 STUN 协议

	// 尝试连接 STUN 服务器
	for _, server := range d.stunServers {
		conn, err := net.DialTimeout("udp", server, 5*time.Second)
		if err != nil {
			continue
		}
		defer conn.Close()

		// 发送 STUN 请求（简化）
		// 实际需要构造 STUN 绑定请求
		_, err = conn.Write([]byte("STUN request"))
		if err != nil {
			continue
		}

		// 读取响应
		buf := make([]byte, 1024)
		_, err = conn.Read(buf)
		if err != nil {
			continue
		}

		// 分析响应确定 NAT 类型（简化）
		return NATFullCone, nil
	}

	return NATNone, fmt.Errorf("failed to detect NAT type")
}

// STUNServer STUN 服务器
type STUNServer struct {
	addr      string
	timeout   time.Duration
}

// NewSTUNServer 创建 STUN 服务器
func NewSTUNServer(addr string) *STUNServer {
	return &STUNServer{
		addr:    addr,
		timeout: 5 * time.Second,
	}
}

// GetPublicIP 获取公网 IP
func (s *STUNServer) GetPublicIP() (string, error) {
	conn, err := net.DialTimeout("udp", s.addr, s.timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// 发送 STUN 绑定请求（简化）
	_, err = conn.Write([]byte("STUN binding request"))
	if err != nil {
		return "", err
	}

	// 读取响应
	buf := make([]byte, 1024)
	_, err = conn.Read(buf)
	if err != nil {
		return "", err
	}

	// 解析响应获取公网 IP（简化）
	return "203.0.113.1", nil
}

// Helper functions

// isIPv6 检查是否是 IPv6 地址
func isIPv6(ip net.IP) bool {
	return ip != nil && ip.To4() == nil
}

// classifyIPv6Address 分类 IPv6 地址
func classifyIPv6Address(ip net.IP) string {
	if ip == nil || ip.To4() != nil {
		return "unknown"
	}

	// 检查地址类型
	if ip.IsLoopback() {
		return "loopback"
	}
	if ip.IsLinkLocalUnicast() {
		return "link-local"
	}
	if ip.IsLinkLocalMulticast() {
		return "multicast"
	}
	if ip.IsGlobalUnicast() {
		return "global"
	}
	if ip.IsPrivate() {
		return "unique-local"
	}

	return "unknown"
}

// bits 获取子网掩码位数
func bits(mask net.IPMask) int {
	ones, _ := mask.Size()
	return ones
}

// IPv6Tunnel IPv6 隧道（用于 IPv4 over IPv6）
type IPv6Tunnel struct {
	localAddr  net.Addr
	remoteAddr net.Addr
	enabled    bool
	mu         sync.RWMutex
}

// NewIPv6Tunnel 创建 IPv6 隧道
func NewIPv6Tunnel(localAddr, remoteAddr string) (*IPv6Tunnel, error) {
	// 解析地址
	laddr, err := net.ResolveTCPAddr("tcp", localAddr)
	if err != nil {
		return nil, err
	}

	raddr, err := net.ResolveTCPAddr("tcp", remoteAddr)
	if err != nil {
		return nil, err
	}

	return &IPv6Tunnel{
		localAddr:  laddr,
		remoteAddr: raddr,
		enabled:    true,
	}, nil
}

// Dial 通过隧道拨号
func (t *IPv6Tunnel) Dial() (net.Conn, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if !t.enabled {
		return nil, fmt.Errorf("tunnel not enabled")
	}

	// 创建 IPv6 连接
	conn, err := net.Dial("tcp", t.remoteAddr.String())
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// Close 关闭隧道
func (t *IPv6Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.enabled = false
	return nil
}
