import { NoiseSuppressorWorklet_Name } from '@timephy/rnnoise-wasm'
import NoiseSuppressorWorkletUrl from '@timephy/rnnoise-wasm/NoiseSuppressorWorklet?worker&url'

// Must match remoteTrackSlots in bridge/peer.go: the bridge pre-negotiates
// this many outbound tracks, one per potential simultaneous Mumble speaker,
// and dynamically relays whichever sessions are talking onto them.
const REMOTE_SLOTS = 5

// Naive loudness-based transmission gate: no VAD model, just RMS audio
// level compared to a fixed threshold. Trades accuracy (any loud sound
// triggers it — a knock, a cough, typing — not just speech) for zero
// inference latency, zero model weight/dependency, and a much simpler
// implementation. A real VAD is a possible future upgrade; deliberately
// not doing that here.
const LOUDNESS_THRESHOLD = 0.01 // RMS of samples in [-1, 1]; tune by ear
// How long to keep transmitting after the last loud sample before gating
// closed again — avoids chopping speech into fragments at every brief dip
// below threshold (a pause for breath, a soft consonant).
const LOUDNESS_REDEMPTION_MS = 500
// Samples per RMS window inside the worklet (see LOUDNESS_WORKLET_SOURCE) —
// 256 at 48kHz is ~5.3ms, matching a run of 2 render quantums (128 samples
// each). Small enough to react quickly, large enough that postMessage
// back to the main thread isn't firing on every single quantum. Shrunk
// from an initial 512 (~10.7ms) after still-clipped speech onsets — this
// window is the dominant term in the gate's total decision latency, so
// halving it directly shaves latency off every onset.
const LOUDNESS_WORKLET_WINDOW_SAMPLES = 256

// Runs on the dedicated audio rendering thread, not the main thread, so
// unlike a setInterval/AnalyserNode poll it (a) never misses any audio —
// every sample delivered gets included in exactly one RMS window, versus
// a poll that only ever sees an AnalyserNode's most recent snapshot and
// can go long stretches without looking at anything in between — and
// (b) keeps running even when the tab is backgrounded, since browsers
// don't throttle Web Audio's rendering thread the way they throttle
// timers. Inlined via a Blob URL rather than a separate vendored asset
// file (contrast the old ONNX-based VAD's worklet bundle) since this is
// a few lines of plain RMS math, not a model runtime.
const LOUDNESS_WORKLET_SOURCE = `
class LoudnessProcessor extends AudioWorkletProcessor {
  constructor() {
    super()
    this.buffer = new Float32Array(${LOUDNESS_WORKLET_WINDOW_SAMPLES})
    this.filled = 0
  }
  process(inputs) {
    const channel = inputs[0]?.[0]
    if (!channel) return true
    for (let i = 0; i < channel.length; i++) {
      this.buffer[this.filled++] = channel[i]
      if (this.filled === this.buffer.length) {
        let sumSquares = 0
        for (const sample of this.buffer) sumSquares += sample * sample
        this.port.postMessage(Math.sqrt(sumSquares / this.buffer.length))
        this.filled = 0
      }
    }
    return true
  }
}
registerProcessor('loudness-processor', LoudnessProcessor)
`

