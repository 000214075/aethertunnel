package crypto

import (
    "crypto/rand"
    "encoding/base64"
    "fmt"

    "golang.org/x/crypto/chacha20poly1305"
)

// Encryption 加密结构
type Encryption struct {
    key []byte
}

// NewEncryption 创建加密对象
func NewEncryption(key string) *Encryption {
    return &Encryption{
        key: []byte(key),
    }
}

// Encrypt 加密数据
func (e *Encryption) Encrypt(plaintext []byte) (string, error) {
    // 生成随机 nonce
    nonce := make([]byte, chacha20poly1305.NonceSize)
    if _, err := rand.Read(nonce); err != nil {
        return "", err
    }

    // 使用 ChaCha20-Poly1305 加密
    aead, err := chacha20poly1305.NewX(e.key, nonce)
    if err != nil {
        return "", err
    }

    ciphertext := aead.Seal(nil, nil, plaintext)
    
    // 将 nonce 和 ciphertext 组合
    result := make([]byte, len(nonce)+len(ciphertext))
    copy(result, nonce)
    copy(result[len(nonce):], ciphertext)
    
    return base64.StdEncoding.EncodeToString(result), nil
}

// Decrypt 解密数据
func (e *Encryption) Decrypt(encrypted string) ([]byte, error) {
    // Base64 解码
    data, err := base64.StdEncoding.DecodeString(encrypted)
    if err != nil {
        return nil, err
    }
    
    // 验证长度
    if len(data) < chacha20poly1305.NonceSize {
        return nil, fmt.Errorf("invalid encrypted data length")
    }
    
    // 分离 nonce 和 ciphertext
    nonce := data[:chacha20poly1305.NonceSize]
    ciphertext := data[chacha20poly1305.NonceSize:]
    
    // 使用 ChaCha20-Poly1305 解密
    aead, err := chacha20poly1305.NewX(e.key, nonce)
    if err != nil {
        return nil, err
    }
    
    plaintext, err := aead.Open(nil, nil, ciphertext)
    if err != nil {
        return nil, err
    }
    
    return plaintext, nil
}

// EncryptBase64 加密并 Base64 编码
func (e *Encryption) EncryptBase64(plaintext string) (string, error) {
    data := []byte(plaintext)
    encrypted, err := e.Encrypt(data)
    if err != nil {
        return "", err
    }

    return encrypted, nil
}

// DecryptBase64 Base64 解码并解密
func (e *Encryption) DecryptBase64(encrypted string) (string, error) {
    data, err := e.Decrypt(encrypted)
    if err != nil {
        return "", err
    }

    return string(data), nil
}
