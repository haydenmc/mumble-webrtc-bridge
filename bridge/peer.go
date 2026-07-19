package bridge

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/hayden/mumble-webrtc-bridge/internal/mumble"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
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

// talkIdleTimeout is how long a session's audio can go quiet before the
// browser's "talking" indicator for it is cleared. Shorter than
// slotIdleTimeout since this only drives a UI cue, not track-pool
// reassignment, and packets normally arrive every ~20ms while someone is
// actually speaking.
const talkIdleTimeout = 300 * time.Millisecond

// opusClockRate is the RTP clock rate for Opus (always 48kHz, regardless of
// the actual audio sample rate) used to derive RTP timestamp increments.
const opusClockRate = 48000

// rtpGapThreshold: when the wall-clock gap since a slot's previous packet
// exceeds this, the RTP timestamp is advanced by the elapsed time (and the
// marker bit set) instead of by one frame duration. This makes the browser's
// jitter buffer see genuine silence between talk spurts rather than a stream
// whose timestamps are suddenly a gap-length behind — the latter is read as
// extreme lateness and inflates NetEq's adaptive playout delay toward ~1s.
const rtpGapThreshold = 100 * time.Millisecond

// trackSlot binds one pooled outbound WebRTC track to whichever Mumble
// session is currently occupying it (session 0 means free).
type trackSlot struct {
	track      *webrtc.TrackLocalStaticRTP
	session    uint32
	lastActive time.Time

	// Outbound RTP state, guarded by slotsMu. Seq/timestamp continuity
	// belongs to the track (SSRC), not the speaker, so this survives slot
	// reassignment: a new speaker just continues the sequence with a
	// forward timestamp jump, which reads as a normal talk-spurt boundary.
	rtpSeq    uint16
	rtpTS     uint32
	lastWrite time.Time
}

// talkEntry is one session's talking-indicator bookkeeping; see the
// talking field on Peer.
type talkEntry struct {
	name       string
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

	// talking tracks, per Mumble session, the last time audio was received
	// from them — used to drive the browser's talking indicator. Separate
	// from slots: audio for a session arrives via OnAudio regardless of
	// whether it currently holds a track slot. The name is cached alongside
	// the timestamp so the reaper can announce talking-stop without needing
	// a live *mumble.Client reference (the session may since have left).
	talkMu  sync.Mutex
	talking map[uint32]talkEntry

	closeCh   chan struct{}
	closeOnce sync.Once
}

