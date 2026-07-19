// Package mumble is a small, purpose-built Mumble client. It implements only
// the subset of the protocol this bridge needs: connect/authenticate,
// channel+user roster tracking, text messages, and audio relay (both the
// TCP-tunneled fallback and, when available, the real low-latency encrypted
// UDP voice channel). It never decodes or encodes audio itself — Opus
// payloads are passed through as opaque bytes in both directions.
//
// See NOTICE.md for attribution of the small pieces of code (protobuf
// bindings, varint codec, OCB2-AES128 cipher) carried over from existing
// open-source Mumble implementations rather than reimplemented from scratch.
package mumble

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/MumbleProto"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/cryptstate"
	"google.golang.org/protobuf/proto"
)

// clientVersion is encoded as (major<<16 | minor<<8 | patch), matching the
// legacy version field every Mumble server still understands.
const clientVersion = 1<<16 | 3<<8 | 0

const dialTimeout = 10 * time.Second

// Client is a connection to a single Mumble server.
type Client struct {
	cfg  *Config
	conn *conn

	// mu guards everything below: the roster/channel tree and self/session
	// are mutated by the read loop and read by callers concurrently.
	mu       sync.Mutex
	synced   bool
	session  uint32
	self     *User
	users    map[uint32]*User
	channels map[uint32]*Channel

	// audio: crypt state + UDP voice channel. See udp.go.
	//
	// encryptMu and decryptMu are separate (rather than one lock for the
	// whole CryptState) so that decrypting another Mumble user's incoming
	// audio can never make our own outgoing audio/ping sends wait, or vice
	// versa: Encrypt only touches EncryptIV, Decrypt only touches
	// DecryptIV/decryptHistory/stats, and the underlying AES cipher.Block
	// is safe for concurrent use, so the two directions have no shared
	// mutable state once the initial key exchange (which does touch both,
	// see handleCryptSetup) has completed.
	//
	// Encrypt itself is only ever called from udpSendLoop (the sole
	// consumer of udpSendCh) — audio (readBrowserAudio) and pings
	// (udpPingLoop) both just enqueue plaintext frames there rather than
	// calling encrypt+write directly, so producing a frame is a fast,
	// lock-free channel send and neither sender can ever be blocked
	// waiting on the other's encrypt+write. encryptMu still exists because
	// handleCryptSetup's rare SetKey/resync-reply cases touch EncryptIV
	// from the main read-loop goroutine, concurrently with udpSendLoop.
	crypt       cryptstate.CryptState
	encryptMu   sync.Mutex
	decryptMu   sync.Mutex
	cryptReady  atomic.Bool
	udpAddr     *net.UDPAddr
	udpConn     atomic.Pointer[net.UDPConn]
	udpSendCh   chan []byte
	udpOnce     sync.Once
	udpLastRecv atomic.Int64 // unix nano of last successfully-decrypted UDP packet
	// udpSendBuf is scratch space for UDP ciphertext, reused across sends
	// (audio and ping) rather than allocated fresh per packet. Safe because
	// it's only ever touched from udpSendLoop, the sole sender goroutine.
	udpSendBuf []byte

	// ping: connection-quality stats reported in our own outgoing Ping
	// messages, purely so a real Mumble client querying our user's
	// Statistics has something other than zeroes to show. tcpPingStats is
	// fed from handlePing (the server echoes back our Timestamp; RTT is
	// now-echoed); udpPingStats is fed from udpReadLoop the same way over
	// the voice channel. Good/Late/Lost/Resync are read directly off crypt
	// (already tracked there for OCB2 replay protection) rather than
	// duplicated here.
	tcpPacketsRecv atomic.Uint32
	udpPacketsRecv atomic.Uint32
	tcpPingStats   pingStats
	udpPingStats   pingStats

	handshakeDone chan error
	end           chan struct{}
	endOnce       sync.Once
}

