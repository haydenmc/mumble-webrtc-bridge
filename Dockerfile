# Stage 1: build TypeScript frontend (arch-independent static assets, so
# this always runs natively on the build host — no emulation needed)
FROM --platform=$BUILDPLATFORM node:24-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package*.json ./
COPY frontend/scripts ./scripts
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Stage 2: build Go binary (pure Go, no CGo/libopus — audio is relayed
# as opaque Opus payloads, never decoded or encoded server-side). Runs
# natively on the build host and cross-compiles for the target arch, so
# multi-arch images don't require QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
# protobuf-dev provides protoc; protoc-gen-go is pinned to match the
# google.golang.org/protobuf version in go.mod (keep in sync with the
# "generate-proto" Makefile target, which uses the same version).
RUN apk add --no-cache ca-certificates protobuf-dev
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
ENV PATH="/root/go/bin:${PATH}"
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Mumble.pb.go is generated here, not committed, from the vendored
# Mumble.proto (see internal/mumble/MumbleProto/Mumble.proto and NOTICE.md).
RUN protoc --go_out=. --go_opt=paths=source_relative \
    internal/mumble/MumbleProto/Mumble.proto
COPY --from=frontend /app/frontend/dist ./frontend/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o bridge-server .

# Stage 3: minimal runtime image — only COPY, no RUN, so no target-arch
# code ever executes during the build
FROM alpine:3.24.1
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
WORKDIR /app
COPY --from=builder /app/bridge-server /app/bridge-server
EXPOSE 8080
ENTRYPOINT ["/app/bridge-server"]
