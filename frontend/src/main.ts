import { MumbleWebRTCClient, type NoiseSuppressionMode, type RoomEventKind, type UserInfo } from './client'
import { playSound, setSoundsEnabled } from './sounds'

// --- DOM refs ---
const viewLogin = document.getElementById('view-login')!
const viewRoom = document.getElementById('view-room')!
const loginForm = document.getElementById('login-form') as HTMLFormElement
const usernameInput = document.getElementById('username') as HTMLInputElement
const passwordInput = document.getElementById('password') as HTMLInputElement
const soundsToggle = document.getElementById('sounds-toggle') as HTMLInputElement
const bitrateSelect = document.getElementById('bitrate-select') as HTMLSelectElement
const lowDelayToggle = document.getElementById('low-delay-toggle') as HTMLInputElement
const autoGainToggle = document.getElementById('auto-gain-toggle') as HTMLInputElement
const echoCancellationToggle = document.getElementById('echo-cancellation-toggle') as HTMLInputElement
const noiseSuppressionSelect = document.getElementById('noise-suppression-select') as HTMLSelectElement
const loginError = document.getElementById('login-error')!
const connectBtn = document.getElementById('connect-btn') as HTMLButtonElement
const muteBtn = document.getElementById('mute-btn') as HTMLButtonElement
const muteBtnIcon = muteBtn.querySelector('.btn-icon') as HTMLSpanElement
const deafenBtn = document.getElementById('deafen-btn') as HTMLButtonElement
const deafenBtnIcon = deafenBtn.querySelector('.btn-icon') as HTMLSpanElement
const disconnectBtn = document.getElementById('disconnect-btn') as HTMLButtonElement
const disconnectBtnMobile = document.getElementById('disconnect-btn-mobile') as HTMLButtonElement
const userList = document.getElementById('user-list')!
const userCount = document.getElementById('user-count')!
const chatMessages = document.getElementById('chat-messages')!
const chatForm = document.getElementById('chat-form') as HTMLFormElement
const chatInput = document.getElementById('chat-input') as HTMLInputElement
const chatPanel = document.querySelector('.chat-panel') as HTMLElement
const sheetToggle = document.getElementById('sheet-toggle')!
const sheetLatest = document.getElementById('sheet-latest')!

let client: MumbleWebRTCClient | null = null
let muted = false
let deafened = false
let currentUsername = ''

// --- Inline SVG icons (stroke inherits currentColor / CSS) ---
const MIC_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="16" height="16"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><line x1="12" x2="12" y1="19" y2="22"/></svg>'
const MIC_OFF_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="16" height="16"><line x1="2" x2="22" y1="2" y2="22"/><path d="M18.89 13.23A7.12 7.12 0 0 0 19 12v-2"/><path d="M5 10v2a7 7 0 0 0 12 5"/><path d="M15 9.34V5a3 3 0 0 0-5.68-1.33"/><path d="M9 9v3a3 3 0 0 0 5.12 2.12"/><line x1="12" x2="12" y1="19" y2="22"/></svg>'
// Header deafen glyph — headphones, crossed out when deafened.
const DEAFEN_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="20" height="20"><path d="M3 14h3a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-5a9 9 0 0 1 18 0v5a2 2 0 0 1-2 2h-1a2 2 0 0 1-2-2v-3a2 2 0 0 1 2-2h3"/></svg>'
const DEAFEN_OFF_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="20" height="20"><line x1="2" x2="22" y1="2" y2="22"/><path d="M3 14h3a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-5a9 9 0 0 1 13.4-7.83"/><path d="M20.6 8.4A9 9 0 0 1 21 12v5.5"/><path d="M18 21a2 2 0 0 1-2-2v-3"/></svg>'
// User-list status glyphs — stroke color comes from CSS (.icon-muted / .icon-deafened).
const USER_MIC_OFF_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="15" height="15"><line x1="2" x2="22" y1="2" y2="22"/><path d="M18.89 13.23A7.12 7.12 0 0 0 19 12v-2"/><path d="M5 10v2a7 7 0 0 0 12 5"/><path d="M15 9.34V5a3 3 0 0 0-5.68-1.33"/><path d="M9 9v3a3 3 0 0 0 5.12 2.12"/><line x1="12" x2="12" y1="19" y2="22"/></svg>'
const USER_DEAF_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="15" height="15"><line x1="2" x2="22" y1="2" y2="22"/><path d="M3 14h3a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-5a9 9 0 0 1 13.4-7.83"/><path d="M20.6 8.4A9 9 0 0 1 21 12v5.5"/><path d="M18 21a2 2 0 0 1-2-2v-3"/></svg>'