func newPeer(ws *websocket.Conn, srv *Server) *Peer {
	return &Peer{
		id:      uuid.New().String(),
		ws:      ws,
		srv:     srv,
		talking: make(map[uint32]talkEntry),
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

	case "mute_deaf":
		var m muteDeafMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		p.handleMuteDeaf(m.Muted, m.Deafened)
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
			p.sendWS(mustMarshal(roomEventMsg{Type: "room_event", Kind: "joined", Username: name}))
		},
		OnUserLeft: func(mc *mumble.Client, name string) {
			p.sendWS(mustMarshal(userEventMsg{Type: "user_left", Username: name}))
			p.sendWS(mustMarshal(roomEventMsg{Type: "room_event", Kind: "left", Username: name}))
		},
		OnUserMoved: func(mc *mumble.Client) {
			p.sendWS(mustMarshal(userListMsg{Type: "user_list", Users: toUserInfos(mc.SelfChannelUsers())}))
		},
		OnUserMuteChanged: func(mc *mumble.Client, name string, muted, selfMuted bool) {
			p.sendWS(mustMarshal(muteStateMsg{
				Type: "mute_state", Username: name, Muted: muted, SelfMuted: selfMuted,
			}))
			kind := "unmuted"
			if muted || selfMuted {
				kind = "muted"
			}
			p.sendWS(mustMarshal(roomEventMsg{Type: "room_event", Kind: kind, Username: name}))
		},
		OnUserDeafChanged: func(mc *mumble.Client, name string, deafened, selfDeafened bool) {
			p.sendWS(mustMarshal(deafStateMsg{
				Type: "deaf_state", Username: name, Deafened: deafened, SelfDeafened: selfDeafened,
			}))
			kind := "undeafened"
			if deafened || selfDeafened {
				kind = "deafened"
			}
			p.sendWS(mustMarshal(roomEventMsg{Type: "room_event", Kind: kind, Username: name}))
		},
		OnAudio: func(mc *mumble.Client, session uint32, seq int64, final bool, opus []byte) {
			p.handleMumbleAudio(session, opus)
			p.markTalking(mc, session)
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
	if p.srv.ice.UDPPortMin != 0 || p.srv.ice.UDPPortMax != 0 {
		if err := se.SetEphemeralUDPPortRange(p.srv.ice.UDPPortMin, p.srv.ice.UDPPortMax); err != nil {
			return fmt.Errorf("webrtc UDP port range: %w", err)
		}
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
		track, err := webrtc.NewTrackLocalStaticRTP(
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
		// Random initial seq/timestamp per RFC 3550.
		p.slots[i] = &trackSlot{
			track:  track,
			rtpSeq: uint16(rand.Uint32()),
			rtpTS:  rand.Uint32(),
		}
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

// toUserInfos converts mumble roster snapshots to their wire representation.
func toUserInfos(statuses []mumble.UserStatus) []userInfo {
	infos := make([]userInfo, len(statuses))
	for i, s := range statuses {
		infos[i] = userInfo{
			Name:         s.Name,
			Muted:        s.Muted,
			SelfMuted:    s.SelfMuted,
			Deafened:     s.Deafened,
			SelfDeafened: s.SelfDeafened,
		}
	}
	return infos
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

// handleMuteDeaf reflects manual (button) mute+deafen to other Mumble clients
// via the self-mute/self-deaf flags, together in one call (deafening forces
// mute; un-deafening restores the prior mute state, so the browser always
// resolves both flags before sending). Remote audio silencing itself is
// handled entirely client-side; this only makes the mute/deaf state visible
// to other Mumble users. Both flags are sent in a single UserState packet —
// sending them as two separate packets (self-deaf then self-mute) causes
// some Mumble servers to broadcast the resulting "is now muted and deafened"
// notification to other clients twice.
func (p *Peer) handleMuteDeaf(muted, deafened bool) {
	p.muted.Store(muted)
	if p.mumble == nil {
		return
	}
	if err := p.mumble.SetSelfMuteDeaf(muted, deafened); err != nil {
		log.Printf("peer %s: set self mute/deaf: %v", p.id, err)
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

	durTicks := uint32(opusFrameDuration(opus).Nanoseconds() * opusClockRate / int64(time.Second))

	// Build the RTP header under slotsMu: OnAudio can fire concurrently from
	// the UDP read loop and the TCP tunnel dispatch loop, which interleave
	// during UDP<->TCP transitions.
	p.slotsMu.Lock()
	now := time.Now()
	marker := slot.lastWrite.IsZero()
	if gap := now.Sub(slot.lastWrite); !marker && gap > rtpGapThreshold {
		slot.rtpTS += uint32(gap.Nanoseconds() * opusClockRate / int64(time.Second))
		marker = true
	}
	slot.lastWrite = now
	hdr := rtp.Header{
		Version:        2,
		Marker:         marker,
		SequenceNumber: slot.rtpSeq,
		Timestamp:      slot.rtpTS,
	}
	slot.rtpSeq++
	slot.rtpTS += durTicks
	p.slotsMu.Unlock()

	if err := slot.track.WriteRTP(&rtp.Packet{Header: hdr, Payload: opus}); err != nil {
		log.Printf("peer %s: write remote rtp: %v", p.id, err)
	}
}

// markTalking records that session just sent an audio packet, notifying the
// browser of a talking-start transition the first time a session appears.
// Clearing back to not-talking happens on a timeout in reapSlots, since
// Mumble doesn't reliably signal end-of-talk-spurt.
func (p *Peer) markTalking(mc *mumble.Client, session uint32) {
	name := mc.UserName(session)
	if name == "" {
		return
	}

	p.talkMu.Lock()
	_, wasTalking := p.talking[session]
	p.talking[session] = talkEntry{name: name, lastActive: time.Now()}
	p.talkMu.Unlock()

	if !wasTalking {
		p.sendWS(mustMarshal(talkingMsg{Type: "talking", Username: name, Talking: true}))
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

			p.talkMu.Lock()
			var stopped []string
			for session, entry := range p.talking {
				if now.Sub(entry.lastActive) > talkIdleTimeout {
					stopped = append(stopped, entry.name)
					delete(p.talking, session)
				}
			}
			p.talkMu.Unlock()
			for _, name := range stopped {
				p.sendWS(mustMarshal(talkingMsg{Type: "talking", Username: name, Talking: false}))
			}
		}
	}
}

// readBrowserAudio reads Opus RTP from the browser and sends it directly to
// Mumble, one packet in, one packet out, in the order pion delivers them.
// No dedup, no reorder handling, no gap-driven Mumble sequence-number
// reconstruction — those were all tried, one at a time and in combination,
// while chasing an intermittent stutter, and none of it fixed the stutter.
// Deliberately going back to the simplest possible version so any further
// investigation starts from a known-plain baseline.
func (p *Peer) readBrowserAudio(track *webrtc.TrackRemote) {
	for {
		rtp, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if p.muted.Load() || p.mumble == nil {
			continue
		}

		// Mumble voice-packet sequence numbers count 10ms frames, not
		// packets: the receiving client schedules playback at
		// timestamp = frameNumber * 10ms (AudioOutputSpeech.cpp:
		// jbp.timestamp = iFrameSize * frameNumber, iFrameSize being 10ms
		// of samples), so a 20ms packet must advance the counter by 2.
		// Advancing by 1 per 20ms packet — as every earlier version of
		// this relay did — makes each listener's jitter-buffer timeline
		// run at half real time, so it chronically underruns and resyncs:
		// heard as a burst of stutters every few seconds, even in silence,
		// identically for every listener. Padding-only packets carry no
		// audio time and advance the counter by 0.
		units := int64(opusFrameDuration(rtp.Payload) / (10 * time.Millisecond))
		if len(rtp.Payload) == 0 {
			units = 0
		}
		seq := p.seqNum.Add(units) - units
		if err := p.mumble.WriteAudioPacket(0, seq, false, rtp.Payload); err != nil {
			log.Printf("peer %s: write audio: %v", p.id, err)
			return
		}
	}
}

// onMumbleConnect is called once the Mumble handshake completes. It may run
// before handleLogin's call to mumble.Dial has returned, so it must use mc
// rather than p.mumble (see the comment on cfg in handleLogin).
func (p *Peer) onMumbleConnect(mc *mumble.Client, welcome string) {
	p.sendWS(mustMarshal(userListMsg{Type: "user_list", Users: toUserInfos(mc.SelfChannelUsers())}))
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
