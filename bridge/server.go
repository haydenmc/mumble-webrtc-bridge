package bridge

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// ICEConfig holds WebRTC ICE/TURN configuration passed to each peer.
type ICEConfig struct {
	// BridgeHost is the IP/hostname reachable by browsers. When set, Pion
	// advertises it as the host candidate instead of the container-internal IP.
	BridgeHost     string
	TURNURLs       []string
	TURNUsername   string
	TURNCredential string

	// UDPPortMin/UDPPortMax bound the ephemeral UDP port range pion draws
	// ICE candidates from. Both zero (the default) leaves pion's own
	// default (1-65535) in place, which is only reasonably reachable from
	// outside the container with --network host or similar. Setting a
	// narrow range makes it practical to publish just that range (e.g.
	// `-p 50000-50100:50000-50100/udp`) instead.
	UDPPortMin uint16
	UDPPortMax uint16
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

	mu    sync.Mutex
	peers map[string]*Peer
}

func NewServer(mumbleAddr, mumbleChannel string, ice ICEConfig, forceTCP bool) *Server {
	return &Server{
		mumbleAddr:    mumbleAddr,
		mumbleChannel: mumbleChannel,
		ice:           ice,
		forceTCP:      forceTCP,
		peers:         make(map[string]*Peer),
	}
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

// ServeDebugRecording handles GET /debug/recording?peer=<id>&dir=out|in,
// dumping that peer's last ~60s of audio in the given direction as a
// downloadable Ogg Opus file (see Peer.recordOutBuf/recordInBuf /
// writeDebugRecording). dir=out (the default) is audio the bridge received
// from that peer's browser; dir=in is audio Mumble relayed back to that
// peer (e.g. from a second bridge session used as a loopback listener).
// TEMPORARY diagnostic endpoint for isolating whether garbled audio is
// already present before it ever leaves the browser, or only appears after
// a round trip through Mumble. Peer IDs are logged on connect (and in the
// "DIAG pkt" audio trace) — copy one from there.
func (s *Server) ServeDebugRecording(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("peer")
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = "out"
	}
	if dir != "out" && dir != "in" {
		http.Error(w, `dir must be "out" or "in"`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	p := s.peers[id]
	s.mu.Unlock()
	if p == nil {
		http.Error(w, "unknown peer id (check the server logs for a live one)", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "audio/ogg")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`-`+dir+`.opus"`)
	if err := p.writeDebugRecording(w, dir); err != nil {
		log.Printf("peer %s: debug recording: %v", id, err)
	}
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
