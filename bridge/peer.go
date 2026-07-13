package bridge

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// remoteTrackSlots is the maximum number of Mumble users this bridge will
// relay to a browser simultaneously. Each slot is a pre-negotiated WebRTC
// track; Mumble sessions are dynamically assigned to a free (or, if the pool
// is full, least-recently-active) slot as they start talking. Must match
// REMOTE_SLOTS in frontend/src/client.ts.
const remoteTrackSlots = 5

// slotIdleTimeout is how long a slot stays bound to a session after its last
// packet before being freed for reassignment. Mumble doesn't reliably signal
// end-of-talk-spurt for Opus, so idle detection is timing-based rather than
// relying on the audio packet's "final" flag.
const slotIdleTimeout = 500 * time.Millisecond
const slotReapInterval = 250 * time.Millisecond

// trackSlot binds one pooled outbound WebRTC track to whichever Mumble
// session is currently occupying it (session 0 means free).
type trackSlot struct {
	track      *webrtc.TrackLocalStaticSample
	session    uint32
	lastActive time.Time
}

// Peer represents one browser user's bridge session.
type Peer struct {
	id  string
	ws  *websocket.Conn
	srv *Server

	wsMu sync.Mutex // serializes WebSocket writes

	pc *webrtc.PeerConnection

	mumble *mumble.Client

	muted  atomic.Bool
	seqNum atomic.Int64

	// slotsReady guards against Mumble audio arriving (on the mumble.Client's
	// internal goroutine, which can run concurrently with handleLogin/
	// setupWebRTC before Dial has even returned) before the track pool
	// below has been populated.
	slotsReady atomic.Bool
	slotsMu    sync.Mutex
	slots      [remoteTrackSlots]*trackSlot

	closeCh   chan struct{}
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
		p.handleMute(m.Muted)
	}
	return nil
}

