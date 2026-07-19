import { MicVAD } from '@ricky0123/vad-web'
import { NoiseSuppressorWorklet_Name } from '@timephy/rnnoise-wasm'
import NoiseSuppressorWorkletUrl from '@timephy/rnnoise-wasm/NoiseSuppressorWorklet?worker&url'

// Must match remoteTrackSlots in bridge/peer.go: the bridge pre-negotiates
// this many outbound tracks, one per potential simultaneous Mumble speaker,
// and dynamically relays whichever sessions are talking onto them.
const REMOTE_SLOTS = 5

// Primary transmission gate: the Silero v5 neural VAD (via @ricky0123/vad-web),
// which distinguishes speech from other loud sounds (knocks, typing, coughs)
// far better than a bare level threshold. It analyzes the *raw* mic signal
// (pre-RNNoise) so denoising can't erase the onset it keys off. Model and
// runtime assets are served from /vad/ (vendored by scripts/copy-vad-assets.mjs).
const VAD_ASSET_PATH = '/vad/'
// Silero v5 consumes 512-sample frames at 16kHz. onSpeechStart fires as soon
// as the first frame crosses the positive threshold; onSpeechEnd fires after
// the redemption window of sub-negative-threshold frames.
const VAD_FRAME_SAMPLES = 512
const VAD_SAMPLE_RATE = 16000
// Silero speech-probability thresholds (0..1). The positive threshold is the
// probability the *first* frame of a word must reach for onSpeechStart to fire
// and open the gate, so it directly controls both sensitivity to short/soft
// words and how promptly the onset is detected (a high value delays the open
// past what the transmit delay line can cover, clipping the word). Kept at the
// library's sensitive default rather than raised — for a comms app, letting an
// occasional non-speech blip through beats swallowing quiet words. Tune by ear.
const VAD_POSITIVE_THRESHOLD = 0.3
const VAD_NEGATIVE_THRESHOLD = 0.2
// Hang time after speech drops before closing the gate — mirrors the old RMS
// gate's 500ms so brief pauses (a breath, a soft consonant) don't chop a
// sentence into fragments. vad-web converts this to whole frames internally.
const VAD_REDEMPTION_MS = 500

// Onset-protection delay, applied to the transmit path only (never to the
// signal the VAD analyzes). Derived from what Silero actually needs rather
// than hard-coded: an onset is detectable at worst one full 512/16000 = 32ms
// frame after it begins, so delaying the transmitted audio by that frame plus
// a small margin (inference, worklet→main-thread messaging, replaceTrack
// settling) lets the gate open before the first phoneme reaches the sender —
// no clipped word starts. Fixed for the session: changing DelayNode.delayTime
// live glitches audibly, so there's no runtime adaptation. The one onset this
// can't cover is the rare case where the first frame scores below threshold
// and only the second (~64ms in) trips it; no sub-50ms delay could.
const TRANSMIT_DELAY_S = VAD_FRAME_SAMPLES / VAD_SAMPLE_RATE + 0.012 // ~44ms

