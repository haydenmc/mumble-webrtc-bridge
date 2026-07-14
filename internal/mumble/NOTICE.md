This package is a small, purpose-built Mumble client written for this
project. It is not a fork of any single upstream library, but it
incorporates a few pieces of code taken from existing open-source Mumble
implementations rather than reimplemented from scratch, credited below.

## `MumbleProto/Mumble.proto`

Vendored from [mumble-voip/mumble](https://github.com/mumble-voip/mumble)
`src/Mumble.proto`, pinned at tag `v1.5.901` (commit
`b26165ed56a08566daf3e1a6eb80df081284b4d9`), licensed under a
BSD-3-Clause-style license. See `LICENSE-BSD-mumble`. `MumbleProto/Mumble.pb.go`
is generated from this file at build time (see the Dockerfile and `make
generate-proto`) and is not committed to the repository.

## `varint/`

Copied verbatim from [layeh.com/gumble](https://github.com/layeh/gumble)
(commit `d1df60a3cc14`), licensed under the Mozilla Public License 2.0. See
`LICENSE-MPL-2.0`. Mumble's variable-length integer codec used in the audio
packet framing.

## `cryptstate/`

Ported from [mumble-voip/grumble](https://github.com/mumble-voip/grumble)
`pkg/cryptstate` (commit `master`, as of 2026-07), licensed under a
BSD-3-Clause-style license. See `LICENSE-BSD-grumble`. Trimmed to the
OCB2-AES128 mode only (the only mode the mainline Mumble/Murmur server
speaks); the XSalsa20-Poly1305 mode was not ported. A `SetDecryptIV` helper
was added to support the client-side crypt resync handshake, which is not
present in the upstream server-side package.
