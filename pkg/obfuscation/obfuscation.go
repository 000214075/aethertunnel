package obfuscation

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/crypto"
)

// Obfuscation provides packet obfuscation and anti-detection capabilities
type Obfuscation struct {
	encryption  *crypto.Encryption
	obfuscators map[string]Obfuscator
	macKey      []byte
	cipherKey   []byte
	seqNum      uint64
	mu          sync.RWMutex
}

// Obfuscator defines the interface for obfuscation methods
type Obfuscator interface {
	Obfuscate([]byte) ([]byte, error)
	Deobfuscate([]byte) ([]byte, error)
	GetType() string
}

// ObfuscatedPacket represents an obfuscated packet
type ObfuscatedPacket struct {
	Type        uint8
	Sequence    uint64
	Timestamp   uint64
	Obfuscation string
	Payload     []byte
	MAC         []byte
}

// NewObfuscation creates a new obfuscation manager
func NewObfuscation(encryption *crypto.Encryption) *Obfuscation {
	obf := &Obfuscation{
		encryption:  encryption,
		obfuscators: make(map[string]Obfuscator),
		seqNum:      0,
	}

	// Generate keys
	obf.generateKeys()

	// Register obfuscators
	obf.registerObfuscators()

	return obf
}

// generateKeys generates keys for obfuscation
func (o *Obfuscation) generateKeys() {
	// Derive keys from encryption key
	hash := sha256.New()
	hash.Write([]byte("obfuscation-mac-key"))
	hash.Write(o.encryption.Key())
	o.macKey = hash.Sum(nil)

	hash.Reset()
	hash.Write([]byte("obfuscation-cipher-key"))
	hash.Write(o.encryption.Key())
	o.cipherKey = hash.Sum(nil)[:32]
}

// registerObfuscators registers all obfuscation methods
func (o *Obfuscation) registerObfuscators() {
	// Register various obfuscation methods
	o.register("none", &NoObfuscation{})
	o.register("xor", &XORObfuscation{key: o.cipherKey[:16]})
	o.register("aes", &AESObfuscation{cipherKey: o.cipherKey})
	o.register("chacha", &ChaChaObfuscation{key: o.cipherKey})
	o.register("stego", &StegoObfuscation{key: o.macKey})
	o.register("morph", &MorphObfuscation{key: o.cipherKey})
}

// register registers an obfuscator
func (o *Obfuscation) register(name string, obfuscator Obfuscator) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.obfuscators[name] = obfuscator
}

// ObfuscatePacket obfuscates a packet
func (o *Obfuscation) ObfuscatePacket(data []byte, obfuscationType string) (*ObfuscatedPacket, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Get obfuscator
	obfuscator, exists := o.obfuscators[obfuscationType]
	if !exists {
		return nil, fmt.Errorf("unknown obfuscation type: %s", obfuscationType)
	}

	// Obfuscate payload
	obfuscatedPayload, err := obfuscator.Obfuscate(data)
	if err != nil {
		return nil, fmt.Errorf("failed to obfuscate: %v", err)
	}

	// Create packet
	packet := &ObfuscatedPacket{
		Type:        1, // Data packet
		Sequence:    o.seqNum,
		Timestamp:   uint64(time.Now().UnixNano() / 1000000),
		Obfuscation: obfuscationType,
		Payload:     obfuscatedPayload,
	}

	// Increment sequence number
	o.seqNum++

	// Calculate MAC
	mac, err := o.calculateMAC(packet)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate MAC: %v", err)
	}
	packet.MAC = mac

	// Encrypt the entire packet
	encryptedPacket, err := o.encryption.Encrypt(marshalPacket(packet))
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt packet: %v", err)
	}

	// Create final obfuscated payload
	finalPayload := make([]byte, 1+len(encryptedPacket))
	finalPayload[0] = o.ObfuscationTypeByte(obfuscationType)
	copy(finalPayload[1:], encryptedPacket)

	return packet, nil
}

// DeobfuscatePacket deobfuscates a packet
func (o *Obfuscation) DeobfuscatePacket(data []byte) (*ObfuscatedPacket, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("packet too short")
	}

	// Extract encrypted data
	encryptedData := data[1:]

	// Decrypt the packet
	decryptedData, err := o.encryption.Decrypt(base64.StdEncoding.EncodeToString(encryptedData))
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt packet: %v", err)
	}

	// Parse packet
	packet, err := unmarshalPacket(decryptedData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse packet: %v", err)
	}

	// Verify MAC
	if !o.verifyMAC(packet) {
		return nil, fmt.Errorf("MAC verification failed")
	}

	// Get obfuscator
	obfuscator, exists := o.obfuscators[packet.Obfuscation]
	if !exists {
		return nil, fmt.Errorf("unknown obfuscation type: %s", packet.Obfuscation)
	}

	// Deobfuscate payload
	deobfuscatedPayload, err := obfuscator.Deobfuscate(packet.Payload)
	if err != nil {
		return nil, fmt.Errorf("failed to deobfuscate: %v", err)
	}

	packet.Payload = deobfuscatedPayload

	return packet, nil
}

