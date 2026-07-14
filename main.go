package main

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/hayden/mumble-webrtc-bridge/bridge"
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

	srv := bridge.NewServer(cfg.MumbleAddr(), cfg.MumbleChannel, bridge.ICEConfig{
		BridgeHost:     cfg.BridgeHost,
		TURNURLs:       cfg.TURNURLs,
		TURNUsername:   cfg.TURNUsername,
		TURNCredential: cfg.TURNCredential,
		UDPPortMin:     cfg.WebRTCUDPPortMin,
		UDPPortMax:     cfg.WebRTCUDPPortMax,
	}, cfg.MumbleForceTCP)

	// Serve compiled frontend. index.html is a template so operators can
	// brand the page via BRIDGE_TITLE/BRIDGE_ABOUT; everything else is
	// served as-is.
	dist, err := fs.Sub(staticFiles, "frontend/dist")
	if err != nil {
		log.Fatalf("embed error: %v", err)
	}
	index, err := renderIndex(dist, cfg.BridgeTitle, cfg.BridgeAbout)
	if err != nil {
		log.Fatalf("index template error: %v", err)
	}
	fileServer := http.FileServer(http.FS(dist))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(index)
			return
		}
		fileServer.ServeHTTP(w, r)
	})

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

func renderIndex(dist fs.FS, title, about string) ([]byte, error) {
	raw, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("index.html").Parse(string(raw))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, struct{ Title, About string }{Title: title, About: about})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
