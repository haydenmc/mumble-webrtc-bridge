PKG_CONFIG_PATH := $(HOME)/.local/lib64/pkgconfig
CGO_CFLAGS := -I$(HOME)/.local/include
CGO_LDFLAGS := -L$(HOME)/.local/lib64
GOENV := PKG_CONFIG_PATH=$(PKG_CONFIG_PATH) CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" CGO_ENABLED=1

.PHONY: build frontend run docker clean

build: frontend
	$(GOENV) go build -o bridge-server .

frontend:
	cd frontend && npm install && npm run build

run: build
	MUMBLE_HOST=localhost $(GOENV) ./bridge-server

docker:
	docker build -t mumble-webrtc-bridge .

clean:
	rm -f bridge-server
	rm -rf frontend/dist
