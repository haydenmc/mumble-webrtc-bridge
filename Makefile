.PHONY: build run dev clean

# Build the container image (frontend + Go compiled inside podman)
build:
	podman build -t mumble-webrtc-bridge .

# Run the built image against a local Mumble server for testing
run:
	podman run --rm -p 8080:8080 \
		-e MUMBLE_HOST=$(MUMBLE_HOST) \
		-e MUMBLE_PASSWORD=$(MUMBLE_PASSWORD) \
		mumble-webrtc-bridge

# Frontend dev server only (Vite hot-reload, no Go needed)
dev:
	cd frontend && npm install && npm run dev

clean:
	podman rmi -f mumble-webrtc-bridge 2>/dev/null || true
	rm -rf frontend/dist
