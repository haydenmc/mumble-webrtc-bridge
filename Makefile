.PHONY: build run dev clean generate-proto

# Build the container image (frontend + Go compiled inside podman)
build:
	podman build -t mumble-webrtc-bridge .

# Regenerate internal/mumble/MumbleProto/Mumble.pb.go from the vendored
# Mumble.proto for local gopls/IDE support. The gitignored output is also
# regenerated fresh by `make build`/the Dockerfile on every image build, so
# this is purely a developer convenience, not a build dependency. Keep the
# protoc-gen-go version pinned here in sync with the Dockerfile's builder
# stage.
generate-proto:
	podman run --rm -v $(CURDIR):/app:Z -w /app golang:1.26-alpine sh -c '\
		apk add --no-cache protobuf-dev >/dev/null && \
		go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11 && \
		export PATH=/root/go/bin:$$PATH && \
		protoc --go_out=. --go_opt=paths=source_relative internal/mumble/MumbleProto/Mumble.proto'

# Run the built image against a Mumble server for testing.
# --network=host lets Pion's ICE candidates use real host interfaces so
# WebRTC UDP traffic isn't blocked by unmapped container ports.
run:
	podman run --rm --network=host \
		-e MUMBLE_HOST=$(MUMBLE_HOST) \
		mumble-webrtc-bridge

# Frontend dev server only (Vite hot-reload, no Go needed)
dev:
	cd frontend && npm install && npm run dev

clean:
	podman rmi -f mumble-webrtc-bridge 2>/dev/null || true
	rm -rf frontend/dist
