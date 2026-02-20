package crypto

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// NonceSize is the size of the nonce for ChaCha20-Poly1305
	NonceSize = chacha20poly1305.NonceSize
	// KeySize is the size of the key for ChaCha20-Poly1305
	KeySize = chacha20poly1305.KeySize
	// Overhead is the size of the authentication tag
	Overhead = chacha20poly1305.Overhead
)

var (
	ErrInvalidKey      = errors.New("invalid key size")
	ErrInvalidNonce    = errors.New("invalid nonce size")
	ErrDecryptionFailed = errors.New("decryption failed")
)

// AEADCipher 封装 AEAD 加密操作
type AEADCipher struct {
	aead cipher.AEAD
}

// NewAEADCipher 创建新的 AEAD 密码器
// 使用 HKDF 从主密钥派生加密密钥
func NewAEADCipher(masterKey []byte, salt []byte, info []byte) (*AEADCipher, error) {
	if len(masterKey) < 16 {
		return nil, ErrInvalidKey
	}

	// 使用 HKDF 派生密钥
	hkdf := hkdf.New(sha256.New, masterKey, salt, info)
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(hkdf, key); err != nil {
		return nil, fmt.Errorf("failed to derive key: %w", err)
	}

	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	return &AEADCipher{aead: aead}, nil
}

// NewAEADCipherFromKey 直接从密钥创建
func NewAEADCipherFromKey(key []byte) (*AEADCipher, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKey
	}

	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	return &AEADCipher{aead: aead}, nil
}

// Encrypt 加密数据
func (c *AEADCipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := c.aead.Seal(nil, nonce, plaintext, nil)
	return append(nonce, ciphertext...), nil
}

// Decrypt 解密数据
func (c *AEADCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < NonceSize {
		return nil, ErrInvalidNonce
	}

	nonce := ciphertext[:NonceSize]
	ciphertext = ciphertext[NonceSize:]

	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}

	return plaintext, nil
}

// EncryptWithAdditionalData 使用额外数据加密
func (c *AEADCipher) EncryptWithAdditionalData(plaintext, additionalData []byte) ([]byte, error) {
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := c.aead.Seal(nil, nonce, plaintext, additionalData)
	return append(nonce, ciphertext...), nil
}

// DecryptWithAdditionalData 使用额外数据解密
func (c *AEADCipher) DecryptWithAdditionalData(ciphertext, additionalData []byte) ([]byte, error) {
	if len(ciphertext) < NonceSize {
		return nil, ErrInvalidNonce
	}

	nonce := ciphertext[:NonceSize]
	ciphertext = ciphertext[NonceSize:]

	plaintext, err := c.aead.Open(nil, nonce, ciphertext, additionalData)
	if err != nil {
		return nil, ErrDecryptionFailed
	}

	return plaintext, nil
}

// GenerateRandomKey 生成随机密钥
func GenerateRandomKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate key: %w", err)
	}
	return key, nil
}

// DeriveKey 派生密钥
func DeriveKey(masterKey, salt, info []byte) []byte {
	hkdf := hkdf.New(sha256.New, masterKey, salt, info)
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(hkdf, key); err != nil {
		panic(err) // 如果 HKDF 失败，应该是编程错误
	}
	return key
}

// ConstantTimeCompare 常量时间比较，用于防止时序攻击
func ConstantTimeCompare(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// SecureEqual 安全比较两个字符串
func SecureEqual(a, b string) bool {
	return ConstantTimeCompare([]byte(a), []byte(b))
}
