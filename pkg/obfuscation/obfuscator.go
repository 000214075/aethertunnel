package obfuscation

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"
)

// Obfuscator 流量混淆器接口
type Obfuscator interface {
	Obfuscate(data []byte) ([]byte, error)
	Deobfuscate(data []byte) ([]byte, error)
	GetType() string
}

// TLSObfuscator TLS 流量伪装
// 让隧道流量看起来像 HTTPS 流量
type TLSObfuscator struct {
	targetHost   string // 伪装的目标主机
	targetConfig *tls.Config
	obfuscateKey []byte
}

// HTTPObfuscator HTTP 流量伪装
// 让隧道流量看起来像 HTTP 流量
type HTTPObfuscator struct {
	fakeHost     string
	fakePath     string
	obfuscateKey []byte
}

// XORObfuscator 简单的 XOR 混淆
type XORObfuscator struct {
	key []byte
}

// NewTLSObfuscator 创建 TLS 混淆器
func NewTLSObfuscator(targetHost string, obfuscateKey []byte) (*TLSObfuscator, error) {
	config := &tls.Config{
		ServerName:         targetHost,
		InsecureSkipVerify: true, // 用于混淆，实际场景需要谨慎
		MinVersion:         tls.VersionTLS12,
	}

	return &TLSObfuscator{
		targetHost:   targetHost,
		targetConfig: config,
		obfuscateKey: obfuscateKey,
	}, nil
}

// Obfuscate TLS 混淆
// 在真实 TLS 数据包之间插入伪造的 TLS 握手
func (t *TLSObfuscator) Obfuscate(data []byte) ([]byte, error) {
	// 构造伪造的 TLS ClientHello
	fakeClientHello := t.buildFakeClientHello()

	// 将真实数据与伪造数据混合
	obfuscated := make([]byte, 0, len(fakeClientHello)+len(data))
	obfuscated = append(obfuscated, fakeClientHello...)
	obfuscated = append(obfuscated, data...)

	// 使用 XOR 混淆
	return t.xorObfuscate(obfuscated)
}

// Deobfuscate TLS 解混淆
func (t *TLSObfuscator) Deobfuscate(data []byte) ([]byte, error) {
	// 先 XOR 解混淆
	data, err := t.xorObfuscate(data)
	if err != nil {
		return nil, err
	}

	// 跳过伪造的 TLS ClientHello
	// TLS ClientHello 格式：
	// 0x16 (Handshake type)
	// 0x03 0x01 (TLS 1.0 version for compatibility)
	// length (2 bytes)
	if len(data) > 5 && data[0] == 0x16 && data[1] == 0x03 {
		// 找到真实数据的起始位置
		tlsLength := binary.BigEndian.Uint16(data[3:5])
		startPos := int(5 + tlsLength)
		if startPos < len(data) {
			return data[startPos:], nil
		}
	}

	// 如果没有找到伪造数据，直接返回
	return data, nil
}

// buildFakeClientHello 构造伪造的 TLS ClientHello
func (t *TLSObfuscator) buildFakeClientHello() []byte {
	// 简化的 TLS ClientHello 构造
	// 实际实现应该构造更真实的握手包

	clientHello := []byte{
		0x16,                         // Content Type: Handshake
		0x03, 0x01,                   // TLS Version: 1.0 (for compatibility)
		0x00, 0x2b,                   // Length
		0x01,                         // Handshake Type: ClientHello
		0x00, 0x00, 0x27,             // Length
		0x03, 0x03,                   // TLS Version: 1.2
	}

	// 随机 Session ID
	rand.Seed(time.Now().UnixNano())
	sessionID := make([]byte, 32)
	rand.Read(sessionID)
	clientHello = append(clientHello, byte(len(sessionID)))
	clientHello = append(clientHello, sessionID...)

	// Cipher Suites (常见套件列表）
	cipherSuites := []uint16{
		0x1301, // TLS_AES_128_GCM_SHA256
		0x1302, // TLS_AES_256_GCM_SHA384
		0x1303, // TLS_CHACHA20_POLY1305_SHA256
		0xc02b, // TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
		0xc02f, // TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
	}

	clientHello = append(clientHello, 0x00, byte(len(cipherSuites)*2))
	for _, suite := range cipherSuites {
		clientHello = append(clientHello, byte(suite>>8), byte(suite))
	}

	// Compression Methods
	clientHello = append(clientHello, 0x01, 0x00) // Only null compression

	// Extensions
	extensions := t.buildExtensions()
	clientHello = append(clientHello, extensions...)

	return clientHello
}

