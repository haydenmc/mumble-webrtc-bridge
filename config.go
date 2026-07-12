package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	MumbleHost     string
	MumblePort     int
	MumblePassword string
	MumbleChannel  string // optional, default root

	// BridgeHost is the IP/hostname the browser can reach this server on.
	// Required when running behind NAT or in a container (e.g. 127.0.0.1
	// for local podman, or the public IP in production).
	BridgeHost string

	TURNURLs       []string
	TURNUsername   string
	TURNCredential string

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

	cfg.MumblePassword = os.Getenv("MUMBLE_PASSWORD")
	cfg.MumbleChannel = os.Getenv("MUMBLE_CHANNEL")
	cfg.BridgeHost = os.Getenv("BRIDGE_HOST")

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

	return cfg, nil
}

func (c *Config) MumbleAddr() string {
	return fmt.Sprintf("%s:%d", c.MumbleHost, c.MumblePort)
}
