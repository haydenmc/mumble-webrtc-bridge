.PHONY: build run dev clean

# Build the container image (frontend + Go compiled inside podman)
build:
	podman build -t mumble-webrtc-bridge .

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
