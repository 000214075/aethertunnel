package protocol

import (
    "encoding/binary"
    "errors"
    "fmt"
    "io"
)

// MessageType 消息类型
type MessageType uint32

const (
    MessageTypeAuth    MessageType = 1 // 认证
    MessageTypeHeartbeat MessageType = 2 // 心跳
    MessageTypeProxy    MessageType = 3 // 代理数据
    MessageTypeData    MessageType = 4 // 数据传输
    MessageTypeError    MessageType = 5 // 错误
)

// Message 消息结构
type Message struct {
    Type    MessageType
    Payload []byte
}

// WriteMessage 写入消息
func WriteMessage(conn io.Writer, msg *Message) error {
    // 写入消息类型（4 字节，大端）
    if err := binary.Write(conn, binary.BigEndian, msg.Type); err != nil {
        return fmt.Errorf("failed to write message type: %w", err)
    }

    // 写入消息长度（4 字节，大端）
    payloadLen := uint32(len(msg.Payload))
    if err := binary.Write(conn, binary.BigEndian, payloadLen); err != nil {
        return fmt.Errorf("failed to write message length: %w", err)
    }

    // 写入消息内容
    if _, err := conn.Write(msg.Payload); err != nil {
        return fmt.Errorf("failed to write message payload: %w", err)
    }

    return nil
}

// ReadMessage 读取消息
func ReadMessage(conn io.Reader) (*Message, error) {
    // 读取消息类型
    var msgType MessageType
    if err := binary.Read(conn, binary.BigEndian, &msgType); err != nil {
        return nil, fmt.Errorf("failed to read message type: %w", err)
    }

    // 读取消息长度
    var payloadLen uint32
    if err := binary.Read(conn, binary.BigEndian, &payloadLen); err != nil {
        return nil, fmt.Errorf("failed to read message length: %w", err)
    }

    // 验证消息长度
    if payloadLen > 10*1024*1024 { // 限制 10MB
        return nil, errors.New("message payload too large")
    }
    if payloadLen == 0 {
        return nil, errors.New("message payload is empty")
    }

    // 读取消息内容
    payload := make([]byte, payloadLen)
    if _, err := io.ReadFull(conn, payload); err != nil {
        return nil, fmt.Errorf("failed to read message payload: %w", err)
    }

    return &Message{
        Type:    msgType,
        Payload: payload,
    }, nil
}

// NewAuthMessage 创建认证消息
func NewAuthMessage(token string) *Message {
    return &Message{
        Type:    MessageTypeAuth,
        Payload: []byte(token),
    }
}

// NewProxyMessage 创建代理消息
func NewProxyMessage(data []byte) *Message {
    return &Message{
        Type:    MessageTypeProxy,
        Payload: data,
    }
}

// NewHeartbeatMessage 创建心跳消息
func NewHeartbeatMessage() *Message {
    return &Message{
        Type:    MessageTypeHeartbeat,
        Payload: []byte{},
    }
}

// NewErrorMessage 创建错误消息
func NewErrorMessage(err string) *Message {
    return &Message{
        Type:    MessageTypeError,
        Payload: []byte(err),
    }
}
