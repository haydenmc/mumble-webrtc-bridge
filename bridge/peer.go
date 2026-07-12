package bridge

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/hraban/opus"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"layeh.com/gumble/gumble"
	"layeh.com/gumble/gumbleutil"
)

// Peer represents one browser user's bridge session.
type Peer struct {
	id  string
	ws  *websocket.Conn
	srv *Server

	wsMu sync.Mutex // serializes WebSocket writes

	pc       *webrtc.PeerConnection
	outTrack *webrtc.TrackLocalStaticSample

	mumble *gumble.Client

	muted  atomic.Bool
	seqNum atomic.Int64

	// audio mixing: accumulates PCM from all speaking Mumble users
	mixMu  sync.Mutex
	mixBuf []int16

	closeCh chan struct{}
	closeOnce sync.Once
}

func newPeer(ws *websocket.Conn, srv *Server) *Peer {
	return &Peer{
		id:      uuid.New().String(),
		ws:      ws,
		srv:     srv,
		closeCh: make(chan struct{}),
	}
}

// run is the main loop: reads WebSocket messages until the connection closes.
func (p *Peer) run() {
	defer p.close()
	for {
		_, msg, err := p.ws.ReadMessage()
		if err != nil {
			return
		}
		if err := p.handleMessage(msg); err != nil {
			log.Printf("peer %s message error: %v", p.id, err)
			return
		}
	}
}

func (p *Peer) handleMessage(raw []byte) error {
	var env msgEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}

	switch env.Type {
	case "login":
		var m loginMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		return p.handleLogin(m.Username, m.Password)

	case "sdp":
		var m sdpMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		return p.handleSDP(m)

	case "ice":
		var m iceMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		return p.handleICE(m)

	case "text":
		var m textMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		p.handleText(m.Message)

	case "mute":
		var m muteMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		p.muted.Store(m.Muted)
	}
	return nil
}

func (p *Peer) handleLogin(username, password string) error {
	mumbleCfg := gumble.NewConfig()
	mumbleCfg.Username = username
	mumbleCfg.Password = password
	mumbleCfg.AudioListeners.Attach(p)
	mumbleCfg.Listeners.Attach(gumbleutil.Listener{
		Connect: func(e *gumble.ConnectEvent) {
			p.onMumbleConnect(e)
		},
		Disconnect: func(e *gumble.DisconnectEvent) {
			p.sendWS(mustMarshal(errorMsg{Type: "error", Message: "mumble disconnected"}))
			p.close()
		},
		TextMessage: func(e *gumble.TextMessageEvent) {
			// Drop server-generated ChannelListener warnings — our client doesn't
			// support the feature, but it doesn't affect bridge functionality.
			if e.Sender == nil && strings.Contains(e.Message, "ChannelListener") {
				return
			}
			sender := ""
			if e.Sender != nil {
				sender = e.Sender.Name
			}
			p.sendWS(mustMarshal(textMsg{
				Type:    "text",
				From:    sender,
				Message: e.Message,
			}))
		},
		UserChange: func(e *gumble.UserChangeEvent) {
			p.onUserChange(e)
		},
	})

	tlsCfg := &tls.Config{InsecureSkipVerify: true} // server may use self-signed cert
	client, err := gumble.DialWithDialer(new(net.Dialer), p.srv.mumbleAddr, mumbleCfg, tlsCfg)
	if err != nil {
		p.sendWS(mustMarshal(errorMsg{Type: "error", Message: err.Error()}))
		return nil // don't tear down WS, let the browser retry
	}
	p.mumble = client

	// Optionally join a configured channel.
	if ch := p.srv.mumbleChannel; ch != "" {
		parts := strings.Split(ch, "/")
		if target := client.Channels.Find(parts...); target != nil {
			client.Self.Move(target)
		}
	}

	// Set up WebRTC peer connection.
	if err := p.setupWebRTC(); err != nil {
		p.sendWS(mustMarshal(errorMsg{Type: "error", Message: "webrtc setup: " + err.Error()}))
		return nil
	}

	// Notify browser that Mumble connection succeeded.
	p.sendWS(mustMarshal(connectedMsg{Type: "connected"}))
	return nil
}

func (p *Peer) setupWebRTC() error {
	iceServers := []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}
	if len(p.srv.ice.TURNURLs) > 0 {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:           p.srv.ice.TURNURLs,
			Username:       p.srv.ice.TURNUsername,
			Credential:     p.srv.ice.TURNCredential,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}

	se := webrtc.SettingEngine{}
	if host := p.srv.ice.BridgeHost; host != "" {
		se.SetNAT1To1IPs([]string{host}, webrtc.ICECandidateTypeHost)
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		return err
	}
	p.pc = pc

	// Outbound track: Mumble audio → browser.
	outTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "mumble",
	)
	if err != nil {
		return err
	}
	p.outTrack = outTrack
	if _, err := pc.AddTrack(outTrack); err != nil {
		return err
	}

	// Inbound track: browser audio → Mumble.
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go p.readBrowserAudio(track)
	})

	// Forward ICE candidates to browser.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		mid := ""
		if init.SDPMid != nil {
			mid = *init.SDPMid
		}
		idx := uint16(0)
		if init.SDPMLineIndex != nil {
			idx = *init.SDPMLineIndex
		}
		p.sendWS(mustMarshal(iceMsg{
			Type:          "ice",
			Candidate:     init.Candidate,
			SDPMid:        mid,
			SDPMLineIndex: idx,
		}))
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			p.close()
		}
	})

	// Start audio mixer.
	go p.runMixer()

	return nil
}

