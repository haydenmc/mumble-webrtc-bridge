export type NoiseSuppressionMode = 'rnnoise' | 'browser' | 'off'

const VALID_NOISE_SUPPRESSION_MODES: NoiseSuppressionMode[] = ['rnnoise', 'browser', 'off']

// Everything the settings modal and login "advanced options" manage. Not
// AudioSettings — the modal is expected to grow beyond audio, and consumers
// (the client, persistence, both UIs) should only ever need this one shape.
export interface Settings {
  bitrateBps: number
  lowDelay: boolean
  noiseSuppression: NoiseSuppressionMode
  autoGainControl: boolean
  echoCancellation: boolean
  soundsEnabled: boolean
  joinMuted: boolean
}

export const DEFAULT_SETTINGS: Settings = {
  bitrateBps: 96000,
  lowDelay: true,
  noiseSuppression: 'rnnoise',
  autoGainControl: true,
  echoCancellation: true,
  soundsEnabled: true,
  joinMuted: false,
}

const STORAGE_KEY = 'mumble-bridge-advanced-options'

export function loadSettings(): Settings {
  const settings = { ...DEFAULT_SETTINGS }
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (!saved) return settings
    const parsed = JSON.parse(saved)
    if (typeof parsed.bitrateBps === 'number') settings.bitrateBps = parsed.bitrateBps
    if (typeof parsed.lowDelay === 'boolean') settings.lowDelay = parsed.lowDelay
    if (VALID_NOISE_SUPPRESSION_MODES.includes(parsed.noiseSuppression)) {
      settings.noiseSuppression = parsed.noiseSuppression
    }
    if (typeof parsed.autoGainControl === 'boolean') settings.autoGainControl = parsed.autoGainControl
    if (typeof parsed.echoCancellation === 'boolean') settings.echoCancellation = parsed.echoCancellation
    if (typeof parsed.soundsEnabled === 'boolean') settings.soundsEnabled = parsed.soundsEnabled
    if (typeof parsed.joinMuted === 'boolean') settings.joinMuted = parsed.joinMuted
  } catch {
    // ignore malformed storage
  }
  return settings
}

export function saveSettings(settings: Settings): void {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(settings))
}