func (p *Peer) handleLogin(username, password string) error {
	cfg := &mumble.Config{
		Username:   username,
		Password:   password,
		DisableUDP: p.srv.forceTCP,
		// Every callback below is passed the *mumble.Client explicitly (as mc,
		// distinct from the client variable Dial() returns below) and must
		// use it rather than p.mumble — the handshake can complete, and
		// OnConnect fire, from mumble.Client's internal goroutine before
		// Dial() has even returned to handleLogin, so p.mumble is not
		// guaranteed to be assigned yet when these run.
		OnConnect: func(mc *mumble.Client, welcome string) {
			p.onMumbleConnect(mc, welcome)
		},
		OnDisconnect: func(mc *mumble.Client, err error) {
			p.sendWS(mustMarshal(errorMsg{Type: "error", Message: "mumble disconnected"}))
			p.close()
		},
		OnTextMessage: func(mc *mumble.Client, from, message string) {
			// Drop server-generated ChannelListener warnings — our client doesn't
			// support the feature, but it doesn't affect bridge functionality.
			if from == "" && strings.Contains(message, "ChannelListener") {
				return
			}
			p.sendWS(mustMarshal(textMsg{Type: "text", From: from, Message: message}))
		},
		OnUserJoined: func(mc *mumble.Client, name string) {
			p.sendWS(mustMarshal(userEventMsg{Type: "user_joined", Username: name}))
		},
		OnUserLeft: func(mc *mumble.Client, name string) {
			p.sendWS(mustMarshal(userEventMsg{Type: "user_left", Username: name}))
		},
		OnUserMoved: func(mc *mumble.Client) {
			p.sendWS(mustMarshal(userListMsg{Type: "user_list", Users: mc.SelfChannelUsers()}))
		},
		OnAudio: func(mc *mumble.Client, session uint32, seq int64, final bool, opus []byte) {
			p.handleMumbleAudio(session, opus)
		},
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: true} // server may use self-signed cert
	client, err := mumble.Dial(p.srv.mumbleAddr, tlsCfg, cfg)
	if err != nil {
		p.sendWS(mustMarshal(errorMsg{Type: "error", Message: err.Error()}))
		return nil // don't tear down WS, let the browser retry
	}
	p.mumble = client

	// Optionally join a configured channel.
	if ch := p.srv.mumbleChannel; ch != "" {
		if err := client.JoinChannel(strings.Split(ch, "/")...); err != nil {
			log.Printf("peer %s: join channel: %v", p.id, err)
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

	// Outbound: a fixed pool of tracks, one per potential simultaneous
	// Mumble speaker. Raw Opus payloads are relayed straight through — no
	// server-side decode/mix/encode — with Mumble sessions dynamically
	// assigned to slots as they start talking (see handleMumbleAudio). The
	// browser plays each slot with its own <audio> element and lets the
	// browser's own audio mixing combine them.
	p.slotsMu.Lock()
	for i := 0; i < remoteTrackSlots; i++ {
		track, err := webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
			fmt.Sprintf("slot%d", i), "mumble",
		)
		if err != nil {
			p.slotsMu.Unlock()
			return err
		}
		if _, err := pc.AddTrack(track); err != nil {
			p.slotsMu.Unlock()
			return err
		}
		p.slots[i] = &trackSlot{track: track}
	}
	p.slotsMu.Unlock()
	// Mumble audio can start arriving (via OnAudio, on the mumble.Client's
	// internal goroutine) before this function returns — slotsReady gates
	// handleMumbleAudio until the pool above is actually populated.
	p.slotsReady.Store(true)

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

	go p.reapSlots()

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
	if p.mumble == nil {
		return
	}
	if err := p.mumble.SendChannelText(message); err != nil {
		log.Printf("peer %s: send text: %v", p.id, err)
	}
}

// handleMute reflects manual (button) mute to other Mumble clients via the
// self-mute flag. Automatic voice-activity gating is handled entirely
// client-side and never reaches here.
func (p *Peer) handleMute(muted bool) {
	p.muted.Store(muted)
	if p.mumble == nil {
		return
	}
	if err := p.mumble.SetSelfMuted(muted); err != nil {
		log.Printf("peer %s: set self muted: %v", p.id, err)
	}
}

// handleMumbleAudio relays one Mumble user's raw Opus packet to whichever
// pooled WebRTC track their session is (or becomes) assigned to.
func (p *Peer) handleMumbleAudio(session uint32, opus []byte) {
	if !p.slotsReady.Load() {
		// setupWebRTC (called after mumble.Dial returns) hasn't populated
		// the track pool yet; drop this packet rather than race it.
		return
	}
	slot := p.assignSlot(session)
	if slot == nil {
		return // pool exhausted; drop this speaker's audio
	}
	if err := slot.track.WriteSample(media.Sample{
		Data:     opus,
		Duration: opusFrameDuration(opus),
	}); err != nil {
		log.Printf("peer %s: write remote sample: %v", p.id, err)
	}
}

// assignSlot returns the track slot bound to session, assigning a free (or,
// if the pool is full, the least-recently-active) slot to it first if
// needed.
func (p *Peer) assignSlot(session uint32) *trackSlot {
	p.slotsMu.Lock()
	defer p.slotsMu.Unlock()

	now := time.Now()
	var free, lru *trackSlot
	freeIdx, lruIdx := -1, -1
	for i, s := range p.slots {
		if s.session == session {
			s.lastActive = now
			return s
		}
		if s.session == 0 && free == nil {
			free, freeIdx = s, i
		}
		if lru == nil || s.lastActive.Before(lru.lastActive) {
			lru, lruIdx = s, i
		}
	}

	target, idx := free, freeIdx
	if target == nil {
		target, idx = lru, lruIdx
		if target != nil {
			log.Printf("peer %s: track pool full, reassigning slot %d (session %d -> %d)", p.id, idx, target.session, session)
		}
	} else {
		log.Printf("peer %s: assigned session %d to slot %d", p.id, session, idx)
	}
	if target == nil {
		return nil
	}
	target.session = session
	target.lastActive = now
	return target
}

// reapSlots periodically frees slots that have gone quiet, so they can be
// reassigned to a different speaker.
func (p *Peer) reapSlots() {
	ticker := time.NewTicker(slotReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.closeCh:
			return
		case <-ticker.C:
			p.slotsMu.Lock()
			now := time.Now()
			for _, s := range p.slots {
				if s.session != 0 && now.Sub(s.lastActive) > slotIdleTimeout {
					s.session = 0
				}
			}
			p.slotsMu.Unlock()
		}
	}
}

// readBrowserAudio reads Opus RTP from the browser and sends it directly to Mumble.
// talkSpurtGapThreshold bounds how long a gap between browser RTP packets
// can be before it's treated as a fresh talk spurt (the client's VAD pausing
// transmission via replaceTrack(null) between utterances) rather than real
// network loss within a continuous stream. VAD only cuts transmission after
// its redemption window (600ms of silence, see frontend/src/client.ts), so
// any gap shorter than that is assumed to be ordinary jitter/loss; anything
// longer is assumed to be an intentional pause.
const talkSpurtGapThreshold = 300 * time.Millisecond

