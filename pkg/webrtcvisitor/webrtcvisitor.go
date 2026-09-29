// Package webrtcvisitor moves a private proxy's visitor data path onto a
// WebRTC DataChannel. The signaling is not WebRTC's problem here: the offer
// and the answer travel on the visitor's already-authenticated control
// connection, and the ICE gathering is non-trickle, so one offer frame and one
// answer frame are the whole exchange. What WebRTC brings is the data path:
// DTLS-encrypted bytes over UDP, with ICE picking a route that works from
// behind the kinds of networks that break direct TCP.
//
// The channel is wrapped as a net.Conn for the same relay code the TCP path
// uses. Deadlines are accepted and ignored — libdatachannel-style channels
// have none — and a half-close closes, as everywhere a stream has no
// half-close of its own.
package webrtcvisitor

import (
	"fmt"
	"net"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

const defaultTimeout = 30 * time.Second

// newPeerConnection creates a datachannel-only peer connection. No ICE servers:
// host candidates are enough for the server-to-visitor case, where the server
// is the reachable end. mDNS candidate obfuscation is off (a name nobody here
// can resolve), and loopback addresses stay in the list — the same-host tests
// are what prove the channel works.
func newPeerConnection() (*webrtc.PeerConnection, error) {
	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settingEngine.SetIPFilter(func(ip net.IP) bool { return true })
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine)).NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{},
	})
	if err != nil {
		return nil, fmt.Errorf("webrtcvisitor: create the peer connection: %w", err)
	}
	return pc, nil
}

// pending is a peer connection waiting for its data channel.
type pending struct {
	pc      *webrtc.PeerConnection
	channel chan *webrtc.DataChannel
}

func (p *pending) watch() {
	p.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			select {
			case p.channel <- dc:
			default:
			}
		})
	})
}

// VisitorOffer builds the visitor's offer. The returned accept function takes
// the server's answer, waits for the connection and the data channel, and
// returns the channel as a net.Conn.
func VisitorOffer() (offerSDP string, accept func(answerSDP string) (net.Conn, error), cleanup func(), err error) {
	pc, err := newPeerConnection()
	if err != nil {
		return "", nil, func() {}, err
	}
	// The visitor is the offerer, so the data channel is created here; the
	// server's channel arrives through its own OnDataChannel instead.
	channel, err := pc.CreateDataChannel("aethertunnel", nil)
	if err != nil {
		_ = pc.Close()
		return "", nil, func() {}, fmt.Errorf("webrtcvisitor: create the data channel: %w", err)
	}
	opened := make(chan *webrtc.DataChannel, 1)
	channel.OnOpen(func() { opened <- channel })

	offerDescription, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return "", nil, func() {}, fmt.Errorf("webrtcvisitor: build the offer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offerDescription); err != nil {
		_ = pc.Close()
		return "", nil, func() {}, fmt.Errorf("webrtcvisitor: set the local description: %w", err)
	}
	select {
	case <-gathered:
	case <-time.After(defaultTimeout):
		_ = pc.Close()
		return "", nil, func() {}, fmt.Errorf("webrtcvisitor: ICE gathering did not finish")
	}

	accept = func(answerSDP string) (net.Conn, error) {
		answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}
		if err := pc.SetRemoteDescription(answer); err != nil {
			return nil, fmt.Errorf("webrtcvisitor: apply the answer: %w", err)
		}
		select {
		case dc := <-opened:
			return newConn(pc, dc), nil
		case <-time.After(defaultTimeout):
			_ = pc.Close()
			return nil, fmt.Errorf("webrtcvisitor: the data channel never opened")
		}
	}
	cleanup = func() { _ = pc.Close() }
	return pc.LocalDescription().SDP, accept, cleanup, nil
}

// ServerAccept takes a visitor's offer and returns the answer SDP plus a wait
// function that blocks until the data channel is open and returns it as a
// net.Conn.
func ServerAccept(offerSDP string) (answerSDP string, wait func() (net.Conn, error), cleanup func(), err error) {
	pc, err := newPeerConnection()
	if err != nil {
		return "", nil, func() {}, err
	}
	pending := &pending{pc: pc, channel: make(chan *webrtc.DataChannel, 1)}
	pending.watch()

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}
	if err := pc.SetRemoteDescription(offer); err != nil {
		_ = pc.Close()
		return "", nil, func() {}, fmt.Errorf("webrtcvisitor: apply the visitor's offer: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return "", nil, func() {}, fmt.Errorf("webrtcvisitor: build the answer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return "", nil, func() {}, fmt.Errorf("webrtcvisitor: set the local description: %w", err)
	}
	select {
	case <-gathered:
	case <-time.After(defaultTimeout):
		_ = pc.Close()
		return "", nil, func() {}, fmt.Errorf("webrtcvisitor: ICE gathering did not finish")
	}

	wait = func() (net.Conn, error) {
		select {
		case dc := <-pending.channel:
			return newConn(pc, dc), nil
		case <-time.After(defaultTimeout):
			_ = pc.Close()
			return nil, fmt.Errorf("webrtcvisitor: the data channel never opened")
		}
	}
	cleanup = func() { _ = pc.Close() }
	return pc.LocalDescription().SDP, wait, cleanup, nil
}
