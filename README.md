# mumble-webrtc-bridge

A bridge that lets a browser join a [Mumble](https://www.mumble.info/) server
over WebRTC, no plugin or native client required. It speaks the Mumble
protocol (TCP control channel + UDP voice) on one side and WebRTC + a
WebSocket signaling channel on the other, relaying Opus audio between the two
without ever decoding it.

Since this is just a hacked together tool for my friend group to use, the
code is largely LLM-generated, but I make an effort to manually review each
change.

<img width="1277" height="719" alt="Screenshot of front-end user interface, showing users on the left, chat on the right, and a set of mute/deafen/disconnect buttons on the top." src="https://github.com/user-attachments/assets/a396f45e-50f5-411c-bf48-d412592bb255" />

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
| `TURN_URLS` | no | — | Comma-separated TURN server URLs, used by both the bridge and the browser (see [Connectivity fallbacks](#connectivity-fallbacks)) |
| `TURN_SECRET` | no | — | Shared secret (coturn `static-auth-secret`) used to mint short-lived, per-session TURN credentials. Preferred over `TURN_USERNAME`/`TURN_CREDENTIAL`, and takes precedence if both are set |
| `TURN_USERNAME` | no | — | Static TURN credential username. Note: sent to every browser that logs in |
| `TURN_CREDENTIAL` | no | — | Static TURN credential password. Note: sent to every browser that logs in |
| `WEBRTC_UDP_PORT_MIN` / `WEBRTC_UDP_PORT_MAX` | no | unset (full range) | Narrows the UDP port range WebRTC draws ICE candidates from, e.g. to publish just `-p 50000-50100:50000-50100/udp` instead of running with `--network host` |
| `WEBRTC_TCP_PORT` | no | unset (disabled) | Enables ICE-over-TCP on this port (one shared listener), so browsers on networks that block UDP can still reach the bridge directly. The port must be reachable from browsers |
| `HTTP_ADDR` | no | `:8080` | Address the HTTP server listens on |
| `TLS_CERT` / `TLS_KEY` | no | — | Serve HTTPS directly if both are set |

## Connectivity fallbacks

Some networks block UDP, and some VPNs and privacy extensions restrict WebRTC
(for example to "proxied/relayed traffic only"). The browser's ICE agent tries
every available path and uses the first that works. In rough priority order:

| Path | Needs | Helps when |
|---|---|---|
| UDP direct to the bridge | nothing extra (`BRIDGE_HOST` behind NAT/containers) | normal networks |
| TCP direct to the bridge | `WEBRTC_TCP_PORT` | the user's network blocks outbound UDP |
| TURN over UDP/TCP | a TURN server + `TURN_URLS`/`TURN_SECRET` | the browser only allows relayed WebRTC (privacy extensions, some VPN clients), or a restrictive NAT/firewall |
| TURN over TLS (`turns:`) | a TURN server with a certificate | firewalls that only allow TLS out (best on port 443) |

If none of these work, the browser doesn't drop silently back to the login
screen. It returns there with an error that explains the likely cause, such as
WebRTC being blocked by an extension or VPN, the server being unreachable, or
microphone access being denied. The bridge gives up after 30s; the browser
gives up after 15s.

`WEBRTC_TCP_PORT` alone covers UDP-blocking networks. The relay-only case
needs a TURN server.

### TURN server (docker compose)

[coturn](https://github.com/coturn/coturn) runs well next to the bridge.
Both services use host networking, matching `make run`, so WebRTC and TURN can
use real host ports. This works with `podman compose` (or `podman-compose`) as
well as Docker Compose.

`compose.yaml`:

```yaml
services:
  bridge:
    image: mumble-webrtc-bridge   # built with `make build`
    network_mode: host
    restart: unless-stopped
    environment:
      MUMBLE_HOST: mumble.example.com
      BRIDGE_HOST: 203.0.113.10          # the bridge's public IP
      WEBRTC_TCP_PORT: "8443"
      TURN_URLS: >-
        turn:turn.example.com:3478?transport=udp,turn:turn.example.com:3478?transport=tcp,turns:turn.example.com:5349?transport=tcp
      TURN_SECRET: change-me-to-a-long-random-string   # must match static-auth-secret

  coturn:
    image: docker.io/coturn/coturn:4.17.2
    network_mode: host
    restart: unless-stopped
    command: ["-c", "/etc/coturn/turnserver.conf"]
    volumes:
      - ./turnserver.conf:/etc/coturn/turnserver.conf:ro,Z
      # Only needed for turns: (TLS). E.g. a Let's Encrypt cert for turn.example.com.
      - ./certs:/etc/coturn/certs:ro,Z
```

`turnserver.conf`:

```ini
listening-port=3478
tls-listening-port=5349
# If the host has a public IP directly on an interface this can be omitted.
# Behind 1:1 NAT (most cloud VMs), set it to <public-ip>/<private-ip>.
external-ip=203.0.113.10
realm=turn.example.com

# Short-lived credentials minted by the bridge from TURN_SECRET.
use-auth-secret
static-auth-secret=change-me-to-a-long-random-string

# Relay port range; open these in the firewall (UDP).
min-port=49160
max-port=49200

cert=/etc/coturn/certs/fullchain.pem
pkey=/etc/coturn/certs/privkey.pem

fingerprint
no-multicast-peers
# Stop the relay from being used to reach private networks. Because of this,
# BRIDGE_HOST must be the bridge's *public* IP, so relayed traffic to the
# bridge isn't denied.
denied-peer-ip=0.0.0.0-0.255.255.255
denied-peer-ip=10.0.0.0-10.255.255.255
denied-peer-ip=100.64.0.0-100.127.255.255
denied-peer-ip=127.0.0.0-127.255.255.255
denied-peer-ip=169.254.0.0-169.254.255.255
denied-peer-ip=172.16.0.0-172.31.255.255
denied-peer-ip=192.168.0.0-192.168.255.255
denied-peer-ip=fc00::-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff
denied-peer-ip=fe80::-febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff
```

Firewall ports to open on the host:

- `3478/udp` and `3478/tcp`: TURN
- `5349/tcp`: TURN over TLS
- `49160-49200/udp`: TURN relay range
- `8443/tcp`: the bridge's ICE-TCP port (`WEBRTC_TCP_PORT`)
- the bridge's WebRTC UDP range (all ports, or `WEBRTC_UDP_PORT_MIN`–`WEBRTC_UDP_PORT_MAX`)

Generate the secret with something like `openssl rand -hex 32`. On very
restrictive networks, `turns:` on port 443 gets through best. That needs 443
to be free on the TURN host's IP, so it can't share the port with a reverse
proxy unless you add SNI-based multiplexing, which is not covered here.

To check the TURN server, open the browser's WebRTC internals
(`chrome://webrtc-internals` or `about:webrtc`) while connecting: a working
relay shows a `relay` local candidate in the selected candidate pair.

## Features

- Voice relay over WebRTC (UDP by default, TCP fallback via
  `MUMBLE_FORCE_TCP`)
- Browser connectivity fallbacks (ICE-over-TCP and TURN) with a clear error
  when voice can't be established
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