// --- Per-user avatar color (stable hash → palette; reused for chat .from) ---
// 32 colors, generated once so large rooms rarely repeat. Hues are spread
// evenly around the wheel; lightness dips through the high-luminance
// yellow/green/cyan band (min ~hue 100) and rises for the dark blues/violets
// (max ~hue 280) so the white avatar initial stays legible on every swatch.
const AVATAR_COLORS = Array.from({ length: 32 }, (_, i) => {
  const hue = (i * 360) / 32
  const light = 51 + 13 * Math.cos(((hue - 280) * Math.PI) / 180)
  return `hsl(${hue.toFixed(0)}, 60%, ${light.toFixed(0)}%)`
})
function colorFor(name: string): string {
  let h = 0
  for (const c of name) h = (h * 31 + c.charCodeAt(0)) >>> 0
  return AVATAR_COLORS[h % AVATAR_COLORS.length]
}

// --- Client setup ---
function createClient(
  bitrateBps: number,
  lowDelay: boolean,
  noiseSuppressionMode: NoiseSuppressionMode,
  autoGainControl: boolean,
  echoCancellation: boolean,
): MumbleWebRTCClient {
  return new MumbleWebRTCClient(
    {
      onConnected() {
        playSound('join')
        showRoom()
      },
      onError(msg) {
        showLoginError(msg)
        connectBtn.disabled = false
        connectBtn.textContent = 'Connect'
      },
      onText(from, message) {
        if (from) playSound('message')
        appendMessage(from, message)
      },
      onUserList(users) {
        setUserList(users)
      },
      onUserJoined(username) {
        playSound('join')
        addUser({ name: username, muted: false, selfMuted: false, deafened: false, selfDeafened: false })
      },
      onUserLeft(username) {
        playSound('leave')
        removeUser(username)
      },
      onMuteState(username, muted, selfMuted) {
        updateUserState(username, { muted, selfMuted })
      },
      onDeafState(username, deafened, selfDeafened) {
        updateUserState(username, { deafened, selfDeafened })
      },
      onTalking(username, talking) {
        setTalking(username, talking)
      },
      onRoomEvent(kind, username) {
        appendRoomEvent(kind, username)
      },
      onDisconnected() {
        // Only a genuine post-connect disconnect counts as "leaving" — a
        // failed login attempt closes the socket too, but never showed the
        // room view, so it shouldn't play the leave sound.
        if (!viewRoom.classList.contains('hidden')) playSound('leave')
        showLogin()
      },
    },
    // TURN config — populated via template or env if needed
    [],
    '',
    '',
    bitrateBps,
    lowDelay,
    noiseSuppressionMode,
    autoGainControl,
    echoCancellation,
  )
}

// --- Persist credentials ---
const STORAGE_KEY = 'mumble-bridge-credentials'

function loadCredentials(): void {
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (!saved) return
    const { username, password } = JSON.parse(saved)
    if (username) usernameInput.value = username
    if (password) passwordInput.value = password
  } catch {
    // ignore malformed storage
  }
}

function saveCredentials(username: string, password: string): void {
  localStorage.setItem(STORAGE_KEY, JSON.stringify({ username, password }))
}

loadCredentials()

// --- Persist advanced options ---
const ADVANCED_OPTIONS_STORAGE_KEY = 'mumble-bridge-advanced-options'
const DEFAULT_BITRATE_BPS = 96000

const VALID_NOISE_SUPPRESSION_MODES: NoiseSuppressionMode[] = ['rnnoise', 'browser', 'off']

