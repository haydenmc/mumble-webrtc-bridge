package bridge

import (
	"log"
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
}

// Server manages the set of active bridge peers.
type Server struct {
	mumbleAddr     string
	mumblePassword string
	mumbleChannel  string
	ice            ICEConfig

	mu    sync.Mutex
	peers map[string]*Peer
}

func NewServer(mumbleAddr, mumblePassword, mumbleChannel string, ice ICEConfig) *Server {
	return &Server{
		mumbleAddr:     mumbleAddr,
		mumblePassword: mumblePassword,
		mumbleChannel:  mumbleChannel,
		ice:            ice,
		peers:          make(map[string]*Peer),
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