// buildExtensions 构造 TLS 扩展
func (t *TLSObfuscator) buildExtensions() []byte {
	// Server Name Indication (SNI）
	sni := []byte{
		0x00, 0x00,             // Extension Type: SNI
		0x00, byte(len(t.targetHost) + 5),
		0x00, byte(len(t.targetHost) + 3),
		0x00,
		byte(len(t.targetHost)),
	}
	sni = append(sni, []byte(t.targetHost)...)

	return sni
}

// xorObfuscate XOR 混淆
func (t *TLSObfuscator) xorObfuscate(data []byte) ([]byte, error) {
	if len(t.obfuscateKey) == 0 {
		return data, nil
	}

	result := make([]byte, len(data))
	keyLen := len(t.obfuscateKey)
	for i := range data {
		result[i] = data[i] ^ t.obfuscateKey[i%keyLen]
	}

	return result, nil
}

// NewHTTPObfuscator 创建 HTTP 混淆器
func NewHTTPObfuscator(fakeHost, fakePath string, obfuscateKey []byte) *HTTPObfuscator {
	return &HTTPObfuscator{
		fakeHost:     fakeHost,
		fakePath:     fakePath,
		obfuscateKey: obfuscateKey,
	}
}

// Obfuscate HTTP 混淆
func (h *HTTPObfuscator) Obfuscate(data []byte) ([]byte, error) {
	// 构造伪造的 HTTP 请求
	fakeRequest := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0\r\n\r\n",
		h.fakePath,
		h.fakeHost,
	)

	// 将真实数据编码到伪造请求中
	encodedData := base64Encode(data)
	obfuscated := []byte(fakeRequest)
	obfuscated = append(obfuscated, encodedData...)

	// 添加混淆
	return h.xorObfuscate(obfuscated), nil
}

// Deobfuscate HTTP 解混淆
func (h *HTTPObfuscator) Deobfuscate(data []byte) ([]byte, error) {
	// 先解混淆
	data, err := h.xorObfuscate(data)
	if err != nil {
		return nil, err
	}

	// 查找真实数据（跳过 HTTP 头）
	// 查找双换行符分隔符
	separator := []byte("\r\n\r\n")
	idx := bytes.Index(data, separator)
	if idx >= 0 {
		// 提取 Base64 编码的数据
		encodedData := data[idx+4:]
		return base64Decode(encodedData), nil
	}

	return data, nil
}

// xorObfuscate HTTP XOR 混淆
func (h *HTTPObfuscator) xorObfuscate(data []byte) ([]byte, error) {
	if len(h.obfuscateKey) == 0 {
		return data, nil
	}

	result := make([]byte, len(data))
	keyLen := len(h.obfuscateKey)
	for i := range data {
		result[i] = data[i] ^ h.obfuscateKey[i%keyLen]
	}

	return result, nil
}

// NewXORObfuscator 创建 XOR 混淆器
func NewXORObfuscator(key []byte) *XORObfuscator {
	return &XORObfuscator{key: key}
}

// Obfuscate XOR 混淆
func (x *XORObfuscator) Obfuscate(data []byte) ([]byte, error) {
	if len(x.key) == 0 {
		return data, nil
	}

	result := make([]byte, len(data))
	keyLen := len(x.key)
	for i := range data {
		result[i] = data[i] ^ x.key[i%keyLen]
	}

	return result, nil
}

// Deobfuscate XOR 解混淆
func (x *XORObfuscator) Deobfuscate(data []byte) ([]byte, error) {
	return x.Obfuscate(data)
}

// GetType 获取混淆器类型
func (t *TLSObfuscator) GetType() string {
	return "tls"
}

func (h *HTTPObfuscator) GetType() string {
	return "http"
}

func (x *XORObfuscator) GetType() string {
	return "xor"
}

// ObfuscationStream 混淆流
type ObfuscationStream struct {
	reader     io.Reader
	writer     io.Writer
	obfuscator Obfuscator
}

// NewObfuscationStream 创建混淆流
func NewObfuscationStream(reader io.Reader, writer io.Writer, obfuscator Obfuscator) *ObfuscationStream {
	return &ObfuscationStream{
		reader:     reader,
		writer:     writer,
		obfuscator: obfuscator,
	}
}

// Read 读取并解混淆数据
func (s *ObfuscationStream) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if err != nil {
		return n, err
	}

	data := p[:n]
	deobfuscated, err := s.obfuscator.Deobfuscate(data)
	if err != nil {
		return 0, err
	}

	copy(p, deobfuscated)
	return len(deobfuscated), nil
}

// Write 混淆并写入数据
func (s *ObfuscationStream) Write(p []byte) (int, error) {
	obfuscated, err := s.obfuscator.Obfuscate(p)
	if err != nil {
		return 0, err
	}

	return s.writer.Write(obfuscated)
}

// TrafficObfuscatorWrapper 包装 net.Conn
type TrafficObfuscatorWrapper struct {
	net.Conn
	obfuscator Obfuscator
}