function loadAdvancedOptions(): void {
  try {
    const saved = localStorage.getItem(ADVANCED_OPTIONS_STORAGE_KEY)
    if (!saved) return
    const { bitrateBps, lowDelay, autoGainControl, echoCancellation, noiseSuppressionMode, soundsEnabled } =
      JSON.parse(saved)
    if (bitrateBps) bitrateSelect.value = String(bitrateBps)
    if (lowDelay !== undefined) lowDelayToggle.checked = Boolean(lowDelay)
    if (autoGainControl !== undefined) autoGainToggle.checked = Boolean(autoGainControl)
    if (echoCancellation !== undefined) echoCancellationToggle.checked = Boolean(echoCancellation)
    if (VALID_NOISE_SUPPRESSION_MODES.includes(noiseSuppressionMode)) {
      noiseSuppressionSelect.value = noiseSuppressionMode
    }
    if (soundsEnabled !== undefined) soundsToggle.checked = Boolean(soundsEnabled)
  } catch {
    // ignore malformed storage
  }
  setSoundsEnabled(soundsToggle.checked)
}

function saveAdvancedOptions(
  bitrateBps: number,
  lowDelay: boolean,
  autoGainControl: boolean,
  echoCancellation: boolean,
  noiseSuppressionMode: NoiseSuppressionMode,
  soundsEnabled: boolean,
): void {
  localStorage.setItem(
    ADVANCED_OPTIONS_STORAGE_KEY,
    JSON.stringify({ bitrateBps, lowDelay, autoGainControl, echoCancellation, noiseSuppressionMode, soundsEnabled }),
  )
}

loadAdvancedOptions()

soundsToggle.addEventListener('change', () => {
  setSoundsEnabled(soundsToggle.checked)
  saveAdvancedOptions(
    parseInt(bitrateSelect.value, 10) || DEFAULT_BITRATE_BPS,
    lowDelayToggle.checked,
    autoGainToggle.checked,
    echoCancellationToggle.checked,
    noiseSuppressionSelect.value as NoiseSuppressionMode,
    soundsToggle.checked,
  )
})

// --- Login ---
loginForm.addEventListener('submit', (e) => {
  e.preventDefault()
  const username = usernameInput.value.trim()
  const password = passwordInput.value
  if (!username) return

  const bitrateBps = parseInt(bitrateSelect.value, 10) || DEFAULT_BITRATE_BPS
  const lowDelay = lowDelayToggle.checked
  const autoGainControl = autoGainToggle.checked
  const echoCancellation = echoCancellationToggle.checked
  const noiseSuppressionMode = noiseSuppressionSelect.value as NoiseSuppressionMode

  saveCredentials(username, password)
  saveAdvancedOptions(bitrateBps, lowDelay, autoGainControl, echoCancellation, noiseSuppressionMode, soundsToggle.checked)
  loginError.classList.add('hidden')
  connectBtn.disabled = true
  connectBtn.textContent = 'Connecting…'

  currentUsername = username
  client = createClient(bitrateBps, lowDelay, noiseSuppressionMode, autoGainControl, echoCancellation)
  client.connect(username, password)
})

// --- Mute ---
function renderMuteButton(): void {
  // Icon + accent fill convey state; the label stays "Mute" so the button
  // never changes width and shifts the app-bar layout.
  muteBtnIcon.innerHTML = muted ? MIC_OFF_SVG : MIC_SVG
  muteBtn.classList.toggle('active', muted)
}

function applyMute(next: boolean): void {
  if (next !== muted) playSound(next ? 'mute' : 'unmute')
  muted = next
  client?.setMuted(muted)
  renderMuteButton()
}

renderMuteButton()
muteBtn.addEventListener('click', () => applyMute(!muted))

// --- Deafen (local only: silences remote playback + forces mute) ---
function renderDeafenButton(): void {
  // Icon (crossed-out headphones) + accent fill convey state; the label stays
  // "Deafen" to avoid layout shift.
  deafenBtnIcon.innerHTML = deafened ? DEAFEN_OFF_SVG : DEAFEN_SVG
  deafenBtn.classList.toggle('active', deafened)
}
renderDeafenButton()