// applyOpusOptions rewrites the opus fmtp line(s) in an SDP offer before
// it's sent, pinning constant bitrate at the user-configured value. Applies
// to every m= section's opus fmtp line rather than just the mic's; the
// extra ones are on recvonly transceivers that never send anything, so
// it's a no-op there, not worth the complexity of targeting only the mic's
// line.
//
// When lowDelay is set, also shrinks the Opus frame size (SDP ptime) from
// the browser's ~20ms default to 10ms. Browsers don't expose libopus's
// internal low-delay application mode through the WebRTC API, so this is
// the closest real lever: a smaller frame means less algorithmic buffering
// latency, at the cost of more packet overhead. Some browsers (e.g.
// Firefox) don't emit a=ptime/a=maxptime in the offer at all unless a
// non-default value is requested, so these lines must be inserted when
// absent rather than only rewritten when present.
function applyOpusOptions(sdp: string, bitrateBps: number, lowDelay: boolean): string {
  const pts = new Set<string>()
  for (const m of sdp.matchAll(/^a=rtpmap:(\d+) opus\/48000/gim)) {
    pts.add(m[1])
  }
  if (pts.size === 0) return sdp
  const fmtpLine = new RegExp(`^(a=fmtp:(?:${[...pts].join('|')}) )(.*)$`, 'gim')

  // Process one m= section at a time so ptime/maxptime insertion (which is
  // per-section, unlike fmtp which is per-payload-type) lands next to the
  // right section's fmtp line rather than only the first one in the SDP.
  const sections = sdp.split(/(?=^m=)/im)
  return sections
    .map((section) => {
      fmtpLine.lastIndex = 0
      if (!fmtpLine.test(section)) return section
      let result = section.replace(fmtpLine, (_match, prefix: string, params: string) => {
        const kept = params
          .split(';')
          .map((p) => p.trim())
          .filter((p) => p !== '' && !p.startsWith('cbr=') && !p.startsWith('maxaveragebitrate='))
        kept.push('cbr=1', `maxaveragebitrate=${bitrateBps}`)
        return prefix + kept.join(';')
      })
      if (lowDelay) {
        const hasPtime = /^a=ptime:/im.test(result)
        const hasMaxptime = /^a=maxptime:/im.test(result)
        result = result.replace(/^a=ptime:\d+$/im, 'a=ptime:10')
        result = result.replace(/^a=maxptime:\d+$/im, 'a=maxptime:10')
        const toInsert = [
          ...(hasPtime ? [] : ['a=ptime:10']),
          ...(hasMaxptime ? [] : ['a=maxptime:10']),
        ]
        if (toInsert.length > 0) {
          result = result.replace(/^(a=fmtp:.*)$/im, (m) => `${m}\n${toInsert.join('\n')}`)
        }
      }
      return result
    })
    .join('')
}

export interface UserInfo {
  name: string
  muted: boolean
  selfMuted: boolean
  deafened: boolean
  selfDeafened: boolean
}

export type RoomEventKind = 'joined' | 'left' | 'muted' | 'unmuted' | 'deafened' | 'undeafened'

export type NoiseSuppressionMode = 'rnnoise' | 'browser' | 'off'

export type ServerMsg =
  | { type: 'connected' }
  | { type: 'error'; message: string }
  | { type: 'sdp'; sdpType: string; sdp: string }
  | { type: 'ice'; candidate: string; sdpMid: string; sdpMLineIndex: number }
  | { type: 'text'; from: string; message: string }
  | { type: 'user_list'; users: UserInfo[] }
  | { type: 'user_joined'; username: string }
  | { type: 'user_left'; username: string }
  | { type: 'mute_state'; username: string; muted: boolean; selfMuted: boolean }
  | { type: 'deaf_state'; username: string; deafened: boolean; selfDeafened: boolean }
  | { type: 'talking'; username: string; talking: boolean }
  | { type: 'room_event'; kind: RoomEventKind; username: string }

export interface ClientEvents {
  onConnected: () => void
  onError: (msg: string) => void
  onText: (from: string, message: string) => void
  onUserList: (users: UserInfo[]) => void
  onUserJoined: (username: string) => void
  onUserLeft: (username: string) => void
  onMuteState: (username: string, muted: boolean, selfMuted: boolean) => void
  onDeafState: (username: string, deafened: boolean, selfDeafened: boolean) => void
  onTalking: (username: string, talking: boolean) => void
  onRoomEvent: (kind: RoomEventKind, username: string) => void
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
  // The unprocessed hardware capture track, kept separately so cleanup() can
  // stop it: when RNNoise is on, micTrack is the *processed* destination
  // track, whose stop() does not release the microphone.
  private rawMicTrack: MediaStreamTrack | null = null
  private rnnoiseNode: AudioWorkletNode | null = null
  private audioSender: RTCRtpSender | null = null
  private audioCtx: AudioContext | null = null
  private loudnessNode: AudioWorkletNode | null = null
  private loudnessSilenceTimer: number | null = null
  private loudEnough = false
  private username = ''
  private selfTalking = false

  constructor(
    private events: ClientEvents,
    private turnURLs: string[] = [],
    private turnUsername: string = '',
    private turnCredential: string = '',
    private opusBitrateBps: number = 96000,
    private opusLowDelay: boolean = true,
    private noiseSuppressionMode: NoiseSuppressionMode = 'rnnoise',
    private autoGainControl: boolean = true,
    private echoCancellation: boolean = true,
  ) {}

