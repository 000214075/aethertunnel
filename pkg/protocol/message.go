package protocol

import (
	"encoding/json"
	"errors"
	"io"
	"net"
)

// 消息类型常量
const (
	TypeLogin          = 'L'  // 登录
	TypeLoginResp      = 'R'  // 登录响应
	TypeNewProxy       = 'P'  // 新代理
	TypeNewProxyResp   = 'Q'  // 新代理响应
	TypeCloseProxy     = 'C'  // 关闭代理
	TypeNewWorkConn    = 'W'  // 新工作连接
	TypeReqWorkConn    = 'O'  // 请求工作连接
	TypeStartWorkConn  = 'S'  // 启动工作连接
	TypePing           = 'H'  // 心跳
	TypePong           = 'O'  // 心跳响应
	TypeUDPPacket      = 'U'  // UDP数据包
)

var (
	ErrInvalidMessage     = errors.New("invalid message")
	ErrUnknownMessageType = errors.New("unknown message type")
)

// Message 消息接口
type Message interface {
	GetType() byte
}

// Login 登录消息
type Login struct {
	Version      string            `json:"version"`
	Hostname     string            `json:"hostname"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	User         string            `json:"user"`
	Token        string            `json:"token"`
	Timestamp    int64             `json:"timestamp"`
	RunID        string            `json:"run_id"`
	ClientID     string            `json:"client_id"`
	PoolCount    int               `json:"pool_count"`
	Metas        map[string]string `json:"metas"`
	Signature    []byte            `json:"signature"`  // Ed25519 签名
	PublicKey    []byte            `json:"public_key"` // Ed25519 公钥
}

func (m *Login) GetType() byte { return TypeLogin }

// LoginResp 登录响应
type LoginResp struct {
	Version     string `json:"version"`
	RunID       string `json:"run_id"`
	ServerTime  int64  `json:"server_time"`
	Error       string `json:"error,omitempty"`
	Nonce       []byte `json:"nonce"` // 用于后续签名
}

func (m *LoginResp) GetType() byte { return TypeLoginResp }

// NewProxy 新代理消息
type NewProxy struct {
	ProxyName          string            `json:"proxy_name"`
	ProxyType          string            `json:"proxy_type"`
	UseEncryption      bool              `json:"use_encryption"`
	UseCompression     bool              `json:"use_compression"`
	BandwidthLimit     string            `json:"bandwidth_limit"`
	BandwidthLimitMode string            `json:"bandwidth_limit_mode"`
	Group              string            `json:"group"`
	GroupKey           string            `json:"group_key"`
	Metas              map[string]string `json:"metas"`

	// TCP/UDP 特定字段
	RemotePort int `json:"remote_port,omitempty"`

	// HTTP/HTTPS 特定字段
	CustomDomains     []string          `json:"custom_domains,omitempty"`
	SubDomain         string            `json:"subdomain,omitempty"`
	Locations         []string          `json:"locations,omitempty"`
	HTTPUser          string            `json:"http_user,omitempty"`
	HTTPPwd           string            `json:"http_pwd,omitempty"`
	HostHeaderRewrite string            `json:"host_header_rewrite,omitempty"`

	// STCP/XTCP 特定字段
	SK         string   `json:"sk,omitempty"`
	AllowUsers []string `json:"allow_users,omitempty"`
}

func (m *NewProxy) GetType() byte { return TypeNewProxy }

// NewProxyResp 新代理响应
type NewProxyResp struct {
	ProxyName  string `json:"proxy_name"`
	RemoteAddr string `json:"remote_addr"`
	Error      string `json:"error,omitempty"`
}

func (m *NewProxyResp) GetType() byte { return TypeNewProxyResp }

// CloseProxy 关闭代理消息
type CloseProxy struct {
	ProxyName string `json:"proxy_name"`
}

func (m *CloseProxy) GetType() byte { return TypeCloseProxy }

// NewWorkConn 新工作连接消息
type NewWorkConn struct {
	RunID        string `json:"run_id"`
	Token        string `json:"token"`
	Timestamp    int64  `json:"timestamp"`
	Signature    []byte `json:"signature"` // 每个工作连接的签名
}

func (m *NewWorkConn) GetType() byte { return TypeNewWorkConn }

// StartWorkConn 启动工作连接消息
type StartWorkConn struct {
	ProxyName string `json:"proxy_name"`
	SrcAddr   string `json:"src_addr,omitempty"`
	SrcPort   uint16 `json:"src_port,omitempty"`
	DstAddr   string `json:"dst_addr,omitempty"`
	DstPort   uint16 `json:"dst_port,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (m *StartWorkConn) GetType() byte { return TypeStartWorkConn }

// Ping 心跳消息
type Ping struct {
	Timestamp int64  `json:"timestamp"`
	Signature []byte `json:"signature"` // HMAC 签名
}

func (m *Ping) GetType() byte { return TypePing }

// Pong 心跳响应
type Pong struct {
	ServerTime int64  `json:"server_time"`
	Signature  []byte `json:"signature"`
	Error      string `json:"error,omitempty"`
}

func (m *Pong) GetType() byte { return TypePong }

// UDPPacket UDP 数据包
type UDPPacket struct {
	Content    string       `json:"content"`
	LocalAddr  *net.UDPAddr `json:"local_addr,omitempty"`
	RemoteAddr *net.UDPAddr `json:"remote_addr,omitempty"`
}

func (m *UDPPacket) GetType() byte { return TypeUDPPacket }

// WriteMsg 写入消息
func WriteMsg(conn io.Writer, msg Message) error {
	// 序列化消息体
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// 消息格式: [类型(1字节)][长度(4字节)][数据体]
	buf := make([]byte, 1+4+len(data))
	buf[0] = msg.GetType()
	// 写入长度（小端）
	for i := 0; i < 4; i++ {
		buf[1+i] = byte(len(data) >> (8 * i))
	}
	copy(buf[5:], data)

	_, err = conn.Write(buf)
	return err
}

// ReadMsg 读取消息
func ReadMsg(conn io.Reader) (Message, error) {
	// 读取类型和长度
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}

	msgType := header[0]
	length := int(uint32(header[1]) | uint32(header[2])<<8 | uint32(header[3])<<16 | uint32(header[4])<<24)

	// 读取数据体
	if length > 10*1024*1024 { // 限制最大10MB
		return nil, ErrInvalidMessage
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, err
	}

	// 根据类型反序列化
	var msg Message
	switch msgType {
	case TypeLogin:
		msg = &Login{}
	case TypeLoginResp:
		msg = &LoginResp{}
	case TypeNewProxy:
		msg = &NewProxy{}
	case TypeNewProxyResp:
		msg = &NewProxyResp{}
	case TypeCloseProxy:
		msg = &CloseProxy{}
	case TypeNewWorkConn:
		msg = &NewWorkConn{}
	case TypeStartWorkConn:
		msg = &StartWorkConn{}
	case TypePing:
		msg = &Ping{}
	case TypePong:
		msg = &Pong{}
	case TypeUDPPacket:
		msg = &UDPPacket{}
	default:
		return nil, ErrUnknownMessageType
	}

	if err := json.Unmarshal(data, msg); err != nil {
		return nil, err
	}

	return msg, nil
}

// ReadMsgInto 读取消息到指定结构
func ReadMsgInto(conn io.Reader, msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	received, err := ReadMsg(conn)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, received)
}