func (p *Peer) handleSDP(m sdpMsg) error {
	if p.pc == nil {
		return fmt.Errorf("webrtc not initialised")
	}
	sdpType := webrtc.SDPTypeOffer
	if m.SDPType == "answer" {
		sdpType = webrtc.SDPTypeAnswer
	}
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: sdpType,
		SDP:  m.SDP,
	}); err != nil {
		return err
	}
	if sdpType == webrtc.SDPTypeOffer {
		answer, err := p.pc.CreateAnswer(nil)
		if err != nil {
			return err
		}
		if err := p.pc.SetLocalDescription(answer); err != nil {
			return err
		}
		p.sendWS(mustMarshal(sdpMsg{
			Type:    "sdp",
			SDPType: "answer",
			SDP:     answer.SDP,
		}))
	}
	return nil
}

func (p *Peer) handleICE(m iceMsg) error {
	if p.pc == nil {
		return nil
	}
	mid := m.SDPMid
	idx := m.SDPMLineIndex
	return p.pc.AddICECandidate(webrtc.ICECandidateInit{
		Candidate:     m.Candidate,
		SDPMid:        &mid,
		SDPMLineIndex: &idx,
	})
}

func (p *Peer) handleText(message string) {
	if p.mumble == nil || p.mumble.Self == nil || p.mumble.Self.Channel == nil {
		return
	}
	p.mumble.Self.Channel.Send(message, false)
}

// OnAudioStream implements gumble.AudioListener. Called once per speaking user per stream.
func (p *Peer) OnAudioStream(e *gumble.AudioStreamEvent) {
	go func() {
		for pkt := range e.C {
			p.addPCM([]int16(pkt.AudioBuffer))
		}
	}()
}

// addPCM mixes incoming PCM into the 20ms accumulator buffer.
func (p *Peer) addPCM(pcm []int16) {
	const frameSize = 960 // 20ms at 48kHz
	p.mixMu.Lock()
	defer p.mixMu.Unlock()
	if p.mixBuf == nil {
		p.mixBuf = make([]int16, frameSize)
	}
	for i := 0; i < len(pcm) && i < frameSize; i++ {
		s := int32(p.mixBuf[i]) + int32(pcm[i])
		if s > 32767 {
			s = 32767
		} else if s < -32768 {
			s = -32768
		}
		p.mixBuf[i] = int16(s)
	}
}

// runMixer fires every 20ms, encodes the mixed PCM buffer, and writes to the WebRTC track.
func (p *Peer) runMixer() {
	const frameSize = 960
	enc, err := opus.NewEncoder(48000, 1, opus.AppVoIP)
	if err != nil {
		log.Printf("peer %s: opus encoder: %v", p.id, err)
		return
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.closeCh:
			return
		case <-ticker.C:
			p.mixMu.Lock()
			frame := p.mixBuf
			p.mixBuf = nil
			p.mixMu.Unlock()

			if frame == nil {
				continue
			}
			if len(frame) < frameSize {
				padded := make([]int16, frameSize)
				copy(padded, frame)
				frame = padded
			}

			opusData, err := encodeOpus(enc, frame)
			if err != nil {
				log.Printf("peer %s: encode: %v", p.id, err)
				continue
			}
			if p.outTrack != nil {
				_ = p.outTrack.WriteSample(media.Sample{
					Data:     opusData,
					Duration: 20 * time.Millisecond,
				})
			}
		}
	}
}

// readBrowserAudio reads Opus RTP from the browser and sends it directly to Mumble.
func (p *Peer) readBrowserAudio(track *webrtc.TrackRemote) {
	for {
		rtp, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if p.muted.Load() || p.mumble == nil {
			continue
		}
		seq := p.seqNum.Add(1) - 1
		if err := p.mumble.Conn.WriteAudio(4, 0, seq, false, rtp.Payload, nil, nil, nil); err != nil {
			log.Printf("peer %s: write audio: %v", p.id, err)
			return
		}
	}
}

// onMumbleConnect is called once the Mumble handshake completes.
// p.mumble is not yet assigned at this point (DialWithDialer hasn't returned),
// so we read the user list directly from the ConnectEvent's client.
func (p *Peer) onMumbleConnect(e *gumble.ConnectEvent) {
	names := []string{}
	if e.Client != nil && e.Client.Self != nil && e.Client.Self.Channel != nil {
		for _, u := range e.Client.Self.Channel.Users {
			names = append(names, u.Name)
		}
	}
	p.sendWS(mustMarshal(userListMsg{Type: "user_list", Users: names}))
}

func (p *Peer) onUserChange(e *gumble.UserChangeEvent) {
	if e.Type.Has(gumble.UserChangeConnected) {
		p.sendWS(mustMarshal(userEventMsg{Type: "user_joined", Username: e.User.Name}))
	}
	if e.Type.Has(gumble.UserChangeDisconnected) {
		p.sendWS(mustMarshal(userEventMsg{Type: "user_left", Username: e.User.Name}))
	}
	if e.Type.Has(gumble.UserChangeChannel) {
		// Refresh full list when someone moves channels.
		users := p.channelUsers()
		p.sendWS(mustMarshal(userListMsg{Type: "user_list", Users: users}))
	}
}

func (p *Peer) channelUsers() []string {
	names := []string{}
	if p.mumble == nil || p.mumble.Self == nil || p.mumble.Self.Channel == nil {
		return names
	}
	for _, u := range p.mumble.Self.Channel.Users {
		names = append(names, u.Name)
	}
	return names
}

func (p *Peer) sendWS(data []byte) {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	_ = p.ws.WriteMessage(websocket.TextMessage, data)
}

func (p *Peer) close() {
	p.closeOnce.Do(func() {
		close(p.closeCh)
		if p.pc != nil {
			_ = p.pc.Close()
		}
		if p.mumble != nil {
			_ = p.mumble.Disconnect()
		}
		_ = p.ws.Close()
	})
}