// outboundFrame is one Opus packet queued for pacing (see paceOutboundAudio).
type outboundFrame struct {
	seq  int64
	data []byte
}

// outboundPaceInterval matches the ~20ms Opus frame duration WebRTC browsers
// use by default. outboundQueueDepth bounds how much jitter gets absorbed
// before frames are dropped rather than let latency grow.
const outboundPaceInterval = 20 * time.Millisecond
const outboundQueueDepth = 3

func (p *Peer) readBrowserAudio(track *webrtc.TrackRemote) {
	queue := make(chan outboundFrame, outboundQueueDepth)
	go p.paceOutboundAudio(queue)
	defer close(queue)

	var haveSeq bool
	var lastRTPSeq uint16
	var lastPacketTime time.Time
	for {
		rtp, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if p.muted.Load() || p.mumble == nil {
			continue
		}
		now := time.Now()

		// Advance the Mumble sequence number by the real RTP gap so a
		// browser->bridge packet loss shows up to the receiving Opus
		// decoders as a gap (letting them apply loss concealment) instead
		// of being silently smoothed into a continuous stream, which
		// otherwise produces audible glitches for other Mumble users.
		//
		// Exception: a long gap most likely means our own VAD gate paused
		// transmission between talk spurts, not that packets were lost.
		// Signaling that as loss would make receivers run concealment to
		// "fill in" audio that was never supposed to exist, which is
		// audible as stuttery/robotic artifacts right at the start of every
		// new utterance — so a long gap resumes fresh instead.
		delta := int64(1)
		if haveSeq {
			gap := rtp.SequenceNumber - lastRTPSeq // uint16 wraparound arithmetic
			switch {
			case gap == 0:
				continue // duplicate
			case now.Sub(lastPacketTime) > talkSpurtGapThreshold:
				delta = 1 // treat as a new talk spurt, not loss
			case gap > 0x8000:
				// Old/out-of-order relative to what we already forwarded;
				// skip it rather than feeding a stateful Opus decoder audio
				// out of order.
				continue
			default:
				delta = int64(gap)
			}
		}
		haveSeq = true
		lastRTPSeq = rtp.SequenceNumber
		lastPacketTime = now

		seq := p.seqNum.Add(delta) - 1
		// rtp.Payload's backing array may be reused by pion after this loop
		// iteration; the pacer goroutine sends it later, so it needs its
		// own copy.
		frame := outboundFrame{seq: seq, data: append([]byte(nil), rtp.Payload...)}
		select {
		case queue <- frame:
		default:
			// The pacer has fallen behind — drop the oldest queued frame
			// rather than let latency grow unbounded.
			select {
			case <-queue:
			default:
			}
			select {
			case queue <- frame:
			default:
			}
		}
	}
}

// paceOutboundAudio dispatches queued browser audio to Mumble on a steady
// tick instead of the instant each RTP packet arrives, so that any jitter in
// the browser's own send timing (encoder/DSP processing variance, scheduler
// hiccups) gets smoothed out rather than relayed straight through as
// irregular delivery timing — which otherwise sounds like small, consistent
// stutters even when no packets are actually being lost.
func (p *Peer) paceOutboundAudio(queue <-chan outboundFrame) {
	ticker := time.NewTicker(outboundPaceInterval)
	defer ticker.Stop()
	for range ticker.C {
		select {
		case frame, ok := <-queue:
			if !ok {
				return
			}
			if p.mumble == nil {
				continue
			}
			if err := p.mumble.WriteAudioPacket(0, frame.seq, false, frame.data); err != nil {
				log.Printf("peer %s: write audio: %v", p.id, err)
				return
			}
		default:
			// Browser paused sending (mute, or between talk spurts); nothing
			// queued this tick.
		}
	}
}

// onMumbleConnect is called once the Mumble handshake completes. It may run
// before handleLogin's call to mumble.Dial has returned, so it must use mc
// rather than p.mumble (see the comment on cfg in handleLogin).
func (p *Peer) onMumbleConnect(mc *mumble.Client, welcome string) {
	p.sendWS(mustMarshal(userListMsg{Type: "user_list", Users: mc.SelfChannelUsers()}))
	if welcome != "" {
		p.sendWS(mustMarshal(textMsg{Type: "text", From: "", Message: welcome}))
	}
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
