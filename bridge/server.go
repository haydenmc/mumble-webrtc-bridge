package bridge

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// stunURL is the public STUN server both the bridge and the browser use to
// discover their server-reflexive addresses.
const stunURL = "stun:stun.l.google.com:19302"

// turnCredentialTTL is how long a TURN credential minted from TURNSecret
// stays valid. TURN only checks it when an allocation is created or
// refreshed, and the browser refreshes its allocation for the life of the
// call, so this only needs to outlast the initial connect plus a margin.
// It's generous so a long-open tab that reconnects doesn't fail.
const turnCredentialTTL = 12 * time.Hour

// ICEConfig holds WebRTC ICE/TURN configuration passed to each peer.
type ICEConfig struct {
	// BridgeHost is the IP/hostname reachable by browsers. When set, Pion
	// advertises it as the host candidate instead of the container-internal IP.
	BridgeHost     string
	TURNURLs       []string
	TURNUsername   string
	TURNCredential string
	// TURNSecret is a coturn static-auth-secret. When set, per-peer
	// time-limited credentials are minted from it (TURN REST API scheme)
	// instead of handing every browser the static username/credential.
	TURNSecret string

	// UDPPortMin/UDPPortMax bound the ephemeral UDP port range pion draws
	// ICE candidates from. Both zero (the default) leaves pion's own
	// default (1-65535) in place, which is only reasonably reachable from
	// outside the container with --network host or similar. Setting a
	// narrow range makes it practical to publish just that range (e.g.
	// `-p 50000-50100:50000-50100/udp`) instead.
	UDPPortMin uint16
	UDPPortMax uint16

	// TCPPort, when non-zero, enables ICE-TCP on that port: one shared
	// listener for every peer, advertised as a passive TCP host candidate.
	// This lets browsers on networks that block UDP reach the bridge
	// directly, without a TURN server.
	TCPPort uint16
}

// Server manages the set of active bridge peers.
type Server struct {
	mumbleAddr    string
	mumbleChannel string
	ice           ICEConfig
	// forceTCP disables the UDP voice channel for every connection,
	// keeping audio on the TCP tunnel. A bisection tool for diagnosing
	// transport-specific audio issues, and an escape hatch for networks
	// that block/mangle UDP to the Mumble server.
	forceTCP bool

	// tcpMux is the shared ICE-TCP listener, nil unless ICEConfig.TCPPort
	// is set.
	tcpMux ice.TCPMux

	mu    sync.Mutex
	peers map[string]*Peer
}

func NewServer(mumbleAddr, mumbleChannel string, iceCfg ICEConfig, forceTCP bool) (*Server, error) {
	s := &Server{
		mumbleAddr:    mumbleAddr,
		mumbleChannel: mumbleChannel,
		ice:           iceCfg,
		forceTCP:      forceTCP,
		peers:         make(map[string]*Peer),
	}
	if iceCfg.TCPPort != 0 {
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{Port: int(iceCfg.TCPPort)})
		if err != nil {
			return nil, fmt.Errorf("ICE-TCP listen on port %d: %w", iceCfg.TCPPort, err)
		}
		s.tcpMux = webrtc.NewICETCPMux(nil, ln, 8)
		log.Printf("ICE-TCP enabled on port %d", iceCfg.TCPPort)
	}
	return s, nil
}

// iceServersFor returns the ICE server list for one peer. It's the single
// source of truth for both sides of the connection: the bridge's pion peer
// and the browser (sent in the "connected" message) get the same list.
func (s *Server) iceServersFor(peerID string) []webrtc.ICEServer {
	servers := []webrtc.ICEServer{{URLs: []string{stunURL}}}
	if len(s.ice.TURNURLs) == 0 {
		return servers
	}
	username, credential := s.ice.TURNUsername, s.ice.TURNCredential
	if s.ice.TURNSecret != "" {
		username, credential = turnRESTCredential(s.ice.TURNSecret, peerID, time.Now().Add(turnCredentialTTL))
	}
	return append(servers, webrtc.ICEServer{
		URLs:           s.ice.TURNURLs,
		Username:       username,
		Credential:     credential,
		CredentialType: webrtc.ICECredentialTypePassword,
	})
}

// turnRESTCredential mints a time-limited TURN credential in the format
// coturn's use-auth-secret mode expects: the username is
// "<unix expiry>:<label>" and the password is
// base64(HMAC-SHA1(secret, username)).
func turnRESTCredential(secret, label string, expiry time.Time) (username, credential string) {
	username = strconv.FormatInt(expiry.Unix(), 10) + ":" + label
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// HandleConn is called in a new goroutine for each incoming WebSocket connection.
func (s *Server) HandleConn(ws *websocket.Conn) {
	p := newPeer(ws, s)
	s.register(p)
	defer s.unregister(p)

	log.Printf("peer %s connected", p.id)
	p.run()
	log.Printf("peer %s disconnected", p.id)
}

func (s *Server) register(p *Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peers[p.id] = p
}

func (s *Server) unregister(p *Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.peers, p.id)
}
