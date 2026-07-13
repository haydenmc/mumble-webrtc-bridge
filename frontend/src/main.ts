import { MumbleWebRTCClient } from './client'

// --- DOM refs ---
const viewLogin = document.getElementById('view-login')!
const viewRoom = document.getElementById('view-room')!
const loginForm = document.getElementById('login-form') as HTMLFormElement
const usernameInput = document.getElementById('username') as HTMLInputElement
const passwordInput = document.getElementById('password') as HTMLInputElement
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
function createClient(): MumbleWebRTCClient {
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
        addUser(username)
      },
      onUserLeft(username) {
        removeUser(username)
      },
      onDisconnected() {
        showLogin()
      },
    },
    // TURN config — populated via template or env if needed
    [],
    '',
    '',
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

// --- Login ---
loginForm.addEventListener('submit', (e) => {
  e.preventDefault()
  const username = usernameInput.value.trim()
  const password = passwordInput.value
  if (!username) return

  saveCredentials(username, password)
  loginError.classList.add('hidden')
  connectBtn.disabled = true
  connectBtn.textContent = 'Connecting…'

  currentUsername = username
  client = createClient()
  // TEMPORARY diagnostic: exposes the client on window so
  // client.downloadDebugRecording() works from the DevTools console (a
  // module-scoped `client` isn't otherwise reachable from there).
  ;(window as unknown as { client: typeof client }).client = client
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
function setUserList(users: string[]): void {
  userList.innerHTML = ''
  for (const name of users) {
    addUser(name)
  }
}

function addUser(name: string): void {
  if (document.getElementById(`user-${CSS.escape(name)}`)) return
  const li = document.createElement('li')
  li.id = `user-${CSS.escape(name)}`
  li.textContent = name
  userList.appendChild(li)
}

function removeUser(name: string): void {
  document.getElementById(`user-${CSS.escape(name)}`)?.remove()
}

// --- Chat helpers ---
function appendMessage(from: string, message: string): void {
  const div = document.createElement('div')
  div.classList.add('message')
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