// Fallback transmission gate, used only when the Silero model or its worklet
// fails to load: RMS audio level against a fixed threshold. Cruder — any loud
// sound trips it, not just speech — but has zero model dependency, so it keeps
// voice activation working when the VAD assets are unavailable.
// Default RMS threshold (samples in [-1, 1]) — user-adjustable via the
// `loudnessThreshold` constructor option so a too-sensitive fallback gate
// (see issue #9) can be tuned without a rebuild. Only takes effect while the
// RMS fallback gate is actually active (see startLoudnessGate).
export const DEFAULT_LOUDNESS_THRESHOLD = 0.01
// Bounds for the user-configurable threshold — keeps a bad saved/URL value
// from producing a gate that's either permanently open (near 0) or can
// never open (near 1).
export const MIN_LOUDNESS_THRESHOLD = 0.001
export const MAX_LOUDNESS_THRESHOLD = 0.2
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
  // Deafen: silences every remote <audio> element's playback (and keeps
  // newly-arriving track slots silenced) for instant local silence, and is
  // also signalled to the Mumble server (see setDeafened) so other users see
  // us as deafened. Deafening also forces mute, mirroring a native client.
  private deafened = false
  // The manual-mute state captured when deafening, restored when un-deafening
  // (deaf implies mute, but un-deafen must not clobber a pre-existing mute).
  private muteBeforeDeafen = false
  private micTrack: MediaStreamTrack | null = null
  // The unprocessed hardware capture track, kept separately so cleanup() can
  // stop it: when RNNoise is on, micTrack is the *processed* destination
  // track, whose stop() does not release the microphone.
  private rawMicTrack: MediaStreamTrack | null = null
  private rnnoiseNode: AudioWorkletNode | null = null
  // Fixed onset-protection delay on the transmit path (see TRANSMIT_DELAY_S).
  private delayNode: DelayNode | null = null
  private audioSender: RTCRtpSender | null = null
  private audioCtx: AudioContext | null = null
  // Silero VAD, the primary transmit gate. Null when it failed to load and the
  // RMS loudness fallback (loudnessNode) is driving the gate instead.
  private vad: MicVAD | null = null
  private loudnessNode: AudioWorkletNode | null = null
  private loudnessSilenceTimer: number | null = null
  // Whether the active gate (VAD or RMS fallback) currently detects speech.
  private gateOpen = false
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
    private loudnessThreshold: number = DEFAULT_LOUDNESS_THRESHOLD,
  ) {
    this.loudnessThreshold = Math.min(
      MAX_LOUDNESS_THRESHOLD,
      Math.max(MIN_LOUDNESS_THRESHOLD, loudnessThreshold),
    )
  }

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
      el.muted = this.deafened
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

    // Build the send graph. Every noise-suppression mode routes through a
    // MediaStreamAudioDestinationNode now — not just 'rnnoise' — because the
    // onset-protection DelayNode (see below) has to live inside the audio
    // graph, so we can no longer shortcut 'browser'/'off' by sending the raw
    // capture track. Chain: micSource → [RNNoise] → delay → dest, and we send
    // dest's track.
    //
    //   RNNoise sits *before* the delay so the RMS fallback gate can tap the
    //   denoised-but-undelayed signal (rnnoiseNode) and keep its decision
    //   lead. Latency: ~13-16ms for RNNoise (10ms frame + a quantum or two)
    //   when enabled, plus TRANSMIT_DELAY_S for the delay line.
    let rnnoiseTap: AudioNode | null = null
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
        micSource.connect(this.rnnoiseNode)
        // Gate on the denoised signal so steady background noise (fans,
        // typing) no longer holds the RMS fallback gate open.
        rnnoiseTap = this.rnnoiseNode
      } catch (err) {
        this.rnnoiseNode = null
        console.warn('RNNoise unavailable, sending raw mic audio', err)
      }
    }

    // Fixed onset-protection delay in front of the destination. Forced mono to
    // match dest and to downmix a stereo mic consistently in the non-RNNoise
    // modes (previously those sent the capture track's channels as-is).
    this.delayNode = new DelayNode(this.audioCtx, {
      delayTime: TRANSMIT_DELAY_S,
      maxDelayTime: TRANSMIT_DELAY_S,
      channelCount: 1,
      channelCountMode: 'explicit',
      channelInterpretation: 'speakers',
    })
    const dest = new MediaStreamAudioDestinationNode(this.audioCtx, { channelCount: 1 })
    ;(rnnoiseTap ?? micSource).connect(this.delayNode)
    this.delayNode.connect(dest)
    const [sendTrack] = dest.stream.getAudioTracks()
    this.micTrack = sendTrack

    // addTransceiver (not addTrack) so the mic always gets its own new m=
    // section. addTrack's spec behavior is to *reuse* an existing
    // compatible unassociated transceiver if one is available — it would
    // silently claim one of the REMOTE_SLOTS recvonly transceivers created
    // above instead of getting a dedicated line, throwing off the 1:1
    // mapping the bridge's track pool assumes.
    const transceiver = this.pc.addTransceiver(sendTrack, {
      direction: 'sendrecv',
      streams: [dest.stream],
    })
    this.audioSender = transceiver.sender

    // Start the primary (Silero) gate on the *raw* capture stream. On failure
    // it falls back to the RMS loudness gate tapping the send-path signal
    // (RNNoise output when enabled, otherwise the raw mic source).
    await this.startVAD(stream, rnnoiseTap ?? micSource)

    this.updateTransmission()

    const offer = await this.pc.createOffer()
    const sdp = applyOpusOptions(offer.sdp ?? '', this.opusBitrateBps, this.opusLowDelay)
    await this.pc.setLocalDescription({ type: offer.type, sdp })
    this.send({ type: 'sdp', sdpType: 'offer', sdp })
  }

  // Starts the Silero VAD on the raw capture stream. The VAD builds its own
  // analysis node inside our AudioContext; it never touches the transmit
  // graph, so it sees the mic *before* the onset-protection delay and its
  // speech decision leads the audio actually being sent. If the model or its
  // worklet can't load, falls back to the RMS loudness gate on `fallbackTap`.
  private async startVAD(rawStream: MediaStream, fallbackTap: AudioNode): Promise<void> {
    try {
      this.vad = await MicVAD.new({
        model: 'v5',
        baseAssetPath: VAD_ASSET_PATH,
        onnxWASMBasePath: VAD_ASSET_PATH,
        audioContext: this.audioCtx!,
        processorType: 'AudioWorklet',
        startOnLoad: true,
        // Reuse our existing capture stream instead of letting vad-web open
        // its own getUserMedia. pauseStream must be a no-op: its default stops
        // the stream's tracks, which would kill the shared microphone.
        getStream: async () => rawStream,
        pauseStream: async () => {},
        resumeStream: async () => rawStream,
        positiveSpeechThreshold: VAD_POSITIVE_THRESHOLD,
        negativeSpeechThreshold: VAD_NEGATIVE_THRESHOLD,
        redemptionMs: VAD_REDEMPTION_MS,
        // We gate a live track; the pre-speech pad buffer vad-web would
        // otherwise assemble is dead weight here.
        preSpeechPadMs: 0,
        // Open the gate on the very first positive frame (onSpeechStart) and
        // always close it on onSpeechEnd — a nonzero minimum would suppress
        // onSpeechEnd for short segments and route them to onVADMisfire
        // instead, leaving the gate stuck open.
        minSpeechMs: 0,
        submitUserSpeechOnPause: false,
        onSpeechStart: () => this.setGateOpen(true),
        onSpeechEnd: () => this.setGateOpen(false),
        onVADMisfire: () => this.setGateOpen(false),
      })
    } catch (err) {
      console.warn('Silero VAD unavailable, falling back to RMS loudness gate', err)
      this.vad = null
      await this.startLoudnessGate(fallbackTap)
    }
  }

  // RMS loudness fallback gate: an AudioWorkletNode tapping the send-path
  // signal — see LOUDNESS_WORKLET_SOURCE for why a worklet instead of a poll.
  // Starts silent, like Mumble's voice-activation mode, until the first loud
  // window; a redemption timer keeps the gate open briefly after level drops.
  private async startLoudnessGate(tap: AudioNode): Promise<void> {
    const workletURL = URL.createObjectURL(
      new Blob([LOUDNESS_WORKLET_SOURCE], { type: 'application/javascript' }),
    )
    try {
      await this.audioCtx!.audioWorklet.addModule(workletURL)
    } finally {
      URL.revokeObjectURL(workletURL)
    }
    this.loudnessNode = new AudioWorkletNode(this.audioCtx!, 'loudness-processor')
    tap.connect(this.loudnessNode)
    this.loudnessNode.port.onmessage = (evt: MessageEvent<number>) => {
      const rms = evt.data
      if (rms >= this.loudnessThreshold) {
        if (this.loudnessSilenceTimer !== null) {
          clearTimeout(this.loudnessSilenceTimer)
          this.loudnessSilenceTimer = null
        }
        this.setGateOpen(true)
      } else if (this.gateOpen && this.loudnessSilenceTimer === null) {
        this.loudnessSilenceTimer = window.setTimeout(() => {
          this.loudnessSilenceTimer = null
          this.setGateOpen(false)
        }, LOUDNESS_REDEMPTION_MS)
      }
    }
  }

  // Records the active gate's speech/silence decision and pushes it through to
  // the RTP sender. Idempotent — no-ops if the state hasn't changed — so the
  // VAD's frame-rate callbacks and the RMS worklet's per-window messages can
  // both call it freely.
  private setGateOpen(open: boolean): void {
    if (this.gateOpen === open) return
    this.gateOpen = open
    this.updateTransmission()
  }

  setMuted(muted: boolean): void {
    this.manuallyMuted = muted
    this.send({ type: 'mute', muted })
    this.updateTransmission()
  }

  // Mutes/unmutes playback of every remote audio element and signals self-deaf
  // to the Mumble server so remote users see our deaf state. Deafening also
  // forces self-mute (a native client can't be deaf-but-transmitting);
  // un-deafening restores whatever mute state we had before deafening rather
  // than blindly unmuting.
  setDeafened(deafened: boolean): void {
    this.deafened = deafened
    for (const el of this.remoteAudioEls.values()) el.muted = deafened
    this.send({ type: 'deaf', deafened })
    if (deafened) {
      this.muteBeforeDeafen = this.manuallyMuted
      this.setMuted(true)
    } else {
      this.setMuted(this.muteBeforeDeafen)
    }
  }

  // Stops audio leaving the browser entirely (rather than just having the
  // server drop it) whenever manually muted or the speech gate is closed —
  // matching a native client's voice-activation mode: no RTP packets at all
  // during silence, not just silent ones, so it doesn't waste bandwidth or
  // show as "talking" to other Mumble users. Gated via replaceTrack, not
  // MediaStreamTrack.enabled — a disabled track is silent for every consumer
  // of the underlying stream, not just the RTP sender, which in the RMS
  // fallback would silence the loudness worklet's own input too since it taps
  // the same graph (see startLoudnessGate).
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
    const shouldTransmit = !this.manuallyMuted && this.gateOpen
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
    this.gateOpen = false
    this.selfTalking = false
    this.audioSender = null
    // Safe to destroy: pauseStream is our no-op (won't stop the shared mic)
    // and we don't own the AudioContext passed in, so destroy() won't close it.
    void this.vad?.destroy()
    this.vad = null
    this.loudnessNode?.port.close()
    this.loudnessNode?.disconnect()
    this.loudnessNode = null
    this.delayNode?.disconnect()
    this.delayNode = null
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
