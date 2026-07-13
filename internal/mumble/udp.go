package mumble

import (
	"encoding/binary"
	"log"
	"net"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/MumbleProto"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/cryptstate"
)

const (
	// udpStaleTimeout bounds how long we'll keep trusting the UDP path
	// without hearing anything back over it. Exceeding it makes
	// tryWriteUDP fall back to the TCP tunnel again, so a UDP path that
	// silently breaks mid-session (NAT rebinding, a firewall closing the
	// flow, etc.) self-heals rather than blackholing audio.
	udpStaleTimeout = 15 * time.Second

	// On first establishing crypt keys we send a short burst of pings to
	// punch through NAT and get an initial liveness confirmation quickly,
	// then settle into a steady keepalive/liveness cadence.
	udpRapidPings         = 5
	udpRapidPingInterval  = 250 * time.Millisecond
	udpSteadyPingInterval = 5 * time.Second
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

	go func() {
		<-c.end
		udpConn.Close()
	}()
	go c.udpReadLoop(udpConn)
	go c.udpPingLoop(udpConn)
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

		pkt := plain[:plainLen]
		if (pkt[0]>>5)&0x7 == audioTypeOpus {
			_ = c.handleAudioPacket(pkt)
		}
		// Any other successfully-decrypted packet (e.g. a Ping reply) only
		// needed to serve as the liveness signal recorded above.
	}
}

func (c *Client) udpPingLoop(udpConn *net.UDPConn) {
	send := func() {
		_ = c.encryptAndSendUDP(udpConn, buildPingFrame())
	}

	for i := 0; i < udpRapidPings; i++ {
		send()
		select {
		case <-c.end:
			return
		case <-time.After(udpRapidPingInterval):
		}
	}

	ticker := time.NewTicker(udpSteadyPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.end:
			return
		case <-ticker.C:
			send()
		}
	}
}

func buildPingFrame() []byte {
	frame := make([]byte, 1+8)
	frame[0] = audioTypePing << 5
	binary.BigEndian.PutUint64(frame[1:], uint64(time.Now().UnixNano()))
	return frame
}

// tryWriteUDP attempts to send an already-built audio frame over the UDP
// voice channel. It returns false (letting the caller fall back to the TCP
// tunnel) whenever UDP isn't ready, hasn't been confirmed working recently,
// or the send itself fails.
func (c *Client) tryWriteUDP(frame []byte) bool {
	if !c.cryptReady.Load() {
		return false
	}
	udpConn := c.udpConn.Load()
	if udpConn == nil {
		return false
	}
	last := c.udpLastRecv.Load()
	if last == 0 || time.Since(time.Unix(0, last)) > udpStaleTimeout {
		return false
	}

	return c.encryptAndSendUDP(udpConn, frame) == nil
}

// encryptAndSendUDP encrypts plain into c.udpSendBuf (grown and reused
// across calls rather than allocated fresh each time — this runs once per
// outbound audio packet, tens of times a second) and writes it to udpConn.
//
// Encrypt and write happen atomically under encryptMu: Mumble's crypt IV is
// a strict per-connection counter, so if this and udpPingLoop's send() both
// encrypted (bumping the IV) before either wrote to the socket, whichever
// write lost the race would arrive with an IV out of sequence. The server's
// decrypt state machine tolerates loss but not reordering, and feeding Opus
// frames to the decoder out of order produces severe distortion. Holding
// the lock across the write serializes encrypt+send as one atomic unit
// against the other UDP sender — but never against udpReadLoop's Decrypt,
// which uses the separate decryptMu.
func (c *Client) encryptAndSendUDP(udpConn *net.UDPConn, plain []byte) error {
	c.encryptMu.Lock()
	need := len(plain) + c.crypt.Overhead()
	if cap(c.udpSendBuf) < need {
		c.udpSendBuf = make([]byte, need)
	}
	ct := c.udpSendBuf[:need]
	c.crypt.Encrypt(ct, plain)
	_, err := udpConn.Write(ct)
	c.encryptMu.Unlock()
	return err
}
