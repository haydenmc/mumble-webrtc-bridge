import { MicVAD } from '@ricky0123/vad-web'

// Must match remoteTrackSlots in bridge/peer.go: the bridge pre-negotiates
// this many outbound tracks, one per potential simultaneous Mumble speaker,
// and dynamically relays whichever sessions are talking onto them.
const REMOTE_SLOTS = 5

// forceOpusCBR appends constant-bitrate parameters to the opus fmtp line(s)
// in an SDP offer before it's sent. Diagnostic/mitigation for a suspected
// cause of intermittent garbled audio: WebRTC's opus encoder adaptively
// switches CELT bandwidth (SWB <-> FB) mid-stream, and each switch resets
// CELT's internal MDCT/overlap-add state, which can produce an audible
// glitch right at the switch. cbr=1 alone only pins bitrate, not bandwidth
// mode — libopus's automatic bandwidth selection is bitrate-dependent, and
// 64kbps sits close enough to the SWB/FB decision boundary that per-frame
// complexity variation can still tip it either way. Pushing the bitrate
// well clear of that boundary should stabilize the bandwidth choice.
// Applies to every m= section's opus fmtp line rather than just the mic's;
// the extra ones are on recvonly transceivers that never send anything, so
// it's a no-op there, not worth the complexity of targeting only the mic's
// line.
function forceOpusCBR(sdp: string): string {
  const pts = new Set<string>()
  for (const m of sdp.matchAll(/^a=rtpmap:(\d+) opus\/48000/gim)) {
    pts.add(m[1])
  }
  if (pts.size === 0) return sdp
  const fmtpLine = new RegExp(`^(a=fmtp:(?:${[...pts].join('|')}) )(.*)$`, 'gim')
  return sdp.replace(fmtpLine, (_match, prefix: string, params: string) => {
    const kept = params
      .split(';')
      .map((p) => p.trim())
      .filter((p) => p !== '' && !p.startsWith('cbr=') && !p.startsWith('maxaveragebitrate='))
    kept.push('cbr=1', 'maxaveragebitrate=128000')
    return prefix + kept.join(';')
  })
}

export type ServerMsg =
  | { type: 'connected' }
  | { type: 'error'; message: string }
  | { type: 'sdp'; sdpType: string; sdp: string }
  | { type: 'ice'; candidate: string; sdpMid: string; sdpMLineIndex: number }
  | { type: 'text'; from: string; message: string }
  | { type: 'user_list'; users: string[] }
  | { type: 'user_joined'; username: string }
  | { type: 'user_left'; username: string }

export interface ClientEvents {
  onConnected: () => void
  onError: (msg: string) => void
  onText: (from: string, message: string) => void
  onUserList: (users: string[]) => void
  onUserJoined: (username: string) => void
  onUserLeft: (username: string) => void
  onDisconnected: () => void
}

export class MumbleWebRTCClient {
  private ws: WebSocket | null = null
  private pc: RTCPeerConnection | null = null
  // One <audio> element per remote track slot, created lazily as each track
  // arrives during initial negotiation and reused for the life of the
  // connection — the bridge reassigns which Mumble user a slot carries, but
  // the track (and thus the element bound to it) stays the same. Multiple
  // simultaneously-playing elements mix automatically in the browser's
  // audio output, so no client-side mixing code is needed.
  private remoteAudioEls = new Map<string, HTMLAudioElement>()
  private manuallyMuted = false
  private micTrack: MediaStreamTrack | null = null
  private audioSender: RTCRtpSender | null = null
  private vad: MicVAD | null = null
  private vadSpeaking = false

  constructor(
    private events: ClientEvents,
    private turnURLs: string[] = [],
    private turnUsername: string = '',
    private turnCredential: string = '',
  ) {}

  connect(username: string, password: string): void {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const wsURL = `${proto}://${location.host}/ws`

    this.ws = new WebSocket(wsURL)

    this.ws.onopen = () => {
      this.send({ type: 'login', username, password })
    }

    this.ws.onclose = () => {
      this.cleanup()
      this.events.onDisconnected()
    }

    this.ws.onerror = () => {
      this.events.onError('WebSocket connection failed')
    }

    this.ws.onmessage = (evt) => {
      const msg = JSON.parse(evt.data as string) as ServerMsg
      this.handleServerMsg(msg)
    }
  }

  private async handleServerMsg(msg: ServerMsg): Promise<void> {
    switch (msg.type) {
      case 'connected':
        await this.startWebRTC()
        this.events.onConnected()
        break

      case 'error':
        this.events.onError(msg.message)
        break

      case 'sdp':
        if (!this.pc) return
        await this.pc.setRemoteDescription({ type: msg.sdpType as RTCSdpType, sdp: msg.sdp })
        break

      case 'ice':
        if (!this.pc) return
        try {
          await this.pc.addIceCandidate({
            candidate: msg.candidate,
            sdpMid: msg.sdpMid,
            sdpMLineIndex: msg.sdpMLineIndex,
          })
        } catch {
          // ignore stale candidates
        }
        break

      case 'text':
        this.events.onText(msg.from, msg.message)
        break

      case 'user_list':
        this.events.onUserList(msg.users)
        break

      case 'user_joined':
        this.events.onUserJoined(msg.username)
        break

      case 'user_left':
        this.events.onUserLeft(msg.username)
        break
    }
  }