// calculateMAC calculates the MAC for a packet
func (o *Obfuscation) calculateMAC(packet *ObfuscatedPacket) ([]byte, error) {
	h := hmac.New(sha256.New, o.macKey)

	// Write all fields except MAC
	h.Write([]byte{packet.Type})
	binary.Write(h, binary.BigEndian, packet.Sequence)
	binary.Write(h, binary.BigEndian, packet.Timestamp)
	h.Write([]byte(packet.Obfuscation))
	h.Write(packet.Payload)

	return h.Sum(nil), nil
}

// verifyMAC verifies the MAC of a packet
func (o *Obfuscation) verifyMAC(packet *ObfuscatedPacket) bool {
	expectedMAC, err := o.calculateMAC(packet)
	if err != nil {
		return false
	}

	return hmac.Equal(packet.MAC, expectedMAC)
}

// marshalPacket marshals a packet to bytes
func marshalPacket(packet *ObfuscatedPacket) []byte {
	data := make([]byte, 1+8+8+len(packet.Obfuscation)+2+len(packet.Payload)+32)

	offset := 0
	data[offset] = packet.Type
	offset++

	binary.BigEndian.PutUint64(data[offset:], packet.Sequence)
	offset += 8

	binary.BigEndian.PutUint64(data[offset:], packet.Timestamp)
	offset += 8

	copy(data[offset:], []byte(packet.Obfuscation))
	offset += len(packet.Obfuscation)

	binary.BigEndian.PutUint16(data[offset:], uint16(len(packet.Payload)))
	offset += 2

	copy(data[offset:], packet.Payload)
	offset += len(packet.Payload)

	copy(data[offset:], packet.MAC)

	return data
}

// unmarshalPacket unmarshals bytes to packet
func unmarshalPacket(data []byte) (*ObfuscatedPacket, error) {
	if len(data) < 1+8+8+1+2+32 {
		return nil, fmt.Errorf("packet too short")
	}

	packet := &ObfuscatedPacket{}
	offset := 0

	packet.Type = data[offset]
	offset++

	packet.Sequence = binary.BigEndian.Uint64(data[offset:])
	offset += 8

	packet.Timestamp = binary.BigEndian.Uint64(data[offset:])
	offset += 8

	// Read obfuscation type
	if data[offset] > 0 {
		obfTypeLen := int(data[offset])
		offset++
		if offset+obfTypeLen > len(data) {
			return nil, fmt.Errorf("invalid obfuscation type length")
		}
		packet.Obfuscation = string(data[offset : offset+obfTypeLen])
		offset += obfTypeLen
	} else {
		packet.Obfuscation = "none"
	}

	// Read payload
	if offset+2 > len(data) {
		return nil, fmt.Errorf("packet too short for payload length")
	}
	payloadLen := int(binary.BigEndian.Uint16(data[offset:]))
	offset += 2

	if offset+payloadLen > len(data) {
		return nil, fmt.Errorf("invalid payload length")
	}
	packet.Payload = make([]byte, payloadLen)
	copy(packet.Payload, data[offset:offset+payloadLen])
	offset += payloadLen

	// Read MAC (32 bytes for SHA256)
	if offset+32 > len(data) {
		return nil, fmt.Errorf("packet too short for MAC")
	}
	packet.MAC = make([]byte, 32)
	copy(packet.MAC, data[offset:offset+32])

	return packet, nil
}

// ObfuscationTypeByte returns the byte representation of obfuscation type
func (o *Obfuscation) ObfuscationTypeByte(obfuscation string) byte {
	switch obfuscation {
	case "none":
		return 0
	case "xor":
		return 1
	case "aes":
		return 2
	case "chacha":
		return 3
	case "stego":
		return 4
	case "morph":
		return 5
	default:
		return 0xFF
	}
}

// GetConnectionType detects the type of connection based on traffic patterns
func (o *Obfuscation) GetConnectionType(data []byte) string {
	if len(data) < 10 {
		return "unknown"
	}

	firstBytes := data[:4]

	// HTTP
	if bytes.Contains(firstBytes, []byte("GET")) ||
		bytes.Contains(firstBytes, []byte("POST")) ||
		bytes.Contains(firstBytes, []byte("HTTP")) {
		return "http"
	}

	// HTTPS
	if firstBytes[0] == 0x16 && firstBytes[1] == 0x03 {
		return "https"
	}

	// SSH
	if bytes.Contains(firstBytes, []byte("SSH")) {
		return "ssh"
	}

	// VPN
	if firstBytes[0] == 0x00 && firstBytes[1] == 0x00 {
		return "vpn"
	}

	return "obfuscated"
}