  connect(username: string, password: string): void {
    this.username = username
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

      case 'mute_state':
        this.events.onMuteState(msg.username, msg.muted, msg.selfMuted)
        break

      case 'deaf_state':
        this.events.onDeafState(msg.username, msg.deafened, msg.selfDeafened)
        break

      case 'talking':
        this.events.onTalking(msg.username, msg.talking)
        break

      case 'room_event':
        this.events.onRoomEvent(msg.kind, msg.username)
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
      // Keep the receiver's jitter-buffer floor at zero. Note this is a
      // *minimum* target in Chrome, not a cap, so it can't shrink an already
      // inflated buffer — the actual latency fix is bridge-side RTP
      // timestamping across silence gaps. This just guards against anything
      // ever raising the floor. No-ops where unsupported (Safari < 17.4).
      const receiver = evt.receiver as RTCRtpReceiver & {
        jitterBufferTarget?: number | null
      }
      try {
        receiver.jitterBufferTarget = 0
      } catch {
        /* unsupported / read-only */
      }

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
      // Only ask the browser for noise suppression in 'browser' mode —
      // stacking it with RNNoise introduces musical-noise artifacts.
      audio: {
        noiseSuppression: this.noiseSuppressionMode === 'browser',
        echoCancellation: this.echoCancellation,
        autoGainControl: this.autoGainControl,
      },
      video: false,
    })
    const [micTrack] = stream.getAudioTracks()
    this.rawMicTrack = micTrack

    // Pinned to 48kHz: RNNoise processes 480-sample frames at exactly 48kHz
    // and the worklet does not resample, so a 44.1kHz context (the macOS
    // default) would produce garbled output. The browser resamples the mic
    // to the context rate transparently, and Opus is 48kHz downstream
    // anyway. Created before addTransceiver because the processed send track
    // must exist before the transceiver is built.
    this.audioCtx = new AudioContext({ sampleRate: 48000 })
    // A suspended context would emit a silent outgoing track (not just a
    // dead loudness gate). connect() runs from a user-gesture submit handler
    // so the context should already start running; this is cheap insurance.
    void this.audioCtx.resume()
    const micSource = this.audioCtx.createMediaStreamSource(stream)

    // Route the mic through RNNoise into a MediaStreamAudioDestinationNode
    // and send that destination's track. Adds ~13-16ms latency (10ms RNNoise
    // frame buffering plus a render quantum or two). On any failure, fall
    // back to sending the raw mic track unprocessed.
    let sendTrack = micTrack
    let sendStream = stream
    let loudnessTap: AudioNode = micSource
    if (this.noiseSuppressionMode === 'rnnoise') {
      try {
        await this.audioCtx.audioWorklet.addModule(NoiseSuppressorWorkletUrl)
        // Force mono I/O. The worklet only ever writes channel 0 of its
        // output; if the mic delivers a stereo stream the node would default
        // to two output channels (channelCountMode 'max') and leave the
        // right channel silent, so listeners hear the speaker in their left
        // ear only. Explicit mono also downmixes a stereo mic to one channel
        // before denoising, which is what we want for voice anyway.
        this.rnnoiseNode = new AudioWorkletNode(this.audioCtx, NoiseSuppressorWorklet_Name, {
          channelCount: 1,
          channelCountMode: 'explicit',
          channelInterpretation: 'speakers',
          numberOfInputs: 1,
          numberOfOutputs: 1,
          outputChannelCount: [1],
        })
        const dest = new MediaStreamAudioDestinationNode(this.audioCtx, { channelCount: 1 })
        micSource.connect(this.rnnoiseNode)
        this.rnnoiseNode.connect(dest)
        sendStream = dest.stream
        ;[sendTrack] = dest.stream.getAudioTracks()
        // Gate on the denoised signal so steady background noise (fans,
        // typing) no longer holds the transmit gate open.
        loudnessTap = this.rnnoiseNode
      } catch (err) {
        this.rnnoiseNode = null
        console.warn('RNNoise unavailable, sending raw mic audio', err)
      }
    }
    this.micTrack = sendTrack

    // addTransceiver (not addTrack) so the mic always gets its own new m=
    // section. addTrack's spec behavior is to *reuse* an existing
    // compatible unassociated transceiver if one is available — it would
    // silently claim one of the REMOTE_SLOTS recvonly transceivers created
    // above instead of getting a dedicated line, throwing off the 1:1
    // mapping the bridge's track pool assumes.
    const transceiver = this.pc.addTransceiver(sendTrack, { direction: 'sendrecv', streams: [sendStream] })
    this.audioSender = transceiver.sender

    // Loudness gate: an AudioWorkletNode tapping the send-path signal
    // (RNNoise output when enabled, otherwise the raw mic source),
    // independent of whatever the RTP sender currently references — see
    // LOUDNESS_WORKLET_SOURCE for why a worklet instead of a poll. Starts
    // silent, like Mumble's voice-activation mode, until the first loud
    // window.
    const workletURL = URL.createObjectURL(
      new Blob([LOUDNESS_WORKLET_SOURCE], { type: 'application/javascript' }),
    )
    try {
      await this.audioCtx.audioWorklet.addModule(workletURL)
    } finally {
      URL.revokeObjectURL(workletURL)
    }
    this.loudnessNode = new AudioWorkletNode(this.audioCtx, 'loudness-processor')
    loudnessTap.connect(this.loudnessNode)
    this.loudnessNode.port.onmessage = (evt: MessageEvent<number>) => {
      const rms = evt.data
      if (rms >= LOUDNESS_THRESHOLD) {
        if (this.loudnessSilenceTimer !== null) {
          clearTimeout(this.loudnessSilenceTimer)
          this.loudnessSilenceTimer = null
        }
        if (!this.loudEnough) {
          this.loudEnough = true
          this.updateTransmission()
        }
      } else if (this.loudEnough && this.loudnessSilenceTimer === null) {
        this.loudnessSilenceTimer = window.setTimeout(() => {
          this.loudnessSilenceTimer = null
          this.loudEnough = false
          this.updateTransmission()
        }, LOUDNESS_REDEMPTION_MS)
      }
    }

    this.updateTransmission()

    const offer = await this.pc.createOffer()
    const sdp = applyOpusOptions(offer.sdp ?? '', this.opusBitrateBps, this.opusLowDelay)
    await this.pc.setLocalDescription({ type: offer.type, sdp })
    this.send({ type: 'sdp', sdpType: 'offer', sdp })
  }

  setMuted(muted: boolean): void {
    this.manuallyMuted = muted
    this.send({ type: 'mute', muted })
    this.updateTransmission()
  }

  // Stops audio leaving the browser entirely (rather than just having the
  // server drop it) whenever manually muted or the loudness gate isn't
  // triggered — matching a native client's voice-activation mode: no RTP
  // packets at all during silence, not just silent ones, so it doesn't
  // waste bandwidth or show as "talking" to other Mumble users. Gated via
  // replaceTrack, not MediaStreamTrack.enabled — a disabled track is
  // silent for every consumer of the underlying stream, not just the RTP
  // sender, which would silence the AnalyserNode's own input too since it
  // taps the same stream (see startWebRTC).
  //
  // Earlier VAD-gated auto-mute attempts (see git history prior to
  // 2026-07-12) blamed replaceTrack(null) between talk spurts for real gaps
  // in the RTP sequence number, audible as choppiness. That diagnosis
  // doesn't hold anymore: the actual bug (fixed in bridge/peer.go) was that
  // the bridge derived Mumble's outgoing sequence number from RTP
  // sequence-number gaps, unrelated to WebRTC's own RTP numbering and wrong
  // regardless of VAD. The bridge no longer looks at RTP sequence numbers
  // at all, so replaceTrack can't desync it.
  private updateTransmission(): void {
    const shouldTransmit = !this.manuallyMuted && this.loudEnough
    this.audioSender?.replaceTrack(shouldTransmit ? this.micTrack : null)

    // Self-talking never round-trips through the server — the bridge's
    // OnAudio callback only sees other sessions' incoming packets, never
    // this browser's own outgoing ones — so it's reported locally instead.
    if (shouldTransmit !== this.selfTalking) {
      this.selfTalking = shouldTransmit
      this.events.onTalking(this.username, shouldTransmit)
    }
  }

  sendText(message: string): void {
    this.send({ type: 'text', message })
  }

  disconnect(): void {
    this.cleanup()
    this.events.onDisconnected()
  }

  private cleanup(): void {
    if (this.loudnessSilenceTimer !== null) {
      clearTimeout(this.loudnessSilenceTimer)
      this.loudnessSilenceTimer = null
    }
    this.manuallyMuted = false
    this.loudEnough = false
    this.selfTalking = false
    this.audioSender = null
    this.loudnessNode?.port.close()
    this.loudnessNode?.disconnect()
    this.loudnessNode = null
    this.rnnoiseNode?.disconnect()
    this.rnnoiseNode = null
    void this.audioCtx?.close()
    this.audioCtx = null
    this.micTrack?.stop()
    this.micTrack = null
    // Separate from micTrack — when RNNoise is on, micTrack is the processed
    // track and only stopping rawMicTrack releases the hardware mic.
    this.rawMicTrack?.stop()
    this.rawMicTrack = null
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
