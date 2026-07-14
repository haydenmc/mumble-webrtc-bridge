package mumble

import (
	"log"
	"net"
	"time"

	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/MumbleProto"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/cryptstate"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/varint"
	"google.golang.org/protobuf/proto"
)

const (
	// udpStaleTimeout bounds how long we'll keep trusting the UDP path
	// without hearing anything back over it. Exceeding it makes
	// tryWriteUDP fall back to the TCP tunnel again, so a UDP path that
	// silently breaks mid-session (NAT rebinding, a firewall closing the
	// flow, etc.) self-heals rather than blackholing audio.
	udpStaleTimeout = 15 * time.Second

	// udpPingInterval matches the real Mumble client's default
	// iPingIntervalMsec exactly (Settings.h, 5000ms) — a flat cadence from
	// the moment UDP is up, no extra burst at the start. An earlier version
	// of this client added a rapid burst of pings (5 at 250ms) on first
	// connecting to punch through NAT faster; the real client has no such
	// behavior, and it was visibly showing up as far more ping traffic than
	// a real client produces once the Ping message actually reported real
	// stats (see sendPing).
	udpPingInterval = 5 * time.Second

	// udpSendQueueDepth bounds how many plaintext frames can be queued for
	// udpSendLoop before a producer (readBrowserAudio or the ping loop)
	// gives up on this send rather than wait. Generous since encrypt+write
	// is fast (microseconds) and this is the only goroutine draining it —
	// this is purely a buffer against transient scheduling delays, not a
	// normal operating depth.
	udpSendQueueDepth = 32
)

// handleCryptSetup applies the server's CryptSetup message. Semantics
// verified against Mumble's own client (MainWindow::msgCryptSetup in
// src/mumble/Messages.cpp):
//
//   - key + client_nonce + server_nonce all set: initial handshake. Our
//     EncryptIV is the client_nonce, our DecryptIV is the server_nonce (the
//     server encrypts with what it calls "ServerNonce" and expects our
//     packets encrypted with what it calls "ClientNonce").
//   - server_nonce only: the server is resyncing our decrypt direction
//     after detecting decrypt failures on its own end.
//   - nothing set: the server is asking us to resend our current EncryptIV
//     (as client_nonce) so it can resync its decrypt direction.
func (c *Client) handleCryptSetup(data []byte) error {
	var p MumbleProto.CryptSetup
	if err := proto.Unmarshal(data, &p); err != nil {
		return err
	}

	switch {
	case len(p.Key) > 0 && len(p.ClientNonce) > 0 && len(p.ServerNonce) > 0:
		// Touches both EncryptIV and DecryptIV together; hold both locks.
		// Safe from deadlock since this is the only place both are ever
		// taken together, and it happens once, before the UDP goroutines
		// (which take them individually) are even started below.
		c.encryptMu.Lock()
		c.decryptMu.Lock()
		err := c.crypt.SetKey(cryptstate.ModeOCB2AES128, p.Key, p.ClientNonce, p.ServerNonce)
		c.decryptMu.Unlock()
		c.encryptMu.Unlock()
		if err != nil {
			return err
		}
		c.cryptReady.Store(true)
		if !c.cfg.DisableUDP {
			c.udpOnce.Do(c.startUDP)
		}
		return nil

	case len(p.ServerNonce) > 0:
		c.decryptMu.Lock()
		err := c.crypt.SetDecryptIV(p.ServerNonce)
		c.crypt.Resync++
		c.decryptMu.Unlock()
		return err

	default:
		if !c.cryptReady.Load() {
			return nil
		}
		c.encryptMu.Lock()
		nonce := append([]byte(nil), c.crypt.EncryptIV...)
		c.encryptMu.Unlock()
		return c.conn.writeProto(&MumbleProto.CryptSetup{ClientNonce: nonce})
	}
}

// startUDP dials the server's UDP voice port (the same host:port as the TCP
// control connection) and starts the send/receive goroutines. Called at
// most once per Client, after the first successful CryptSetup.
func (c *Client) startUDP() {
	udpConn, err := net.DialUDP("udp", nil, c.udpAddr)
	if err != nil {
		log.Printf("mumble: UDP voice channel unavailable, staying on TCP tunnel: dial %s: %v", c.udpAddr, err)
		return
	}
	log.Printf("mumble: UDP voice channel dialed (%s); confirming liveness before using it for audio", c.udpAddr)
	c.udpConn.Store(udpConn)
	c.udpSendCh = make(chan []byte, udpSendQueueDepth)

	go func() {
		<-c.end
		udpConn.Close()
	}()
	go c.udpReadLoop(udpConn)
	go c.udpSendLoop(udpConn)
	go c.udpPingLoop()
}

func (c *Client) teardownUDP() {
	if conn := c.udpConn.Load(); conn != nil {
		conn.Close()
	}
}

