package crypto

import (
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidSignature = errors.New("invalid signature")
	ErrInvalidPublicKey  = errors.New("invalid public key")
	ErrInvalidPrivateKey = errors.New("invalid private key")
)

// Ed25519Signer Ed25519 签名器
type Ed25519Signer struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

// NewEd25519Signer 从私钥创建签名器
func NewEd25519Signer(privateKey []byte) (*Ed25519Signer, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrInvalidPrivateKey
	}
	key := ed25519.PrivateKey(privateKey)
	return &Ed25519Signer{
		privateKey: key,
		publicKey:  key.Public().(ed25519.PublicKey),
	}, nil
}

// NewEd25519SignerFromKeyPair 从密钥对创建签名器
func NewEd25519SignerFromKeyPair(privateKey, publicKey []byte) (*Ed25519Signer, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrInvalidPrivateKey
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrInvalidPublicKey
	}
	return &Ed25519Signer{
		privateKey: ed25519.PrivateKey(privateKey),
		publicKey:  ed25519.PublicKey(publicKey),
	}, nil
}

// GenerateEd25519KeyPair 生成 Ed25519 密钥对
func GenerateEd25519KeyPair() ([]byte, []byte, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate key pair: %w", err)
	}
	return privateKey, publicKey, nil
}

// Sign 签名数据
func (s *Ed25519Signer) Sign(data []byte) []byte {
	return ed25519.Sign(s.privateKey, data)
}

// SignString 签名字符串
func (s *Ed25519Signer) SignString(str string) []byte {
	return s.Sign([]byte(str))
}

// SignTimestamp 签名时间戳
func (s *Ed25519Signer) SignTimestamp(timestamp int64) []byte {
	data := []byte(fmt.Sprintf("%d", timestamp))
	return s.Sign(data)
}

// Verify 验证签名
func (s *Ed25519Signer) Verify(message, signature []byte) bool {
	return ed25519.Verify(s.publicKey, message, signature)
}

// VerifyTimestamp 验证时间戳签名
func (s *Ed25519Signer) VerifyTimestamp(timestamp int64, signature []byte) bool {
	data := []byte(fmt.Sprintf("%d", timestamp))
	return s.Verify(data, signature)
}

// GetPublicKey 获取公钥
func (s *Ed25519Signer) GetPublicKey() []byte {
	return []byte(s.publicKey)
}

// GetPrivateKey 获取私钥
func (s *Ed25519Signer) GetPrivateKey() []byte {
	return []byte(s.privateKey)
}

// Ed25519Verifier Ed25519 验证器
type Ed25519Verifier struct {
	publicKey ed25519.PublicKey
}

// NewEd25519Verifier 从公钥创建验证器
func NewEd25519Verifier(publicKey []byte) (*Ed25519Verifier, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrInvalidPublicKey
	}
	return &Ed25519Verifier{
		publicKey: ed25519.PublicKey(publicKey),
	}, nil
}

// Verify 验证签名
func (v *Ed25519Verifier) Verify(message, signature []byte) bool {
	return ed25519.Verify(v.publicKey, message, signature)
}

// VerifyTimestamp 验证时间戳签名
func (v *Ed25519Verifier) VerifyTimestamp(timestamp int64, signature []byte) bool {
	data := []byte(fmt.Sprintf("%d", timestamp))
	return v.Verify(data, signature)
}

// VerifyTimestampWithGrace 验证时间戳签名（允许一定时间差）
func (v *Ed25519Verifier) VerifyTimestampWithGrace(timestamp, serverTime int64, gracePeriod time.Duration, signature []byte) bool {
	// 检查时间戳是否在允许范围内
	if timestamp > serverTime+int64(gracePeriod.Seconds()) {
		return false
	}
	if timestamp < serverTime-int64(gracePeriod.Seconds()) {
		return false
	}

	data := []byte(fmt.Sprintf("%d", timestamp))
	return v.Verify(data, signature)
}

// HMACSigner HMAC 签名器（用于心跳等轻量级场景）
type HMACSigner struct {
	key []byte
}

// NewHMACSigner 创建 HMAC 签名器
func NewHMACSigner(key []byte) *HMACSigner {
	return &HMACSigner{key: key}
}

// Sign 签名数据
func (s *HMACSigner) Sign(data []byte) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write(data)
	return mac.Sum(nil)
}

// SignString 签名字符串
func (s *HMACSigner) SignString(str string) []byte {
	return s.Sign([]byte(str))
}

// SignTimestamp 签名时间戳
func (s *HMACSigner) SignTimestamp(timestamp int64) []byte {
	data := []byte(fmt.Sprintf("%d", timestamp))
	return s.Sign(data)
}

// Verify 验证签名
func (s *HMACSigner) Verify(data, signature []byte) bool {
	expected := s.Sign(data)
	return ConstantTimeCompare(expected, signature)
}

// VerifyTimestamp 验证时间戳签名
func (s *HMACSigner) VerifyTimestamp(timestamp int64, signature []byte) bool {
	data := []byte(fmt.Sprintf("%d", timestamp))
	return s.Verify(data, signature)
}

// Hash 哈希函数
func Hash(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

// HashString 哈希字符串
func HashString(str string) []byte {
	return Hash([]byte(str))
}

// ComputeSessionKey 计算会话密钥
// 使用 Diffie-Hellman 风格的密钥派生
func ComputeSessionKey(privateKey, publicKey []byte) []byte {
	// 这里我们使用简单的 XOR + Hash 作为示例
	// 实际生产环境应使用 ECDH
	hashed := Hash(privateKey)
	for i := range publicKey {
		hashed[i] ^= publicKey[i]
	}
	return DeriveKey(hashed, []byte("session"), []byte("aethertunnel"))
}

// GetKeyHash 获取密钥的哈希值（用于密钥标识）
func GetKeyHash(key []byte) string {
	return fmt.Sprintf("%x", Hash(key))
}

// KeyFromPassword 从密码派生密钥
func KeyFromPassword(password, salt []byte) []byte {
	h := hmac.New(sha256.New, password)
	h.Write(salt)
	result := h.Sum(nil)

	// 多次迭代增强安全性
	for i := 0; i < 10000; i++ {
		h = hmac.New(sha256.New, result)
		h.Write(salt)
		result = h.Sum(nil)
	}

	return result
}