// Dial connects to the Mumble server at addr, authenticates, and waits for
// the initial channel/user sync to complete before returning.
func Dial(addr string, tlsConfig *tls.Config, cfg *Config) (*Client, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	tcpConn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	if err != nil {
		return nil, err
	}

	// Mumble's UDP voice port is the same host:port as the TCP control
	// connection. Derive the UDP target from the TCP connection's actual
	// resolved remote address (rather than re-resolving addr independently)
	// so the two can never land on different IPs for a multi-A-record host.
	tcpRemote, ok := tcpConn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		tcpConn.Close()
		return nil, fmt.Errorf("mumble: unexpected remote address type %T", tcpConn.RemoteAddr())
	}
	udpAddr := &net.UDPAddr{IP: tcpRemote.IP, Port: tcpRemote.Port, Zone: tcpRemote.Zone}

	if cfg.DisableUDP {
		log.Printf("mumble: UDP voice channel disabled by config; all audio will use the TCP tunnel")
	}

	root := newChannel(0)
	c := &Client{
		cfg:           cfg,
		conn:          newConn(tcpConn),
		users:         make(map[uint32]*User),
		channels:      map[uint32]*Channel{0: root},
		udpAddr:       udpAddr,
		handshakeDone: make(chan error, 1),
		end:           make(chan struct{}),
	}

	go c.readLoop()
	go c.pingLoop()

	version := &MumbleProto.Version{
		VersionV1: proto.Uint32(clientVersion),
		Release:   proto.String("mumble-webrtc-bridge"),
		Os:        proto.String(runtime.GOOS),
		OsVersion: proto.String(runtime.GOARCH),
	}
	auth := &MumbleProto.Authenticate{
		Username: proto.String(cfg.Username),
		Password: proto.String(cfg.Password),
		Opus:     proto.Bool(true),
		Tokens:   cfg.Tokens,
	}
	if err := c.conn.writeProto(version); err != nil {
		tcpConn.Close()
		return nil, err
	}
	if err := c.conn.writeProto(auth); err != nil {
		tcpConn.Close()
		return nil, err
	}

	select {
	case err := <-c.handshakeDone:
		if err != nil {
			c.conn.Close()
			return nil, err
		}
		return c, nil
	case <-time.After(dialTimeout):
		c.conn.Close()
		return nil, errors.New("mumble: synchronization timeout")
	}
}

// completeHandshake delivers the Dial() result exactly once. Safe to call
// multiple times (e.g. a Reject arriving after some other error).
func (c *Client) completeHandshake(err error) {
	select {
	case c.handshakeDone <- err:
	default:
	}
}

func (c *Client) pingLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.end:
			return
		case <-ticker.C:
			c.sendPing()
		}
	}
}

// sendPing builds and sends a fully populated Ping message: our own
// timestamp (which the server echoes back verbatim, letting handlePing
// derive a TCP RTT sample from it — see docs/dev/network-protocol in the
// Mumble repo, "Server should not attempt to decode" this field) plus the
// same connection-quality stats a real client reports, so anyone viewing
// this bridge's connection in their Statistics dialog sees real numbers
// instead of nothing.
func (c *Client) sendPing() {
	ts := uint64(time.Now().UnixNano())

	c.decryptMu.Lock()
	good, late, lost, resync := c.crypt.Good, c.crypt.Late, c.crypt.Lost, c.crypt.Resync
	c.decryptMu.Unlock()

	udpPackets := c.udpPacketsRecv.Load()
	tcpPackets := c.tcpPacketsRecv.Load()
	udpAvg, udpVar := c.udpPingStats.avgVar()
	tcpAvg, tcpVar := c.tcpPingStats.avgVar()

	_ = c.conn.writeProto(&MumbleProto.Ping{
		Timestamp:  &ts,
		Good:       &good,
		Late:       &late,
		Lost:       &lost,
		Resync:     &resync,
		UdpPackets: &udpPackets,
		TcpPackets: &tcpPackets,
		UdpPingAvg: &udpAvg,
		UdpPingVar: &udpVar,
		TcpPingAvg: &tcpAvg,
		TcpPingVar: &tcpVar,
	})
}

// handlePing processes a Ping message received from the server: its reply
// to our own most recent Ping (Timestamp is our value, echoed back — RTT is
// simply now minus that), and its good/late/lost/resync counts describing
// how well it's been receiving our UDP audio (stored on crypt purely for
// parity with a real client's m_statsRemote; nothing here currently reads
// them back out).
func (c *Client) handlePing(data []byte) error {
	var p MumbleProto.Ping
	if err := proto.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.Timestamp != nil {
		if rttNanos := time.Now().UnixNano() - int64(*p.Timestamp); rttNanos >= 0 {
			c.tcpPingStats.add(float32(rttNanos) / 1e6)
		}
	}
	c.decryptMu.Lock()
	if p.Good != nil {
		c.crypt.RemoteGood = *p.Good
	}
	if p.Late != nil {
		c.crypt.RemoteLate = *p.Late
	}
	if p.Lost != nil {
		c.crypt.RemoteLost = *p.Lost
	}
	if p.Resync != nil {
		c.crypt.RemoteResync = *p.Resync
	}
	c.decryptMu.Unlock()
	return nil
}

