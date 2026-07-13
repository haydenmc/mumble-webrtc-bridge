package bridge

import "time"

// opusFrameDuration returns the audio duration encoded in an Opus packet's
// TOC byte (RFC 6716 §3.1). Used as the WebRTC sample pacing hint since
// Mumble->browser audio is relayed as raw Opus payloads without decoding —
// there's no PCM sample count to derive duration from otherwise. Falls back
// to 20ms (Mumble's common default) for empty or malformed packets.
func opusFrameDuration(data []byte) time.Duration {
	const fallback = 20 * time.Millisecond
	if len(data) == 0 {
		return fallback
	}

	config := data[0] >> 3
	var frameUs int64
	switch {
	case config < 12: // SILK-only: 10/20/40/60ms
		frameUs = []int64{10000, 20000, 40000, 60000}[config%4]
	case config < 16: // Hybrid: 10/20ms
		frameUs = []int64{10000, 20000}[config%2]
	default: // CELT-only: 2.5/5/10/20ms
		frameUs = []int64{2500, 5000, 10000, 20000}[config%4]
	}

	frameCount := 1
	switch data[0] & 0x3 {
	case 1, 2:
		frameCount = 2
	case 3: // "code 3": arbitrary frame count byte follows
		if len(data) < 2 {
			return fallback
		}
		frameCount = int(data[1] & 0x3F)
		if frameCount == 0 {
			return fallback
		}
	}

	return time.Duration(frameUs*int64(frameCount)) * time.Microsecond
}
