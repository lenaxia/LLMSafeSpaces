// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/version"
)

const (
	defaultRouterListen     = ":8080"
	defaultRouterUpstream   = "https://opencode.ai/zen/v1"
	defaultHealthInterval   = 15 * time.Second
	defaultHealthTimeout    = 5 * time.Second
	defaultHealthThreshold  = 3
	defaultPeerConfigPath   = "/etc/relay-router/peers.json"
	defaultPeerPollInterval = 5 * time.Second
	defaultMax429Rate       = 0.5
	defaultDetectionWindow  = 5 * time.Minute
	defaultDetectorInterval = 30 * time.Second
	defaultFallbackRate     = 0.5
	defaultFallbackMaxConc  = 1
)

type routerConfig struct {
	listenAddr       string
	upstreamURL      string
	upstreamAuth     upstreamAuth
	peerConfigPath   string
	peerPollInterval time.Duration
	healthInterval   time.Duration
	healthTimeout    time.Duration
	healthThreshold  int
	max429Rate       float64
	detectionWindow  time.Duration
	detectorInterval time.Duration
	fallbackRate     float64
	fallbackMaxConc  int
}

func loadRouterConfig() routerConfig {
	return routerConfig{
		listenAddr:  getEnv("LISTEN_ADDR", defaultRouterListen),
		upstreamURL: getEnv("UPSTREAM_URL", defaultRouterUpstream),
		upstreamAuth: upstreamAuth{
			key:    os.Getenv("UPSTREAM_AUTH_KEY"),
			header: getEnv("UPSTREAM_AUTH_HEADER", ""),
		},
		peerConfigPath:   getEnv("PEER_CONFIG_PATH", defaultPeerConfigPath),
		peerPollInterval: getEnvDuration("PEER_POLL_INTERVAL", defaultPeerPollInterval),
		healthInterval:   getEnvDuration("HEALTH_INTERVAL", defaultHealthInterval),
		healthTimeout:    getEnvDuration("HEALTH_TIMEOUT", defaultHealthTimeout),
		healthThreshold:  getEnvInt("HEALTH_THRESHOLD", defaultHealthThreshold),
		max429Rate:       getEnvFloat("MAX_429_RATE", defaultMax429Rate),
		detectionWindow:  getEnvDuration("DETECTION_WINDOW", defaultDetectionWindow),
		detectorInterval: getEnvDuration("DETECTOR_INTERVAL", defaultDetectorInterval),
		fallbackRate:     getEnvFloat("FALLBACK_RATE", defaultFallbackRate),
		fallbackMaxConc:  getEnvInt("FALLBACK_MAX_CONCURRENT", defaultFallbackMaxConc),
	}
}

func main() {
	cfg := loadRouterConfig()

	fleet := newRelayFleet(cfg.healthThreshold, cfg.detectionWindow)
	metrics := newRouterMetrics()
	detector := newDetector429(fleet, cfg.max429Rate, 0)

	fallback, err := newFallbackProxy(cfg.upstreamURL, cfg.fallbackRate, cfg.fallbackMaxConc)
	if err != nil {
		log.Fatalf("relay-router: fallback init: %v", err)
	}
	fallback.withUpstreamAuth(cfg.upstreamAuth)

	proxy := newRouterProxy(fleet, detector, metrics, 0, fallback).withUpstreamAuth(cfg.upstreamAuth)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Llmsafespaces-Version", version.Version)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		statuses := fleet.HealthyRelays()
		for _, s := range statuses {
			metrics.setRelayHealthy(s.ID, s.Healthy)
			metrics.setActiveStreams(s.ID, s.ActiveStreams)
			metrics.setRelayEgress(s.ID, s.EgressBytes)
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		metrics.writePrometheus(w)
	})
	mux.Handle("/", proxy)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hc := newHealthChecker(fleet, cfg.healthInterval, cfg.healthTimeout, 0)
	go hc.run(ctx)

	go detector.runPeriodicCheck(ctx, cfg.detectorInterval)

	go pollPeerConfig(ctx, cfg.peerConfigPath, cfg.peerPollInterval, fleet)

	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("relay-router: shutting down...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		cancel()
	}()

	log.Printf("relay-router: listening on %s, upstream=%s", cfg.listenAddr, cfg.upstreamURL)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("relay-router: %v", err)
	}
	log.Println("relay-router: stopped")
}

func pollPeerConfig(ctx context.Context, path string, interval time.Duration, fleet *relayFleet) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	loadPeerConfig(path, fleet)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			loadPeerConfig(path, fleet)
		}
	}
}

// loadPeerConfig reads the peer config file once and applies it to the fleet.
// Extracted from pollPeerConfig so tests can drive the load synchronously
// without relying on goroutine scheduling and time.Sleep windows. See
// worklog 0470.
func loadPeerConfig(path string, fleet *relayFleet) {
	data, err := os.ReadFile(path)
	if err != nil {
		// File missing → ConfigMap was deleted (e.g. InferenceRelay CR
		// removed). Treat as "no peers" to drop orphaned relays from
		// the in-memory fleet. Without this, relays linger in metrics
		// and selection until pod restart (worklog 0467).
		if os.IsNotExist(err) {
			fleet.UpdatePeers(nil)
		}
		return
	}
	if strings.TrimSpace(string(data)) == "" {
		// Empty file — same as missing: drop all peers.
		fleet.UpdatePeers(nil)
		return
	}
	peerCfg, err := ParsePeerConfig(data)
	if err != nil {
		// Parse errors are transient — keep last-known-good rather
		// than draining the fleet on a temporary corruption.
		log.Printf("relay-router: parse peer config: %v", err)
		return
	}
	fleet.UpdatePeers(peerCfg.Relays)
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

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}
