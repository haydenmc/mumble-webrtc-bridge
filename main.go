package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/hayden/mumble-webrtc-bridge/bridge"
	"layeh.com/gumble/gumble"
)

//go:embed frontend/dist
var staticFiles embed.FS

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	// Register Opus codec for gumble globally (once at startup).
	gumble.RegisterAudioCodec(4, bridge.NewOpusCodec())

	srv := bridge.NewServer(cfg.MumbleAddr(), cfg.MumblePassword, cfg.MumbleChannel, bridge.ICEConfig{
		BridgeHost:     cfg.BridgeHost,
		TURNURLs:       cfg.TURNURLs,
		TURNUsername:   cfg.TURNUsername,
		TURNCredential: cfg.TURNCredential,
	})

	// Serve compiled frontend.
	dist, err := fs.Sub(staticFiles, "frontend/dist")
	if err != nil {
		log.Fatalf("embed error: %v", err)
	}
	http.Handle("/", http.FileServer(http.FS(dist)))

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade: %v", err)
			return
		}
		go srv.HandleConn(conn)
	})

	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		log.Printf("listening on %s (TLS)", cfg.HTTPAddr)
		log.Fatal(http.ListenAndServeTLS(cfg.HTTPAddr, cfg.TLSCert, cfg.TLSKey, nil))
	} else {
		log.Printf("listening on %s", cfg.HTTPAddr)
		log.Fatal(http.ListenAndServe(cfg.HTTPAddr, nil))
	}
}
