package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	MumbleHost    string
	MumblePort    int
	MumbleChannel string // optional, default root

	// BridgeHost is the IP/hostname the browser can reach this server on.
	// Required when running behind NAT or in a container (e.g. 127.0.0.1
	// for local podman, or the public IP in production).
	BridgeHost string

	TURNURLs       []string
	TURNUsername   string
	TURNCredential string

	// MumbleForceTCP disables the UDP voice channel, keeping audio on the
	// TCP tunnel. A bisection tool for diagnosing transport-specific audio
	// issues, and an escape hatch for networks that block/mangle UDP to
	// the Mumble server.
	MumbleForceTCP bool

	// WebRTCUDPPortMin/Max bound the ephemeral UDP port range WebRTC draws
	// ICE candidates from. Both zero (the default) leaves the range wide
	// open, which in practice requires --network host (or equivalent) to
	// be reachable from outside the container. Set both to a narrow range
	// to instead publish just that range, e.g.
	// `-p 50000-50100:50000-50100/udp`.
	WebRTCUDPPortMin uint16
	WebRTCUDPPortMax uint16

	HTTPAddr string
	TLSCert  string
	TLSKey   string
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		MumblePort: 64738,
		HTTPAddr:   ":8080",
	}

	cfg.MumbleHost = os.Getenv("MUMBLE_HOST")
	if cfg.MumbleHost == "" {
		return nil, fmt.Errorf("MUMBLE_HOST is required")
	}

	if p := os.Getenv("MUMBLE_PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid MUMBLE_PORT: %w", err)
		}
		cfg.MumblePort = n
	}

	cfg.MumbleChannel = os.Getenv("MUMBLE_CHANNEL")
	cfg.BridgeHost = os.Getenv("BRIDGE_HOST")
	cfg.MumbleForceTCP = os.Getenv("MUMBLE_FORCE_TCP") != ""

	if urls := os.Getenv("TURN_URLS"); urls != "" {
		cfg.TURNURLs = strings.Split(urls, ",")
	}
	cfg.TURNUsername = os.Getenv("TURN_USERNAME")
	cfg.TURNCredential = os.Getenv("TURN_CREDENTIAL")

	if addr := os.Getenv("HTTP_ADDR"); addr != "" {
		cfg.HTTPAddr = addr
	}
	cfg.TLSCert = os.Getenv("TLS_CERT")
	cfg.TLSKey = os.Getenv("TLS_KEY")

	portMinStr := os.Getenv("WEBRTC_UDP_PORT_MIN")
	portMaxStr := os.Getenv("WEBRTC_UDP_PORT_MAX")
	if (portMinStr == "") != (portMaxStr == "") {
		return nil, fmt.Errorf("WEBRTC_UDP_PORT_MIN and WEBRTC_UDP_PORT_MAX must be set together")
	}
	if portMinStr != "" {
		portMin, err := parsePort(portMinStr)
		if err != nil {
			return nil, fmt.Errorf("invalid WEBRTC_UDP_PORT_MIN: %w", err)
		}
		portMax, err := parsePort(portMaxStr)
		if err != nil {
			return nil, fmt.Errorf("invalid WEBRTC_UDP_PORT_MAX: %w", err)
		}
		if portMin > portMax {
			return nil, fmt.Errorf("WEBRTC_UDP_PORT_MIN (%d) must be <= WEBRTC_UDP_PORT_MAX (%d)", portMin, portMax)
		}
		cfg.WebRTCUDPPortMin = portMin
		cfg.WebRTCUDPPortMax = portMax
	}

	return cfg, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("port must be between 1 and 65535")
	}
	return uint16(n), nil
}

func (c *Config) MumbleAddr() string {
	return fmt.Sprintf("%s:%d", c.MumbleHost, c.MumblePort)
}
