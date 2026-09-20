package dht

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// Datagram layout, all integers big endian:
//
//	+---------+--------+----------+--------+-------------+
//	| magic:4 | type:1 | txid:16  | len:4  | body:len    |
//	+---------+--------+----------+--------+-------------+
//
// magic is "AET\x01". The declared length is compared with the datagram size
// before any body field is read, so a truncated or padded packet is dropped
// instead of being decoded out of whatever follows it in the buffer.
const (
	magicA      = 0x41
	magicE      = 0x45
	magicT      = 0x54
	magicVer    = 0x01
	txLen       = 16
	headerLen   = 4 + 1 + txLen + 4
	maxBody     = 8 << 10
	maxDatagram = headerLen + maxBody
	maxNodes    = 64 // most contacts one NODES reply may carry
	maxAddrLen  = 255
)

type msgType uint8

const (
	msgPing      msgType = 1
	msgPong      msgType = 2
	msgStore     msgType = 3
	msgFindNode  msgType = 4
	msgNodes     msgType = 5
	msgFindValue msgType = 6
	msgValue     msgType = 7
)

func (t msgType) String() string {
	switch t {
	case msgPing:
		return "PING"
	case msgPong:
		return "PONG"
	case msgStore:
		return "STORE"
	case msgFindNode:
		return "FIND_NODE"
	case msgNodes:
		return "NODES"
	case msgFindValue:
		return "FIND_VALUE"
	case msgValue:
		return "VALUE"
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// message is one datagram. Fields that do not apply to a type are zero.
type message struct {
	typ    msgType
	tx     [txLen]byte
	id     ID   // sender
	target ID   // NODES: the contacts are chosen for proximity to this
	ns     byte // STORE, FIND_VALUE, VALUE: key namespace
	key    ID   // STORE, FIND_VALUE, VALUE
	expiry int64
	value  []byte
	found  bool
	nodes  []Node
}

func encode(m message) ([]byte, error) {
	body, err := encodeBody(m)
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("dht: %s body of %d bytes exceeds %d", m.typ, len(body), maxBody)
	}
	pkt := make([]byte, headerLen+len(body))
	pkt[0], pkt[1], pkt[2], pkt[3] = magicA, magicE, magicT, magicVer
	pkt[4] = byte(m.typ)
	copy(pkt[5:5+txLen], m.tx[:])
	binary.BigEndian.PutUint32(pkt[5+txLen:headerLen], uint32(len(body)))
	copy(pkt[headerLen:], body)
	return pkt, nil
}

func decode(pkt []byte) (message, error) {
	var m message
	if len(pkt) < headerLen {
		return m, fmt.Errorf("dht: %d byte datagram is shorter than the %d byte header", len(pkt), headerLen)
	}
	if pkt[0] != magicA || pkt[1] != magicE || pkt[2] != magicT || pkt[3] != magicVer {
		return m, errors.New("dht: bad magic")
	}
	n := binary.BigEndian.Uint32(pkt[5+txLen : headerLen])
	if n > maxBody {
		return m, fmt.Errorf("dht: declared body of %d bytes exceeds %d", n, maxBody)
	}
	if int(n) != len(pkt)-headerLen {
		return m, fmt.Errorf("dht: declared body of %d bytes, datagram carries %d", n, len(pkt)-headerLen)
	}
	m.typ = msgType(pkt[4])
	copy(m.tx[:], pkt[5:5+txLen])
	// Copy the body: the caller reuses the read buffer, and decoded values are
	// handed to the store and to callers.
	body := make([]byte, n)
	copy(body, pkt[headerLen:])
	if err := decodeBody(&m, body); err != nil {
		return message{}, err
	}
	return m, nil
}

func encodeBody(m message) ([]byte, error) {
	b := make([]byte, 0, 64)
	switch m.typ {
	case msgPing, msgPong:
		b = append(b, m.id[:]...)
	case msgFindNode:
		b = append(b, m.id[:]...)
		b = append(b, m.target[:]...)
	case msgFindValue:
		b = append(b, m.id[:]...)
		b = append(b, m.ns)
		b = append(b, m.key[:]...)
	case msgStore:
		if len(m.value) > MaxValueSize {
			return nil, ErrValueTooLarge
		}
		b = append(b, m.id[:]...)
		b = append(b, m.ns)
		b = append(b, m.key[:]...)
		var exp [8]byte
		binary.BigEndian.PutUint64(exp[:], uint64(m.expiry))
		b = append(b, exp[:]...)
		b = append(b, m.value...)
	case msgValue:
		b = append(b, m.id[:]...)
		b = append(b, m.ns)
		b = append(b, m.key[:]...)
		if m.found {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
		b = append(b, m.value...)
	case msgNodes:
		entries := make([]byte, 0, 64*len(m.nodes))
		count := 0
		for _, n := range m.nodes {
			if count >= maxNodes || len(n.Addr) == 0 || len(n.Addr) > maxAddrLen {
				break
			}
			// Drop entries that would push the reply past the datagram limit
			// instead of failing the whole reply.
			if headerLen+idLen+2+len(entries)+idLen+1+len(n.Addr) > maxDatagram {
				break
			}
			entries = append(entries, n.ID[:]...)
			entries = append(entries, byte(len(n.Addr)))
			entries = append(entries, n.Addr...)
			count++
		}
		b = append(b, m.id[:]...)
		b = append(b, m.target[:]...)
		var cnt [2]byte
		binary.BigEndian.PutUint16(cnt[:], uint16(count))
		b = append(b, cnt[:]...)
		b = append(b, entries...)
	default:
		return nil, fmt.Errorf("dht: cannot encode unknown message type %d", uint8(m.typ))
	}
	return b, nil
}

func decodeBody(m *message, body []byte) error {
	r := &bodyReader{b: body}
	var err error
	switch m.typ {
	case msgPing, msgPong:
		m.id, err = r.id()
	case msgFindNode:
		if m.id, err = r.id(); err == nil {
			m.target, err = r.id()
		}
	case msgFindValue:
		if m.id, err = r.id(); err == nil {
			if m.ns, err = r.byte(); err == nil {
				m.key, err = r.id()
			}
		}
	case msgStore:
		if m.id, err = r.id(); err == nil {
			if m.ns, err = r.byte(); err == nil {
				if m.key, err = r.id(); err == nil {
					var exp uint64
					if exp, err = r.uint64(); err == nil {
						m.expiry = int64(exp)
						m.value, err = r.rest()
					}
				}
			}
		}
	case msgValue:
		if m.id, err = r.id(); err == nil {
			if m.ns, err = r.byte(); err == nil {
				if m.key, err = r.id(); err == nil {
					var present byte
					if present, err = r.byte(); err == nil {
						m.found = present != 0
						m.value, err = r.rest()
					}
				}
			}
		}
	case msgNodes:
		if m.id, err = r.id(); err == nil {
			if m.target, err = r.id(); err == nil {
				var count uint16
				if count, err = r.uint16(); err == nil {
					if int(count) > maxNodes {
						return fmt.Errorf("dht: NODES carries %d contacts, max %d", count, maxNodes)
					}
					nodes := make([]Node, 0, count)
					for i := 0; i < int(count) && err == nil; i++ {
						var n Node
						if n.ID, err = r.id(); err == nil {
							var l byte
							if l, err = r.byte(); err == nil {
								var addr []byte
								if addr, err = r.take(int(l)); err == nil {
									// Only literal addresses are accepted: a peer
									// must not be able to name a hostname that
									// this node would then resolve.
									if n.Addr = string(addr); !validContact(n.Addr) {
										err = fmt.Errorf("dht: NODES contact %q is not a host:port literal address", n.Addr)
									}
								}
							}
						}
						if err == nil {
							nodes = append(nodes, n)
						}
					}
					m.nodes = nodes
				}
			}
		}
	default:
		return fmt.Errorf("dht: unknown message type %d", uint8(m.typ))
	}
	if err != nil {
		return err
	}
	if (m.typ == msgStore || m.typ == msgValue) && len(m.value) > MaxValueSize {
		return ErrValueTooLarge
	}
	return r.done()
}

// validContact reports whether addr is an IP literal with a port.
func validContact(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return net.ParseIP(host) != nil
}

// bodyReader reads big endian body fields, failing instead of panicking on a
// short body.
type bodyReader struct {
	b   []byte
	off int
}

var errShortBody = errors.New("dht: truncated body")

func (r *bodyReader) take(n int) ([]byte, error) {
	if n < 0 || n > len(r.b)-r.off {
		return nil, errShortBody
	}
	start := r.off
	r.off += n
	return r.b[start:r.off], nil
}

func (r *bodyReader) rest() ([]byte, error) { return r.take(len(r.b) - r.off) }

func (r *bodyReader) byte() (byte, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *bodyReader) uint16() (uint16, error) {
	b, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (r *bodyReader) uint64() (uint64, error) {
	b, err := r.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

func (r *bodyReader) id() (ID, error) {
	var id ID
	b, err := r.take(idLen)
	if err != nil {
		return id, err
	}
	copy(id[:], b)
	return id, nil
}

func (r *bodyReader) done() error {
	if r.off != len(r.b) {
		return fmt.Errorf("dht: %d unread bytes at the end of the body", len(r.b)-r.off)
	}
	return nil
}
