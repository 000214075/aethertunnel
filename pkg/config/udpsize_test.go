package config

import (
	"strconv"
	"strings"
	"testing"
)

const udpSizeServerBase = `
[server]
bind_addr = "127.0.0.1"
bind_port = 7001
auth_token = "0123456789abcdef0123456789abcdef"
`

const udpSizeClientBase = `
[client]
server_addr = "127.0.0.1:7001"
auth_token = "0123456789abcdef0123456789abcdef"
`

// A datagram cap has to be a payload size: negative asks for fewer than zero
// bytes and a value above the largest UDP payload cannot be reached, so both
// are refused by name rather than quietly clamped.
func TestUDPPacketSizeRejectsImpossibleValues(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		client bool
		want   string
	}{
		{"server negative", udpSizeServerBase + "udp_packet_size = -1\n",
			false, "server.udp_packet_size cannot be negative"},
		{"server above the largest UDP payload", udpSizeServerBase + "udp_packet_size = 65536\n",
			false, "server.udp_packet_size cannot exceed 65535"},
		{"client negative", udpSizeClientBase + "udp_packet_size = -1\n",
			true, "client.udp_packet_size cannot be negative"},
		{"client above the largest UDP payload", udpSizeClientBase + "udp_packet_size = 70000\n",
			true, "client.udp_packet_size cannot exceed 65535"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			var err error
			if tc.client {
				_, err = LoadClient(path)
			} else {
				_, err = LoadServer(path)
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// Zero and a fitting cap are both accepted: zero keeps the largest payload,
// and a smaller one is the point of the key.
func TestUDPPacketSizeAcceptsUsableValues(t *testing.T) {
	for _, value := range []int{0, 1200, 65535} {
		path := writeConfig(t, udpSizeServerBase+"udp_packet_size = "+strconv.Itoa(value)+"\n")
		cfg, err := LoadServer(path)
		if err != nil {
			t.Fatalf("LoadServer rejected server.udp_packet_size = %d: %v", value, err)
		}
		if cfg.Server.UDPPacketSize != value {
			t.Errorf("server.udp_packet_size is %d, want %d", cfg.Server.UDPPacketSize, value)
		}
	}
}