func (c *Client) readLoop() {
	var disconnectErr error
	for {
		pType, data, err := c.conn.readPacket()
		if err != nil {
			disconnectErr = err
			break
		}
		c.tcpPacketsRecv.Add(1)
		c.dispatch(pType, data)
	}

	c.mu.Lock()
	wasSynced := c.synced
	c.mu.Unlock()

	c.completeHandshake(disconnectErr)
	c.teardownUDP()
	c.endOnce.Do(func() { close(c.end) })

	if wasSynced && c.cfg.OnDisconnect != nil {
		c.cfg.OnDisconnect(c, disconnectErr)
	}
}

func (c *Client) dispatch(pType uint16, data []byte) {
	var err error
	switch pType {
	case ptUDPTunnel:
		err = c.handleAudioPacket(data)
	case ptPing:
		err = c.handlePing(data)
	case ptReject:
		err = c.handleReject(data)
	case ptServerSync:
		err = c.handleServerSync(data)
	case ptChannelState:
		err = c.handleChannelState(data)
	case ptChannelRemove:
		err = c.handleChannelRemove(data)
	case ptUserState:
		err = c.handleUserState(data)
	case ptUserRemove:
		err = c.handleUserRemove(data)
	case ptTextMessage:
		err = c.handleTextMessage(data)
	case ptCryptSetup:
		err = c.handleCryptSetup(data)
	default:
		// Version and everything else this client doesn't act on.
	}
	if err != nil {
		// A single malformed/unexpected message shouldn't take down the
		// connection; log-and-continue.
		log.Printf("mumble: handling packet type %d: %v", pType, err)
	}
}

// Disconnect closes the connection. OnDisconnect will still fire (with a nil
// error) from the read loop as it unwinds.
func (c *Client) Disconnect() error {
	return c.conn.Close()
}

// Self returns the client's own user. Only valid after Dial returns.
func (c *Client) Self() *User {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.self
}

// SelfChannelUsers returns the status of users in the client's current
// channel (including the client itself).
func (c *Client) SelfChannelUsers() []UserStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.self == nil || c.self.Channel == nil {
		return nil
	}
	statuses := make([]UserStatus, 0, len(c.self.Channel.Users))
	for _, u := range c.self.Channel.Users {
		statuses = append(statuses, UserStatus{
			Name:         u.Name,
			Muted:        u.Muted,
			SelfMuted:    u.SelfMuted,
			Deafened:     u.Deafened,
			SelfDeafened: u.SelfDeafened,
		})
	}
	return statuses
}

// UserName returns the display name of the user with the given session, or
// "" if unknown.
func (c *Client) UserName(session uint32) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u := c.users[session]; u != nil {
		return u.Name
	}
	return ""
}

// JoinChannel moves the client into the channel at the given path (by
// channel name, from the root), e.g. JoinChannel("Games", "Sub"). It is a
// no-op if no channel matches the path.
func (c *Client) JoinChannel(path ...string) error {
	c.mu.Lock()
	target := c.channels[0].find(path)
	session := c.session
	c.mu.Unlock()
	if target == nil {
		return nil
	}
	return c.conn.writeProto(&MumbleProto.UserState{
		Session:   &session,
		ChannelId: &target.ID,
	})
}

// SendChannelText sends a text message to the client's current channel.
func (c *Client) SendChannelText(message string) error {
	c.mu.Lock()
	ch := c.self.Channel
	c.mu.Unlock()
	if ch == nil {
		return errors.New("mumble: not in a channel")
	}
	return c.conn.writeProto(&MumbleProto.TextMessage{
		ChannelId: []uint32{ch.ID},
		Message:   &message,
	})
}

// SetSelfMuted sets the client's own self-mute flag, visible to other users.
func (c *Client) SetSelfMuted(muted bool) error {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	return c.conn.writeProto(&MumbleProto.UserState{
		Session:  &session,
		SelfMute: &muted,
	})
}

// SetSelfDeafened sets the client's own self-deafen flag, visible to other
// users.
func (c *Client) SetSelfDeafened(deaf bool) error {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	return c.conn.writeProto(&MumbleProto.UserState{
		Session:  &session,
		SelfDeaf: &deaf,
	})
}

// SetSelfMuteDeaf sets the client's own self-mute and self-deaf flags
// together in a single UserState packet, visible to other users. Toggling
// deafen always implies a mute change too, and sending that as one packet
// (rather than two separate SetSelfMuted/SetSelfDeafened calls) avoids some
// Mumble servers broadcasting the resulting state-change notification twice.
func (c *Client) SetSelfMuteDeaf(muted, deaf bool) error {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	return c.conn.writeProto(&MumbleProto.UserState{
		Session:  &session,
		SelfMute: &muted,
		SelfDeaf: &deaf,
	})
}
