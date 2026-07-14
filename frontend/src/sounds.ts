export type SoundKey = 'join' | 'leave' | 'message' | 'mute' | 'unmute' | 'deafen' | 'undeafen'

const SOUND_FILES: Record<SoundKey, string> = {
  join: '/sounds/join.ogg',
  leave: '/sounds/leave.ogg',
  message: '/sounds/message.ogg',
  mute: '/sounds/mute.ogg',
  unmute: '/sounds/unmute.ogg',
  deafen: '/sounds/deafen.ogg',
  undeafen: '/sounds/undeafen.ogg',
}

let enabled = true

export function setSoundsEnabled(v: boolean): void {
  enabled = v
}

// One Audio element per key, cloned on play so rapid repeats (e.g. two quick
// joins) don't cut each other off.
const cache = new Map<SoundKey, HTMLAudioElement>()

export function playSound(key: SoundKey): void {
  if (!enabled) return
  let base = cache.get(key)
  if (!base) {
    base = new Audio(SOUND_FILES[key])
    cache.set(key, base)
  }
  const el = base.cloneNode(true) as HTMLAudioElement
  el.play().catch(() => {
    // Autoplay can be blocked before the user has interacted with the page;
    // login already requires a click/submit so this should be rare post-connect.
  })
}
