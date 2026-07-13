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
  // TEMPORARY diagnostic fields — see downloadDebugRecording().
  private debugRecorder: MediaRecorder | null = null
  private debugChunks: Blob[] = []

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
      // sampleRate/channelCount: DIAGNOSTIC — explicitly requesting Opus's
      // native 48kHz mono to rule out WebRTC's real-time pipeline having
      // to resample from the mic's native rate (a likely source of
      // occasional glitches that wouldn't show up as anything unusual at
      // the RTP level, since the resampled PCM would just be garbled
      // before encoding into an otherwise-normal-looking Opus packet).
      // These are only *requested*; the browser can still pick something
      // else if the device doesn't support it — see the logged
      // getSettings() output below to confirm what was actually granted.
      // DIAGNOSTIC: DSP temporarily off again, now in combination with the
      // confirmed-correct 48kHz above, to rule out any interaction between
      // the two rather than assuming the earlier (pre-sample-rate-fix) DSP
      // test still applies. Revert to true/true/true once ruled out.
      audio: {
        noiseSuppression: false,
        echoCancellation: false,
        autoGainControl: false,
        sampleRate: 48000,
        channelCount: 1,
      },
      video: false,
    })
    const [micTrack] = stream.getAudioTracks()
    this.micTrack = micTrack
    console.log('mic track settings:', micTrack.getSettings())
    if (micTrack.getCapabilities) {
      console.log('mic track capabilities:', micTrack.getCapabilities())
    }
    // getSettings()/getCapabilities() don't reliably report sampleRate
    // across browsers (Firefox omits it entirely) — an AudioContext
    // attached to the same track is a more consistent way to see the
    // actual operating sample rate of the audio graph the browser built
    // for this device.
    try {
      const probeCtx = new AudioContext()
      probeCtx.createMediaStreamSource(stream)
      console.log('AudioContext sampleRate for this stream:', probeCtx.sampleRate)
      void probeCtx.close()
    } catch (err) {
      console.warn('AudioContext sample rate probe failed:', err)
    }

    // TEMPORARY diagnostic: record the raw mic stream locally, completely
    // independent of WebRTC transmission, so it can be compared against
    // what's heard over the bridge — if this recording is also garbled,
    // the problem is in capture itself (mic/OS/driver as the browser sees
    // it), before WebRTC's own Opus encoder or any network path is
    // involved; if it's clean, the problem is specifically in WebRTC's
    // internal audio pipeline. See downloadDebugRecording().
    if (typeof MediaRecorder !== 'undefined') {
      try {
        this.debugRecorder = new MediaRecorder(stream)
        this.debugRecorder.ondataavailable = (e) => {
          if (e.data.size > 0) this.debugChunks.push(e.data)
        }
        this.debugRecorder.start(1000)
      } catch (err) {
        console.warn('debug MediaRecorder unavailable:', err)
      }
    }

    // addTransceiver (not addTrack) so the mic always gets its own new m=
    // section. addTrack's spec behavior is to *reuse* an existing
    // compatible unassociated transceiver if one is available — it would
    // silently claim one of the REMOTE_SLOTS recvonly transceivers created
    // above instead of getting a dedicated line, throwing off the 1:1
    // mapping the bridge's track pool assumes.
    this.pc.addTransceiver(micTrack, { direction: 'sendrecv', streams: [stream] })
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

  // Silences outgoing audio when manually muted.
  //
  // VAD-gated auto-mute was tried twice here and removed both times — see
  // git history (frontend/src/client.ts prior to 2026-07-12) if picking
  // this back up. replaceTrack(null) between talk spurts caused real gaps
  // in the RTP sequence (audible as choppiness, not just irregular timing
  // an outbound pacer could smooth over) — VAD toggles on brief pauses
  // within normal speech far more than expected. Switching to
  // MediaStreamTrack.enabled kept the stream continuous, but a disabled
  // track is silent for every consumer of it, not just the outgoing RTP
  // sender — reusing one stream deadlocked VAD's own analysis (it starts
  // "not speaking", which disabled the track, which silenced what VAD
  // itself was listening to, so it could never detect speech to turn back
  // on), and even after fixing that with a second independent capture, the
  // stream was still choppy — and "transmitting silence" defeats the
  // point of a voice-activation indicator for other Mumble users anyway,
  // who'd see this client as talking constantly. Needs a real design (per
  // Mumble's own protocol, actually stopping/restarting transmission
  // between talk spurts without desyncing the receiving decoder is what
  // native clients do — see the "final" packet handling in
  // internal/mumble), not another quick patch.
  private updateTransmission(): void {
    if (this.micTrack) {
      this.micTrack.enabled = !this.manuallyMuted
    }
  }

  sendText(message: string): void {
    this.send({ type: 'text', message })
  }

  // TEMPORARY diagnostic: saves everything recorded so far (webm/opus,
  // playable in the browser or VLC) as a download, without interrupting
  // the ongoing recording. Call from the browser console:
  //   client.downloadDebugRecording()
  // (main.ts doesn't expose a button for this — it's a one-off debugging
  // aid, not a feature.)
  downloadDebugRecording(): void {
    if (this.debugChunks.length === 0) {
      console.warn('no debug recording data yet')
      return
    }
    const blob = new Blob(this.debugChunks, { type: 'audio/webm' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `mic-debug-${new Date().toISOString().replace(/[:.]/g, '-')}.webm`
    a.click()
    setTimeout(() => URL.revokeObjectURL(url), 10000)
  }

  disconnect(): void {
    this.cleanup()
    this.events.onDisconnected()
  }

  private cleanup(): void {
    this.manuallyMuted = false
    if (this.debugRecorder?.state !== 'inactive') {
      this.debugRecorder?.stop()
    }
    this.debugRecorder = null
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
