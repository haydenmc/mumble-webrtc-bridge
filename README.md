# mumble-webrtc-bridge

A bridge that lets a browser join a [Mumble](https://www.mumble.info/) server
over WebRTC, no plugin or native client required. It speaks the Mumble
protocol (TCP control channel + UDP voice) on one side and WebRTC + a
WebSocket signaling channel on the other, relaying Opus audio between the two
without ever decoding it.

Since this is just a hacked together tool for my friend group to use, the
code is largely LLM-generated, but I make an effort to manually review each
change.

## How it works

- The Go server holds a from-scratch Mumble client (`internal/mumble`) that
  connects to the target Mumble server over TLS/TCP for control traffic and
  UDP for voice, implementing just enough of the protocol to relay audio,
  roster state, and text chat.
- Each browser tab opens a WebSocket (`/ws`) for signaling and a WebRTC
  `PeerConnection` for audio, handled by `bridge.Server`/`bridge.Peer`
  (`bridge/`).
- Audio is relayed as opaque Opus payloads in both directions — the server
  never encodes or decodes audio, so there's no CGo/libopus dependency and no
  transcoding cost.
- The frontend (`frontend/`, TypeScript + Vite) is a single-page app compiled
  to static assets and embedded into the Go binary at build time.

## Requirements

- [podman](https://podman.io/) — all building and running goes through
  podman; there is no supported local (non-container) build path.
- A reachable Mumble server to bridge to.

## Quick start

```sh
make build
MUMBLE_HOST=your.mumble.server make run
```

This builds the container image (frontend assets are compiled with Node,
then the Go binary is cross-compiled, all inside the podman build — no local
Go/Node toolchain needed) and runs it with `--network=host`, which lets
WebRTC's ICE candidates bind to real host interfaces. Then open
`http://localhost:8080`.

For frontend-only iteration (hot reload, no Go/container involved):

```sh
make dev
```

## Configuration

The server is configured entirely via environment variables:

| Variable | Required | Default | Description |
|---|---|---|---|
| `MUMBLE_HOST` | yes | — | Hostname/IP of the Mumble server to bridge to |
| `MUMBLE_PORT` | no | `64738` | Mumble server port |
| `MUMBLE_CHANNEL` | no | root channel | Channel to join on connect |
| `MUMBLE_FORCE_TCP` | no | unset | If set, disables UDP voice and tunnels audio over TCP only — useful for diagnosing transport issues or on networks that block/mangle UDP to the Mumble server |
| `BRIDGE_HOST` | no | — | IP/hostname browsers can reach this server on; advertised as a host ICE candidate instead of the container-internal IP. Needed behind NAT or in a container |
| `BRIDGE_TITLE` | no | `Mumble Bridge` | Branding text for `<title>` and the login header |
| `BRIDGE_ABOUT` | no | — | Optional blurb (raw HTML allowed) shown under the login header |
| `TURN_URLS` | no | — | Comma-separated TURN server URLs |
| `TURN_USERNAME` | no | — | TURN credential username |
| `TURN_CREDENTIAL` | no | — | TURN credential password |
| `WEBRTC_UDP_PORT_MIN` / `WEBRTC_UDP_PORT_MAX` | no | unset (full range) | Narrows the UDP port range WebRTC draws ICE candidates from, e.g. to publish just `-p 50000-50100:50000-50100/udp` instead of running with `--network host` |
| `HTTP_ADDR` | no | `:8080` | Address the HTTP server listens on |
| `TLS_CERT` / `TLS_KEY` | no | — | Serve HTTPS directly if both are set |

## Features

- Voice relay over WebRTC (UDP by default, TCP fallback via
  `MUMBLE_FORCE_TCP`)
- Roster with talking/mute/deafen indicators and join/leave/mute event log
- Text chat
- Per-connection Opus bitrate and low-delay mode, configurable from the
  login screen

## Project layout

```
main.go, config.go     HTTP entrypoint, env-based configuration
bridge/                 WebRTC signaling + peer/session management
internal/mumble/        Mumble protocol client (TCP control + UDP voice)
frontend/               TypeScript/Vite single-page app
Dockerfile               Multi-stage podman build (frontend -> Go -> runtime)
```

## Sound effect attribution

The notification sounds in `frontend/public/sounds/` (join/leave/message/mute/
deafen, toggleable from Advanced options) are sourced from Pixabay:

- `join.ogg`: [Film Special Effects Notification 1](https://pixabay.com/sound-effects/film-special-effects-notification-1-269296/)
- `leave.ogg`: [Film Special Effects Notification 1 (reversed)](https://pixabay.com/sound-effects/film-special-effects-notification-1-reversed-317859/)
- `message.ogg`: [App Interface Click 2](https://pixabay.com/sound-effects/app-interface-click-2-476372/)
- `mute.ogg`, `deafen.ogg`: [Film Special Effects UI Sound Off](https://pixabay.com/sound-effects/film-special-effects-ui-sound-off-270300/)
- `unmute.ogg`, `undeafen.ogg`: [Film Special Effects UI Sound On](https://pixabay.com/sound-effects/film-special-effects-ui-sound-on-270295/)
