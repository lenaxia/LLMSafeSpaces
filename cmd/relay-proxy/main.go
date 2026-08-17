// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/version"
)

const (
	defaultUpstreamURL       = "https://opencode.ai/zen/v1"
	defaultListenAddr        = "0.0.0.0:8080"
	defaultKeepaliveInterval = 30 * time.Second
)

type config struct {
	upstreamURL       string
	listenAddr        string
	token             string
	keepaliveInterval time.Duration
}

// loadConfig builds the relay-proxy config from CLI flags, falling back to env
// vars, then to hardcoded defaults. Precedence: flag > env > default.
//
// The --upstream flag is load-bearing: controller/internal/relay/cloudinit.go
// renders `--upstream=<relay.Spec.UpstreamURL>` into each relay VM's systemd
// ExecStart, so a per-CR upstream override must reach the binary. Before this
// parsed os.Args, the rendered flag was silently dropped and every VM fell
// back to the hardcoded default — breaking any non-default spec.upstreamURL.
//
// The --token flag is the shared secret the relay-router presents to identify
// itself. Each relay VM gets a unique token (per-VM blast radius: a
// compromised VM's token cannot be used against sibling relays). The
// controller generates and embeds it into cloud-init at provision time.
//
// args is os.Args[1:] from main(); explicit for testability.
func loadConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("relay-proxy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", getEnv("UPSTREAM_URL", defaultUpstreamURL),
		"LLM provider endpoint to proxy to (env: UPSTREAM_URL)")
	listen := fs.String("listen", getEnv("LISTEN_ADDR", defaultListenAddr),
		"address to listen on (env: LISTEN_ADDR)")
	token := fs.String("token", getEnv("RELAY_TOKEN", ""),
		"shared-secret token the relay-router must present in the X-Relay-Token header (env: RELAY_TOKEN). "+
			"Empty disables auth (local dev only) — production relays must set this; without it the proxy is an open forwarder to the upstream.")
	keepalive := fs.Duration("keepalive-interval", getEnvDuration("KEEPALIVE_INTERVAL", defaultKeepaliveInterval),
		"interval between upstream keepalive probes (env: KEEPALIVE_INTERVAL)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	return config{
		upstreamURL:       *upstream,
		listenAddr:        *listen,
		token:             *token,
		keepaliveInterval: *keepalive,
	}, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			// Bound the phase before response headers arrive (issue #911:
			// a stalled upstream left the relay's proxy handler hanging with
			// no response and no log). 5m covers slow non-streaming
			// completions (`stream:false` sends headers only after the full
			// generation); streaming chat/SSE get headers immediately. Body
			// streaming stays unbounded (no total client Timeout) so long
			// generations are never truncated.
			ResponseHeaderTimeout: 5 * time.Minute,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
		},
	}
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("X-Llmsafespaces-Version", version.Version)
	w.WriteHeader(http.StatusOK)
}

func metricsHandler(metrics *relayMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		metrics.writePrometheus(w)
	}
}

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		log.Fatalf("relay-proxy: %v", err)
	}

	client := defaultHTTPClient()
	metrics := newRelayMetrics()

	proxy, err := newProxyHandler(cfg.upstreamURL, client, metrics)
	if err != nil {
		log.Fatalf("relay-proxy: %v", err)
	}

	mux := buildMux(cfg.token, proxy, metrics)

	ka := newKeepalive(cfg.upstreamURL, client, cfg.keepaliveInterval, metrics)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ka.run(ctx)

	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("relay-proxy: shutting down...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		cancel()
	}()

	log.Printf("relay-proxy: listening on %s, upstream=%s, auth=%s",
		cfg.listenAddr, cfg.upstreamURL, authMode(cfg.token))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("relay-proxy: %v", err)
	}
	log.Println("relay-proxy: stopped")
}

// authMode renders the auth state for the startup log line. "token" means a
// shared secret is configured (production posture); "open" means no token and
// the proxy forwards anything (local dev only).
func authMode(token string) string {
	if token == "" {
		return "open"
	}
	return "token"
}