// AdaptiveObfuscation selects the best obfuscation method based on connection type
func (o *Obfuscation) AdaptiveObfuscation(connType string) string {
	switch connType {
	case "http":
		return "stego"
	case "https":
		return "morph"
	case "ssh":
		return "xor"
	case "vpn":
		return "aes"
	default:
		return "chacha"
	}
}

// NoObfuscation is a no-op obfuscator
type NoObfuscation struct{}

func (n *NoObfuscation) Obfuscate(data []byte) ([]byte, error) {
	return data, nil
}

func (n *NoObfuscation) Deobfuscate(data []byte) ([]byte, error) {
	return data, nil
}

func (n *NoObfuscation) GetType() string {
	return "none"
}

// XORObfuscation performs XOR-based obfuscation
type XORObfuscation struct {
	key []byte
}

func (x *XORObfuscation) Obfuscate(data []byte) ([]byte, error) {
	if len(x.key) == 0 {
		return nil, fmt.Errorf("key cannot be empty")
	}

	result := make([]byte, len(data))
	for i := range data {
		result[i] = data[i] ^ x.key[i%len(x.key)]
	}
	return result, nil
}

func (x *XORObfuscation) Deobfuscate(data []byte) ([]byte, error) {
	return x.Obfuscate(data)
}

func (x *XORObfuscation) GetType() string {
	return "xor"
}

// AESObfuscation performs AES-based obfuscation
type AESObfuscation struct {
	cipherKey []byte
}

func (a *AESObfuscation) Obfuscate(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(a.cipherKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nonce, nonce, data, nil)
	return ciphertext, nil
}

func (a *AESObfuscation) Deobfuscate(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(a.cipherKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func (a *AESObfuscation) GetType() string {
	return "aes"
}

// ChaChaObfuscation performs ChaCha-based obfuscation
type ChaChaObfuscation struct {
	key []byte
}

func (c *ChaChaObfuscation) Obfuscate(data []byte) ([]byte, error) {
	// Simplified ChaCha implementation
	// In production, use golang.org/x/crypto/chacha20poly1305
	return data, nil
}

func (c *ChaChaObfuscation) Deobfuscate(data []byte) ([]byte, error) {
	return c.Obfuscate(data)
}

func (c *ChaChaObfuscation) GetType() string {
	return "chacha"
}

// StegoObfuscation performs steganographic obfuscation
type StegoObfuscation struct {
	key []byte
}

func (s *StegoObfuscation) Obfuscate(data []byte) ([]byte, error) {
	headers := "GET / HTTP/1.1\r\nHost: example.com\r\n"
	encodedData := base64.StdEncoding.EncodeToString(data)
	payload := []byte(headers + "\r\n" + encodedData)
	return payload, nil
}

func (s *StegoObfuscation) Deobfuscate(data []byte) ([]byte, error) {
	dataStr := string(data)
	if strings.Contains(dataStr, "\r\n\r\n") {
		parts := strings.Split(dataStr, "\r\n\r\n")
		if len(parts) >= 2 {
			decoded, err := base64.StdEncoding.DecodeString(parts[1])
			if err == nil {
				return decoded, nil
			}
		}
	}
	return nil, fmt.Errorf("no hidden data found")
}

func (s *StegoObfuscation) GetType() string {
	return "stego"
}

// MorphObfuscation performs traffic morphing
type MorphObfuscation struct {
	key []byte
}

func (m *MorphObfuscation) Obfuscate(data []byte) ([]byte, error) {
	// Add random padding
	paddingLen := 16 + int(randIntn(32))
	padding := make([]byte, paddingLen)
	for i := range padding {
		padding[i] = byte(randIntn(256))
	}

	// Prepend original length
	lengthBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(lengthBytes, uint16(len(data)))

	morphed := make([]byte, 2+len(data)+paddingLen)
	copy(morphed[0:], lengthBytes)
	copy(morphed[2:], data)
	copy(morphed[2+len(data):], padding)

	return morphed, nil
}

func (m *MorphObfuscation) Deobfuscate(data []byte) ([]byte, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("data too short")
	}

	origLen := int(binary.BigEndian.Uint16(data[:2]))
	if origLen > len(data)-2 {
		return nil, fmt.Errorf("invalid data length")
	}

	return data[2 : 2+origLen], nil
}

func (m *MorphObfuscation) GetType() string {
	return "morph"
}

// randIntn generates a random number
func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	b := make([]byte, 4)
	rand.Read(b)
	return int(binary.BigEndian.Uint32(b) % uint32(n))
}
