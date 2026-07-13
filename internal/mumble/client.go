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

	"github.com/golang/protobuf/proto"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/MumbleProto"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/cryptstate"
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
	crypt       cryptstate.CryptState
	encryptMu   sync.Mutex
	decryptMu   sync.Mutex
	cryptReady  atomic.Bool
	udpAddr     *net.UDPAddr
	udpConn     atomic.Pointer[net.UDPConn]
	udpOnce     sync.Once
	udpLastRecv atomic.Int64 // unix nano of last successfully-decrypted UDP packet
	// udpSendBuf is scratch space for UDP ciphertext, reused across sends
	// (audio and ping) rather than allocated fresh per packet. Safe because
	// every write to it happens while holding encryptMu, which both
	// senders already need for crypt.Encrypt itself.
	udpSendBuf []byte

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
		Version:   proto.Uint32(clientVersion),
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
			ts := uint64(time.Now().UnixNano())
			_ = c.conn.writeProto(&MumbleProto.Ping{Timestamp: &ts})
		}
	}
}

func (c *Client) readLoop() {
	var disconnectErr error
	for {
		pType, data, err := c.conn.readPacket()
		if err != nil {
			disconnectErr = err
			break
		}
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
		// Version, Ping, and everything else this client doesn't act on.
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

// SelfChannelUsers returns the names of users in the client's current
// channel (including the client itself).
func (c *Client) SelfChannelUsers() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.self == nil || c.self.Channel == nil {
		return nil
	}
	names := make([]string, 0, len(c.self.Channel.Users))
	for _, u := range c.self.Channel.Users {
		names = append(names, u.Name)
	}
	return names
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
