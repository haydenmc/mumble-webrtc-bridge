package mumble

import (
	"errors"

	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/varint"
)

const (
	audioTypeOpus = 4
	audioTypePing = 1
)

var errInvalidAudioPacket = errors.New("mumble: invalid audio packet")

// handleAudioPacket parses a decrypted (or TCP-tunneled, which is sent
// unencrypted) audio packet in server->client wire format:
//
//	[type<<5|target byte][session varint][seq varint][len|final-bit varint][opus bytes][optional 12-byte position]
//
// The Opus payload is passed through untouched — this client never decodes
// audio. Only type 4 (Opus) is understood; anything else (e.g. a Ping sent
// via the TCP tunnel, which real clients don't do but nothing forbids) is
// ignored.
func (c *Client) handleAudioPacket(buf []byte) error {
	if len(buf) < 1 {
		return errInvalidAudioPacket
	}
	audioType := (buf[0] >> 5) & 0x7
	buf = buf[1:]

	if audioType != audioTypeOpus {
		return nil
	}

	session, n := varint.Decode(buf)
	if n <= 0 {
		return errInvalidAudioPacket
	}
	buf = buf[n:]

	seq, n := varint.Decode(buf)
	if n <= 0 {
		return errInvalidAudioPacket
	}
	buf = buf[n:]

	length, n := varint.Decode(buf)
	if n <= 0 {
		return errInvalidAudioPacket
	}
	buf = buf[n:]

	final := length&0x2000 != 0
	opusLen := int(length &^ 0x2000)
	if opusLen < 0 || opusLen > len(buf) {
		return errInvalidAudioPacket
	}

	if c.cfg.OnAudio != nil {
		c.cfg.OnAudio(c, uint32(session), seq, final, buf[:opusLen])
	}
	return nil
}

// buildAudioFrame builds a client->server audio packet payload. Unlike the
// server->client format, no session id is included: the server identifies
// the sender from the connection (TCP tunnel) or crypt key (UDP).
func buildAudioFrame(target byte, seq int64, final bool, opus []byte) []byte {
	length := int64(len(opus))
	if final {
		length |= 0x2000
	}
	var header [1 + varint.MaxVarintLen*2]byte // fixed size keeps this on the stack
	header[0] = (audioTypeOpus << 5) | (target & 0x1F)
	n := 1
	n += varint.Encode(header[n:], seq)
	n += varint.Encode(header[n:], length)

	frame := make([]byte, n+len(opus))
	copy(frame, header[:n])
	copy(frame[n:], opus)
	return frame
}

// WriteAudioPacket sends an already Opus-encoded frame to the server,
// preferring the low-latency UDP voice channel when it's established and its
// liveness has been confirmed recently, and otherwise falling back to the
// TCP-tunneled path automatically.
func (c *Client) WriteAudioPacket(target byte, seq int64, final bool, opus []byte) error {
	frame := buildAudioFrame(target, seq, final, opus)
	if c.tryWriteUDP(frame) {
		return nil
	}
	return c.conn.writeAudioTunnel(frame)
}
