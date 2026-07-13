package mumble

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble/MumbleProto"
)

// Packet type IDs, per the Mumble control-channel protocol. Only the types
// this client actually sends or handles are named; anything else read off
// the wire is dispatched to a no-op default case.
const (
	ptVersion       = 0
	ptUDPTunnel     = 1
	ptAuthenticate  = 2
	ptPing          = 3
	ptReject        = 4
	ptServerSync    = 5
	ptChannelRemove = 6
	ptChannelState  = 7
	ptUserRemove    = 8
	ptUserState     = 9
	ptTextMessage   = 11
	ptCryptSetup    = 15
)

const maxPacketBytes = 10 * 1024 * 1024

// conn wraps the TLS control connection, handling the six-byte
// [type uint16][length uint32] framing shared by every control message.
type conn struct {
	net.Conn

	writeMu sync.Mutex
	readBuf []byte
}

func newConn(nc net.Conn) *conn {
	return &conn{Conn: nc}
}

// readPacket reads one framed control-channel packet.
func (c *conn) readPacket() (uint16, []byte, error) {
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	var header [6]byte
	if _, err := io.ReadFull(c.Conn, header[:]); err != nil {
		return 0, nil, err
	}
	pType := binary.BigEndian.Uint16(header[:2])
	pLen := binary.BigEndian.Uint32(header[2:])
	if int(pLen) > maxPacketBytes {
		return 0, nil, errors.New("mumble: packet exceeds maximum size")
	}
	if int(pLen) > len(c.readBuf) {
		c.readBuf = make([]byte, pLen)
	}
	buf := c.readBuf[:pLen]
	if _, err := io.ReadFull(c.Conn, buf); err != nil {
		return 0, nil, err
	}
	return pType, buf, nil
}

func (c *conn) writeHeader(pType uint16, pLen uint32) error {
	var header [6]byte
	binary.BigEndian.PutUint16(header[:2], pType)
	binary.BigEndian.PutUint32(header[2:], pLen)
	_, err := c.Conn.Write(header[:])
	return err
}

func protoPacketType(msg proto.Message) (uint16, bool) {
	switch msg.(type) {
	case *MumbleProto.Version:
		return ptVersion, true
	case *MumbleProto.Authenticate:
		return ptAuthenticate, true
	case *MumbleProto.Ping:
		return ptPing, true
	case *MumbleProto.ServerSync:
		return ptServerSync, true
	case *MumbleProto.ChannelRemove:
		return ptChannelRemove, true
	case *MumbleProto.ChannelState:
		return ptChannelState, true
	case *MumbleProto.UserRemove:
		return ptUserRemove, true
	case *MumbleProto.UserState:
		return ptUserState, true
	case *MumbleProto.TextMessage:
		return ptTextMessage, true
	case *MumbleProto.CryptSetup:
		return ptCryptSetup, true
	}
	return 0, false
}

// writeProto marshals and frames a protobuf control message.
func (c *conn) writeProto(msg proto.Message) error {
	pType, ok := protoPacketType(msg)
	if !ok {
		return fmt.Errorf("mumble: no packet type registered for %T", msg)
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.writeHeader(pType, uint32(len(data))); err != nil {
		return err
	}
	_, err = c.Conn.Write(data)
	return err
}

// writeAudioTunnel sends an already-framed audio packet (see audio.go) as a
// TCP-tunneled UDPTunnel control message. Used as the fallback path when the
// real UDP voice channel isn't (yet, or no longer) available.
func (c *conn) writeAudioTunnel(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.writeHeader(ptUDPTunnel, uint32(len(data))); err != nil {
		return err
	}
	_, err := c.Conn.Write(data)
	return err
}