  private async startWebRTC(): Promise<void> {
    const iceServers: RTCIceServer[] = [{ urls: 'stun:stun.l.google.com:19302' }]
    if (this.turnURLs.length > 0) {
      iceServers.push({
        urls: this.turnURLs,
        username: this.turnUsername,
        credential: this.turnCredential,
      })
    }

    this.pc = new RTCPeerConnection({ iceServers })

    this.pc.onicecandidate = (evt) => {
      if (!evt.candidate) return
      this.send({
        type: 'ice',
        candidate: evt.candidate.candidate,
        sdpMid: evt.candidate.sdpMid ?? '',
        sdpMLineIndex: evt.candidate.sdpMLineIndex ?? 0,
      })
    }

    this.pc.ontrack = (evt) => {
      const el = document.createElement('audio')
      el.autoplay = true
      el.setAttribute('playsinline', '')
      el.style.display = 'none'
      el.srcObject = new MediaStream([evt.track])
      document.body.appendChild(el)
      this.remoteAudioEls.set(evt.track.id, el)
    }

    this.pc.onconnectionstatechange = () => {
      if (
        this.pc?.connectionState === 'failed' ||
        this.pc?.connectionState === 'closed'
      ) {
        this.disconnect()
      }
    }

    // Pre-negotiate one recvonly slot per pooled bridge track, in addition
    // to this connection's own send/receive mic transceiver below. WebRTC
    // answerers can't add m= sections beyond what the offer contains, so
    // these must exist before createOffer() for the bridge's AddTrack calls
    // to have somewhere to land.
    for (let i = 0; i < REMOTE_SLOTS; i++) {
      this.pc.addTransceiver('audio', { direction: 'recvonly' })
    }

    const stream = await navigator.mediaDevices.getUserMedia({
      audio: { noiseSuppression: true, echoCancellation: true, autoGainControl: true },
      video: false,
    })
    const [micTrack] = stream.getAudioTracks()
    this.micTrack = micTrack

    // addTransceiver (not addTrack) so the mic always gets its own new m=
    // section. addTrack's spec behavior is to *reuse* an existing
    // compatible unassociated transceiver if one is available — it would
    // silently claim one of the REMOTE_SLOTS recvonly transceivers created
    // above instead of getting a dedicated line, throwing off the 1:1
    // mapping the bridge's track pool assumes.
    const transceiver = this.pc.addTransceiver(micTrack, { direction: 'sendrecv', streams: [stream] })
    this.audioSender = transceiver.sender

    // Voice-activity gate: reuses the same mic stream already flowing to the
    // sender rather than opening a second capture — VAD attaches its own
    // MediaStreamAudioSourceNode directly to `stream` via getStream() below,
    // which is independent of whatever the RTP sender currently references,
    // so toggling the sender's track (see updateTransmission) never affects
    // what VAD itself is listening to. Starts silent (like Mumble's
    // voice-activation mode) until speech is first detected — this only
    // controls whether audio actually leaves the browser, independent of
    // manual mute, which is the only thing reported to the server/other
    // Mumble users.
    this.vad = await MicVAD.new({
      model: 'v5',
      baseAssetPath: '/vad/',
      onnxWASMBasePath: '/vad/',
      getStream: async () => stream,
      pauseStream: async () => {},
      resumeStream: async () => stream,
      // Shorter than the library default (1400ms), which is tuned for not
      // truncating recorded speech segments rather than snappy cutoff.
      redemptionMs: 600,
      onSpeechStart: () => {
        this.vadSpeaking = true
        this.updateTransmission()
      },
      onSpeechEnd: () => {
        this.vadSpeaking = false
        this.updateTransmission()
      },
      onVADMisfire: () => {
        this.vadSpeaking = false
        this.updateTransmission()
      },
    })
    this.vad.start()
    this.updateTransmission()

    const offer = await this.pc.createOffer()
    const sdp = forceOpusCBR(offer.sdp ?? '')
    await this.pc.setLocalDescription({ type: offer.type, sdp })
    this.send({ type: 'sdp', sdpType: 'offer', sdp })
  }

  setMuted(muted: boolean): void {
    this.manuallyMuted = muted
    this.send({ type: 'mute', muted })
    this.updateTransmission()
  }

  // Stops audio leaving the browser entirely (rather than just having the
  // server drop it) whenever manually muted or VAD isn't hearing speech —
  // matching a native client's voice-activation mode, including not
  // showing as "talking" to other Mumble users during silence.
  //
  // VAD-gated auto-mute was tried and reverted twice before this (see git
  // history prior to 2026-07-12): replaceTrack(null) between talk spurts
  // was blamed for real gaps in the RTP sequence number, audible as
  // choppiness. That diagnosis no longer holds — the actual bug (fixed in
  // bridge/peer.go) was that the bridge derived Mumble's outgoing sequence
  // number from RTP sequence-number gaps, which was wrong on two counts:
  // Mumble's sequence counter is a 10ms-frame clock, not a packet counter,
  // and it has nothing to do with WebRTC's own RTP numbering in the first
  // place. The bridge no longer looks at RTP sequence numbers at all, so a
  // gap from replaceTrack (or anything else) can't desync it anymore.
  private updateTransmission(): void {
    const shouldTransmit = !this.manuallyMuted && this.vadSpeaking
    this.audioSender?.replaceTrack(shouldTransmit ? this.micTrack : null)
  }

  sendText(message: string): void {
    this.send({ type: 'text', message })
  }

  disconnect(): void {
    this.cleanup()
    this.events.onDisconnected()
  }

  private cleanup(): void {
    this.vad?.destroy()
    this.vad = null
    this.manuallyMuted = false
    this.vadSpeaking = false
    this.audioSender = null
    this.micTrack?.stop()
    this.micTrack = null
    this.pc?.close()
    this.pc = null
    this.ws?.close()
    this.ws = null
    for (const el of this.remoteAudioEls.values()) {
      el.srcObject = null
      el.remove()
    }
    this.remoteAudioEls.clear()
  }

  private send(obj: object): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(obj))
    }
  }
}