// NewTrafficObfuscatorWrapper 创建连接包装器
func NewTrafficObfuscatorWrapper(conn net.Conn, obfuscator Obfuscator) *TrafficObfuscatorWrapper {
	return &TrafficObfuscatorWrapper{
		Conn:       conn,
		obfuscator: obfuscator,
	}
}

// Read 读取并解混淆
func (w *TrafficObfuscatorWrapper) Read(b []byte) (int, error) {
	n, err := w.Conn.Read(b)
	if err != nil {
		return n, err
	}

	data := b[:n]
	deobfuscated, err := w.obfuscator.Deobfuscate(data)
	if err != nil {
		return 0, err
	}

	copy(b, deobfuscated)
	return len(deobfuscated), nil
}

// Write 混淆并写入
func (w *TrafficObfuscatorWrapper) Write(b []byte) (int, error) {
	obfuscated, err := w.obfuscator.Obfuscate(b)
	if err != nil {
		return 0, err
	}

	return w.Conn.Write(obfuscated)
}

// Common伪装目标
const (
	TargetGoogle    = "www.google.com"
	TargetYouTube   = "www.youtube.com"
	TargetNetflix   = "www.netflix.com"
	TargetFacebook  = "www.facebook.com"
	TargetTwitter   = "www.twitter.com"
	TargetAmazon    = "www.amazon.com"
	TargetWikipedia = "www.wikipedia.org"
)

// CreateRecommendedObfuscator 创建推荐的混淆器
func CreateRecommendedObfuscator(obfuscationType, target string, key []byte) (Obfuscator, error) {
	switch strings.ToLower(obfuscationType) {
	case "tls":
		if target == "" {
			target = TargetGoogle
		}
		return NewTLSObfuscator(target, key)
	case "http":
		if target == "" {
			target = "www.example.com"
		}
		fakePath := "/index.html"
		return NewHTTPObfuscator(target, fakePath, key), nil
	case "xor":
		return NewXORObfuscator(key), nil
	default:
		return NewXORObfuscator(key), nil
	}
}

// Helper functions

func base64Encode(data []byte) []byte {
	// 简化的 Base64 编码
	// 实际实现应该使用 encoding/base64
	return []byte(fmt.Sprintf("BASE64:%x", data))
}

func base64Decode(data []byte) []byte {
	// 简化的 Base64 解码
	str := string(data)
	if strings.HasPrefix(str, "BASE64:") {
		hexStr := str[7:]
		// 解析十六进制（简化）
		return []byte(hexStr)
	}
	return data
}

// HTTP2Obfuscator HTTP/2 流量伪装（更高级）
type HTTP2Obfuscator struct {
	fakeHost     string
	obfuscateKey []byte
}

// NewHTTP2Obfuscator 创建 HTTP/2 混淆器
func NewHTTP2Obfuscator(fakeHost string, obfuscateKey []byte) *HTTP2Obfuscator {
	return &HTTP2Obfuscator{
		fakeHost:     fakeHost,
		obfuscateKey: obfuscateKey,
	}
}

// Obfuscate HTTP/2 混淆
func (h *HTTP2Obfuscator) Obfuscate(data []byte) ([]byte, error) {
	// 构造 HTTP/2 连接前言
	// HTTP/2 连接前言： "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	clientPreface := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

	// 将真实数据编码到 SETTINGS 帧
	// 这只是示意，实际需要完整的 HTTP/2 帧格式
	obfuscated := make([]byte, 0, len(clientPreface)+len(data)+10)
	obfuscated = append(obfuscated, clientPreface...)
	obfuscated = append(obfuscated, data...)

	// 使用混淆
	return h.xorObfuscate(obfuscated), nil
}

// Deobfuscate HTTP/2 解混淆
func (h *HTTP2Obfuscator) Deobfuscate(data []byte) ([]byte, error) {
	data, err := h.xorObfuscate(data)
	if err != nil {
		return nil, err
	}

	// 跳过 HTTP/2 连接前言
	clientPreface := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	if bytes.HasPrefix(data, clientPreface) {
		return data[len(clientPreface):], nil
	}

	return data, nil
}

// xorObfuscate HTTP/2 XOR 混淆
func (h *HTTP2Obfuscator) xorObfuscate(data []byte) ([]byte, error) {
	if len(h.obfuscateKey) == 0 {
		return data, nil
	}

	result := make([]byte, len(data))
	keyLen := len(h.obfuscateKey)
	for i := range data {
		result[i] = data[i] ^ h.obfuscateKey[i%keyLen]
	}

	return result, nil
}

// GetType 获取类型
func (h *HTTP2Obfuscator) GetType() string {
	return "http2"
}