func (c *Client) udpReadLoop(udpConn *net.UDPConn) {
	buf := make([]byte, 2048)
	plain := make([]byte, 2048)
	for {
		n, err := udpConn.Read(buf)
		if err != nil {
			return
		}

		c.decryptMu.Lock()
		decErr := c.crypt.Decrypt(plain, buf[:n])
		plainLen := 0
		if decErr == nil {
			plainLen = n - c.crypt.Overhead()
		}
		c.decryptMu.Unlock()

		if decErr != nil || plainLen <= 0 {
			continue
		}
		if c.udpLastRecv.Load() == 0 {
			log.Printf("mumble: UDP voice channel confirmed working; audio will prefer it over the TCP tunnel")
		}
		c.udpLastRecv.Store(time.Now().UnixNano())
		c.udpPacketsRecv.Add(1)

		pkt := plain[:plainLen]
		switch (pkt[0] >> 5) & 0x7 {
		case audioTypeOpus:
			_ = c.handleAudioPacket(pkt)
		case audioTypePing:
			// The server echoes back our own ping payload verbatim, so the
			// decoded value is our own send timestamp — RTT is just now
			// minus that.
			if ts, n := varint.Decode(pkt[1:]); n > 0 {
				if rttNanos := time.Now().UnixNano() - ts; rttNanos >= 0 {
					c.udpPingStats.add(float32(rttNanos) / 1e6)
				}
			}
		}
	}
}

func (c *Client) udpPingLoop() {
	// One immediate ping so UDP liveness (and the TCP-tunnel-vs-UDP choice
	// in tryWriteUDP) is confirmed right away rather than waiting a full
	// interval for the first tick; every ping after that follows the real
	// client's flat cadence, no burst.
	c.enqueueUDPSend(buildPingFrame(uint64(time.Now().UnixNano())))

	ticker := time.NewTicker(udpPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.end:
			return
		case <-ticker.C:
			c.enqueueUDPSend(buildPingFrame(uint64(time.Now().UnixNano())))
		}
	}
}

// buildPingFrame builds a legacy voice-channel ping packet: header byte
// (type=Ping, target=0) followed by a single varint-encoded timestamp — the
// server treats this as opaque and just echoes it back verbatim, so any
// monotonically-useful value works as long as it fits a 64-bit varint (see
// docs/dev/network-protocol/voice_data.md in the Mumble repo). Previously
// this encoded the timestamp as a fixed 8-byte big-endian integer instead of
// a varint, which happened to still produce a packet the server accepted
// and replied to (so UDP liveness detection never noticed), but wasn't
// actually protocol-correct.
func buildPingFrame(timestamp uint64) []byte {
	var buf [1 + varint.MaxVarintLen]byte
	buf[0] = audioTypePing << 5
	n := 1 + varint.Encode(buf[1:], int64(timestamp))
	return append([]byte(nil), buf[:n]...)
}

// tryWriteUDP attempts to queue an already-built audio frame for the UDP
// voice channel's sender goroutine (see udpSendLoop). It returns false
// (letting the caller fall back to the TCP tunnel) whenever UDP isn't
// ready, hasn't been confirmed working recently, or udpSendLoop is
// (abnormally) backed up enough to fill udpSendQueueDepth.
func (c *Client) tryWriteUDP(frame []byte) bool {
	if !c.cryptReady.Load() {
		return false
	}
	if c.udpConn.Load() == nil {
		return false
	}
	last := c.udpLastRecv.Load()
	if last == 0 || time.Since(time.Unix(0, last)) > udpStaleTimeout {
		return false
	}
	return c.enqueueUDPSend(frame)
}

// enqueueUDPSend hands a plaintext frame to udpSendLoop. Returns false if
// the queue is full rather than blocking the caller (readBrowserAudio or
// udpPingLoop) — under normal conditions udpSendLoop drains in
// microseconds, so hitting this means something is genuinely wrong with
// the UDP path, not routine contention.
func (c *Client) enqueueUDPSend(frame []byte) bool {
	select {
	case c.udpSendCh <- frame:
		return true
	default:
		return false
	}
}

// udpSendLoop is the sole goroutine that ever calls crypt.Encrypt or writes
// to the UDP socket, for both audio (via tryWriteUDP) and keepalive pings
// (via udpPingLoop) — they only ever enqueue a plaintext frame here rather
// than encrypting and writing directly. This means producing a frame to
// send is always a fast, lock-free channel operation that can never block
// on the other sender's encrypt+write, the same property Mumble's own C++
// client gets for its UDP receive path for free from Qt's thread-affinity
// model (see ServerHandler::udpReady, which needs no lock because it's
// only ever invoked on ServerHandler's own thread) — Go has no equivalent
// automatic single-thread confinement, so this goroutine exists to
// establish it explicitly for the send side.
//
// Mumble's crypt IV is a strict per-connection counter, so encrypt and
// write still need to happen as one atomic step relative to encryptMu's
// other (rare) users — handleCryptSetup's SetKey/resync-reply cases, which
// run on the main read-loop goroutine — even though there's no longer
// another sender goroutine to worry about.
func (c *Client) udpSendLoop(udpConn *net.UDPConn) {
	for {
		select {
		case <-c.end:
			return
		case plain := <-c.udpSendCh:
			c.encryptMu.Lock()
			need := len(plain) + c.crypt.Overhead()
			if cap(c.udpSendBuf) < need {
				c.udpSendBuf = make([]byte, need)
			}
			ct := c.udpSendBuf[:need]
			c.crypt.Encrypt(ct, plain)
			_, _ = udpConn.Write(ct)
			c.encryptMu.Unlock()
		}
	}
}