deafenBtn.addEventListener('click', () => {
  deafened = !deafened
  playSound(deafened ? 'deafen' : 'undeafen')
  client?.setDeafened(deafened) // also mutes remote audio + forces client mute
  renderDeafenButton()
  // Reflect the forced mute in the UI without re-sending the mute message
  // (client.setDeafened already muted us server-side).
  if (deafened && !muted) {
    muted = true
    renderMuteButton()
  }
})

// --- Disconnect ---
disconnectBtn.addEventListener('click', () => {
  client?.disconnect()
  client = null
})
disconnectBtnMobile.addEventListener('click', () => disconnectBtn.click())

// --- Mobile chat sheet ---
sheetToggle.addEventListener('click', () => {
  const open = chatPanel.classList.toggle('open')
  sheetToggle.setAttribute('aria-expanded', String(open))
})

function setSheetLatest(text: string): void {
  sheetLatest.textContent = text
}

// --- Chat ---
chatForm.addEventListener('submit', (e) => {
  e.preventDefault()
  const msg = chatInput.value.trim()
  if (!msg || !client) return
  client.sendText(msg)
  appendMessage(currentUsername, msg)
  chatInput.value = ''
})

// --- View helpers ---
function showLogin(): void {
  viewRoom.classList.add('hidden')
  viewLogin.classList.remove('hidden')
  connectBtn.disabled = false
  connectBtn.textContent = 'Connect'
  muted = false
  deafened = false
  currentUsername = ''
  renderMuteButton()
  renderDeafenButton()
  chatPanel.classList.remove('open')
  sheetToggle.setAttribute('aria-expanded', 'false')
  setSheetLatest('')
  userList.innerHTML = ''
  chatMessages.innerHTML = ''
  refreshRoster()
}

function showRoom(): void {
  loginError.classList.add('hidden')
  viewLogin.classList.add('hidden')
  viewRoom.classList.remove('hidden')
  // Render the roster count immediately in case no roster arrives yet.
  refreshRoster()
}

function showLoginError(msg: string): void {
  loginError.textContent = msg
  loginError.classList.remove('hidden')
  showLogin()
}

// --- User list helpers ---
function setUserList(users: UserInfo[]): void {
  userList.innerHTML = ''
  for (const user of users) {
    addUser(user)
  }
  refreshRoster()
}

function addUser(user: UserInfo): void {
  if (document.getElementById(`user-${CSS.escape(user.name)}`)) return
  // A user is a solid colored Metro tile: initial + status glyphs on top,
  // name (and a "you" caption for self, via CSS) along the bottom.
  const li = document.createElement('li')
  li.id = `user-${CSS.escape(user.name)}`
  li.classList.add('user-tile')
  if (user.name === currentUsername) li.classList.add('is-self')
  li.style.background = colorFor(user.name)

  const top = document.createElement('div')
  top.classList.add('tile-top')

  const initial = document.createElement('span')
  initial.classList.add('tile-initial')
  initial.textContent = user.name.charAt(0).toUpperCase()
  top.appendChild(initial)

  const status = document.createElement('div')
  status.classList.add('tile-status')

  // Talking equalizer bars — shown via CSS when li has .is-talking.
  const bars = document.createElement('span')
  bars.classList.add('talking-bars')
  bars.innerHTML = '<span></span><span></span><span></span>'
  status.appendChild(bars)

  const muteIcon = document.createElement('span')
  muteIcon.classList.add('icon-muted')
  muteIcon.title = 'Muted'
  muteIcon.innerHTML = USER_MIC_OFF_SVG
  status.appendChild(muteIcon)

  const deafIcon = document.createElement('span')
  deafIcon.classList.add('icon-deafened')
  deafIcon.title = 'Deafened'
  deafIcon.innerHTML = USER_DEAF_SVG
  status.appendChild(deafIcon)

  top.appendChild(status)
  li.appendChild(top)

  const bottom = document.createElement('div')
  bottom.classList.add('tile-bottom')
  const nameSpan = document.createElement('span')
  nameSpan.classList.add('user-name')
  nameSpan.textContent = user.name
  bottom.appendChild(nameSpan)
  li.appendChild(bottom)

  userList.appendChild(li)
  applyUserState(li, user)
  refreshRoster()
}

