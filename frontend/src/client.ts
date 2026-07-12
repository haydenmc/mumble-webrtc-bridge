import { MicVAD } from '@ricky0123/vad-web'

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
  private audioEl: HTMLAudioElement
  private vad: MicVAD | null = null
  private manuallyMuted = false
  private vadSpeaking = false
  private micTrack: MediaStreamTrack | null = null
  private audioSender: RTCRtpSender | null = null

  constructor(
    private events: ClientEvents,
    private turnURLs: string[] = [],
    private turnUsername: string = '',
    private turnCredential: string = '',
  ) {
    this.audioEl = document.getElementById('remote-audio') as HTMLAudioElement
  }

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
      if (evt.streams[0]) {
        this.audioEl.srcObject = evt.streams[0]
      }
    }

    this.pc.onconnectionstatechange = () => {
      if (
        this.pc?.connectionState === 'failed' ||
        this.pc?.connectionState === 'closed'
      ) {
        this.disconnect()
      }
    }

    const stream = await navigator.mediaDevices.getUserMedia({ audio: true, video: false })
    const [micTrack] = stream.getAudioTracks()
    this.micTrack = micTrack
    this.audioSender = this.pc.addTrack(micTrack, stream)

    // Voice-activity gate: reuses the same mic stream already flowing to the
    // peer connection rather than opening a second capture. Starts silent
    // (like Mumble's voice-activation mode) until speech is first detected.
    // This only controls whether audio actually leaves the browser
    // (via replaceTrack below) — it's independent of manual mute, which is
    // the only thing reported to the server/other Mumble users.
    this.vad = await MicVAD.new({
      model: 'v5',
      baseAssetPath: '/vad/',
      onnxWASMBasePath: '/vad/',
      getStream: async () => stream,
      pauseStream: async () => {},
      resumeStream: async () => stream,
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
    await this.pc.setLocalDescription(offer)
    this.send({ type: 'sdp', sdpType: 'offer', sdp: offer.sdp ?? '' })
  }

  setMuted(muted: boolean): void {
    this.manuallyMuted = muted
    this.send({ type: 'mute', muted })
    this.updateTransmission()
  }

  // Stops audio leaving the browser entirely (rather than just having the
  // server drop it) whenever manually muted or the VAD isn't hearing speech.
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
    this.micTrack = null
    this.audioSender = null
    this.pc?.close()
    this.pc = null
    this.ws?.close()
    this.ws = null
    this.audioEl.srcObject = null
  }

  private send(obj: object): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(obj))
    }
  }
}
