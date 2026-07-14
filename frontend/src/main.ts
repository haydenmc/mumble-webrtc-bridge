import { MumbleWebRTCClient, type NoiseSuppressionMode, type RoomEventKind, type UserInfo } from './client'

// --- DOM refs ---
const viewLogin = document.getElementById('view-login')!
const viewRoom = document.getElementById('view-room')!
const loginForm = document.getElementById('login-form') as HTMLFormElement
const usernameInput = document.getElementById('username') as HTMLInputElement
const passwordInput = document.getElementById('password') as HTMLInputElement
const bitrateSelect = document.getElementById('bitrate-select') as HTMLSelectElement
const lowDelayToggle = document.getElementById('low-delay-toggle') as HTMLInputElement
const autoGainToggle = document.getElementById('auto-gain-toggle') as HTMLInputElement
const echoCancellationToggle = document.getElementById('echo-cancellation-toggle') as HTMLInputElement
const noiseSuppressionSelect = document.getElementById('noise-suppression-select') as HTMLSelectElement
const loginError = document.getElementById('login-error')!
const connectBtn = document.getElementById('connect-btn') as HTMLButtonElement
const muteBtn = document.getElementById('mute-btn') as HTMLButtonElement
const disconnectBtn = document.getElementById('disconnect-btn') as HTMLButtonElement
const userList = document.getElementById('user-list')!
const chatMessages = document.getElementById('chat-messages')!
const chatForm = document.getElementById('chat-form') as HTMLFormElement
const chatInput = document.getElementById('chat-input') as HTMLInputElement

let client: MumbleWebRTCClient | null = null
let muted = false
let currentUsername = ''

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
        showRoom()
      },
      onError(msg) {
        showLoginError(msg)
        connectBtn.disabled = false
        connectBtn.textContent = 'Connect'
      },
      onText(from, message) {
        appendMessage(from, message)
      },
      onUserList(users) {
        setUserList(users)
      },
      onUserJoined(username) {
        addUser({ name: username, muted: false, selfMuted: false, deafened: false, selfDeafened: false })
      },
      onUserLeft(username) {
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
    const { bitrateBps, lowDelay, autoGainControl, echoCancellation, noiseSuppressionMode } = JSON.parse(saved)
    if (bitrateBps) bitrateSelect.value = String(bitrateBps)
    if (lowDelay !== undefined) lowDelayToggle.checked = Boolean(lowDelay)
    if (autoGainControl !== undefined) autoGainToggle.checked = Boolean(autoGainControl)
    if (echoCancellation !== undefined) echoCancellationToggle.checked = Boolean(echoCancellation)
    if (VALID_NOISE_SUPPRESSION_MODES.includes(noiseSuppressionMode)) {
      noiseSuppressionSelect.value = noiseSuppressionMode
    }
  } catch {
    // ignore malformed storage
  }
}

function saveAdvancedOptions(
  bitrateBps: number,
  lowDelay: boolean,
  autoGainControl: boolean,
  echoCancellation: boolean,
  noiseSuppressionMode: NoiseSuppressionMode,
): void {
  localStorage.setItem(
    ADVANCED_OPTIONS_STORAGE_KEY,
    JSON.stringify({ bitrateBps, lowDelay, autoGainControl, echoCancellation, noiseSuppressionMode }),
  )
}

loadAdvancedOptions()

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
  saveAdvancedOptions(bitrateBps, lowDelay, autoGainControl, echoCancellation, noiseSuppressionMode)
  loginError.classList.add('hidden')
  connectBtn.disabled = true
  connectBtn.textContent = 'Connecting…'

  currentUsername = username
  client = createClient(bitrateBps, lowDelay, noiseSuppressionMode, autoGainControl, echoCancellation)
  client.connect(username, password)
})

// --- Mute ---
muteBtn.addEventListener('click', () => {
  muted = !muted
  client?.setMuted(muted)
  muteBtn.textContent = muted ? 'Unmute' : 'Mute'
  muteBtn.classList.toggle('active', muted)
})

// --- Disconnect ---
disconnectBtn.addEventListener('click', () => {
  client?.disconnect()
  client = null
})

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
  currentUsername = ''
  muteBtn.textContent = 'Mute'
  muteBtn.classList.remove('active')
  userList.innerHTML = ''
  chatMessages.innerHTML = ''
}

function showRoom(): void {
  loginError.classList.add('hidden')
  viewLogin.classList.add('hidden')
  viewRoom.classList.remove('hidden')
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
}

function addUser(user: UserInfo): void {
  if (document.getElementById(`user-${CSS.escape(user.name)}`)) return
  const li = document.createElement('li')
  li.id = `user-${CSS.escape(user.name)}`

  const nameSpan = document.createElement('span')
  nameSpan.classList.add('user-name')
  nameSpan.textContent = user.name
  li.appendChild(nameSpan)

  const muteIcon = document.createElement('span')
  muteIcon.classList.add('icon-muted')
  muteIcon.title = 'Muted'
  muteIcon.textContent = '\u{1F507}' // 🔇
  li.appendChild(muteIcon)

  const deafIcon = document.createElement('span')
  deafIcon.classList.add('icon-deafened')
  deafIcon.title = 'Deafened'
  deafIcon.textContent = '\u{1F515}' // 🔕
  li.appendChild(deafIcon)

  userList.appendChild(li)
  applyUserState(li, user)
}

function removeUser(name: string): void {
  document.getElementById(`user-${CSS.escape(name)}`)?.remove()
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
  const text = document.createElement('span')
  text.textContent = `${username} ${ROOM_EVENT_TEXT[kind]}`
  div.appendChild(text)
  chatMessages.appendChild(div)
  chatMessages.scrollTop = chatMessages.scrollHeight
}

function appendMessage(from: string, message: string): void {
  const div = document.createElement('div')
  div.classList.add('message')
  div.appendChild(makeTimestampSpan())
  const label = document.createElement('span')
  label.classList.add('from')
  label.textContent = from ? `${from}: ` : ''
  const text = document.createElement('span')
  text.innerHTML = message
  div.appendChild(label)
  div.appendChild(text)
  chatMessages.appendChild(div)
  chatMessages.scrollTop = chatMessages.scrollHeight
}