function removeUser(name: string): void {
  document.getElementById(`user-${CSS.escape(name)}`)?.remove()
  refreshRoster()
}

// Updates the roster count pill from the number of user tiles.
function refreshRoster(): void {
  userCount.textContent = String(userList.querySelectorAll('li').length)
}

function applyUserState(
  li: HTMLElement,
  state: { muted?: boolean; selfMuted?: boolean; deafened?: boolean; selfDeafened?: boolean },
): void {
  if (state.muted !== undefined || state.selfMuted !== undefined) {
    li.classList.toggle('is-muted', Boolean(state.muted || state.selfMuted))
  }
  if (state.deafened !== undefined || state.selfDeafened !== undefined) {
    li.classList.toggle('is-deafened', Boolean(state.deafened || state.selfDeafened))
  }
}

function updateUserState(
  name: string,
  state: { muted?: boolean; selfMuted?: boolean; deafened?: boolean; selfDeafened?: boolean },
): void {
  const li = document.getElementById(`user-${CSS.escape(name)}`)
  if (!li) return
  applyUserState(li, state)
}

function setTalking(name: string, talking: boolean): void {
  const li = document.getElementById(`user-${CSS.escape(name)}`)
  li?.classList.toggle('is-talking', talking)
}

// --- Chat helpers ---
const ROOM_EVENT_TEXT: Record<RoomEventKind, string> = {
  joined: 'connected.',
  left: 'disconnected.',
  muted: 'is now muted.',
  unmuted: 'is no longer muted.',
  deafened: 'is now deafened.',
  undeafened: 'is no longer deafened.',
}

// Timestamps are stamped locally on arrival rather than carried over the
// wire — these are live UI events, and the browser's own clock is what the
// user reads everything else in the room against.
function formatTimestamp(date: Date): string {
  return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

function makeTimestampSpan(): HTMLSpanElement {
  const span = document.createElement('span')
  span.classList.add('timestamp')
  span.textContent = formatTimestamp(new Date())
  return span
}

// Synthesized locally from roster events, same as a native Mumble client's
// own event log — never a real chat message, so unlike appendMessage this
// must not interpret `username` as HTML (a Mumble display name is
// attacker-controlled and, unlike the `message` field, was never meant to
// carry markup).
function appendRoomEvent(kind: RoomEventKind, username: string): void {
  const div = document.createElement('div')
  div.classList.add('message', 'chat-event')
  div.appendChild(makeTimestampSpan())
  const line = `${username} ${ROOM_EVENT_TEXT[kind]}`
  const text = document.createElement('span')
  text.textContent = line
  div.appendChild(text)
  chatMessages.appendChild(div)
  chatMessages.scrollTop = chatMessages.scrollHeight
  setSheetLatest(line)
}

function appendMessage(from: string, message: string): void {
  // Server messages carry no sender (e.g. the MOTD / ASCII welcome banner) —
  // render them as a bordered monospace block rather than a chat line.
  if (!from) {
    const welcome = document.createElement('div')
    welcome.classList.add('chat-welcome')
    const pre = document.createElement('pre')
    pre.innerHTML = message
    welcome.appendChild(pre)
    chatMessages.appendChild(welcome)
    chatMessages.scrollTop = chatMessages.scrollHeight
    setSheetLatest(pre.textContent ?? '')
    return
  }

  const div = document.createElement('div')
  div.classList.add('message')
  div.appendChild(makeTimestampSpan())
  const label = document.createElement('span')
  label.classList.add('from')
  label.textContent = from ? `${from}: ` : ''
  // Match the sender's avatar color (same hash as the roster).
  if (from) label.style.color = colorFor(from)
  const text = document.createElement('span')
  text.classList.add('body')
  text.innerHTML = message
  div.appendChild(label)
  div.appendChild(text)
  chatMessages.appendChild(div)
  chatMessages.scrollTop = chatMessages.scrollHeight
  // Collapsed mobile sheet preview — plain text, not the message's markup.
  const preview = from ? `${from}: ${text.textContent ?? ''}` : (text.textContent ?? '')
  setSheetLatest(preview)
}
