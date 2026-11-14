// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_bridge.go — the harness-port HTTP surface (S5, the "agentd
// bridge" S4's transport contract names): supervise-pi serves
// agentd.AgentPort (4096 — the port opencode serve holds on opencode
// pods; on pi pods the bridge holds it) so the API server's pi
// Transport (pkg/agent/pi/podtransport.go) drives the in-pod RPC
// children exactly where the opencode adapter drives opencode serve.
//
// The bridge is a DUMB PIPE by contract: it authenticates (the same
// per-workspace Basic auth opencode serve used — username
// agentd.AuthUsername, password from the workspace password secret),
// routes, and streams; every byte of pi RPC wire knowledge (command
// names, response envelopes, event records) stays inside
// pkg/agent/pi (S4) and the S2 supervisor. No compatibility shims
// onto opencode's route shapes — a pi pod is honestly a pi pod.
//
// Surface (bridge v1, mirroring the S4 Transport interface 1:1):
//
//	GET    /v1/pi/sessions             ListSessionFiles
//	POST   /v1/pi/sessions/{id}        CreateSession  (spawn pinned child)
//	DELETE /v1/pi/sessions/{id}        RemoveSessionFile (unlink + release)
//	GET    /v1/pi/sessions/{id}/file   OpenSessionFile (raw JSONL)
//	POST   /v1/pi/sessions/{id}/rpc    Session Call    (attach + command)
//	POST   /v1/pi/control/rpc          Control Call    (ephemeral --no-session child)
//	GET    /v1/pi/sessions/{id}/events Events          (SSE of raw records)
//
// SSE framing: one `data: <record>\n\n` per stdout record — the same
// one-line-per-record contract the JSONL pump sees; the API-side
// transport injects the session id the S4 contract requires.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// bridgeSessionIDRe is the bridge's id guard (S4's validateSessionID
// stance: pi's charset minus '.', so ids stay path-safe for every
// transport; mirrored here because cmd cannot import the unexported
// pkg/agent/pi validator — the shared constraint is pi's documented
// --session-id charset, cited in both files).
var bridgeSessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// eventHub is the bridge's per-session raw-event fan-out: the
// aggregator tap publishes; SSE subscribers receive. Bounded channels
// (64 records): a slow consumer drops records rather than blocking
// the pump — an SSE replay gap surfaces to the client as a stream
// hiccup, never as a wedged harness.
type eventHub struct {
	mu   sync.Mutex
	subs map[string]map[chan []byte]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[string]map[chan []byte]struct{})}
}

func (h *eventHub) subscribe(sessionID string) (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[sessionID]
	if !ok {
		set = make(map[chan []byte]struct{})
		h.subs[sessionID] = set
	}
	set[ch] = struct{}{}
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if set, ok := h.subs[sessionID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.subs, sessionID)
			}
		}
	}
	return ch, cancel
}

func (h *eventHub) publish(sessionID string, raw []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[sessionID] {
		select {
		case ch <- raw:
		default: // slow consumer: drop, never block the pump
		}
	}
}

// piBridge serves bridge v1 from the supervise-pi process.
type piBridge struct {
	log        *zap.Logger
	sup        *piSupervisor
	sessionDir string
	hub        *eventHub
	password   string
	srv        *http.Server
}

// newPiBridge constructs the bridge and installs the aggregator tap
// (the SSE fan-out's feed). The password is the workspace password
// (readAgentPassword — the same OPENCODE_SERVER_PASSWORD delivery the
// controller wires for both harnesses).
func newPiBridge(sup *piSupervisor, password string) *piBridge {
	b := &piBridge{
		log:        sup.log.With(zap.String("component", "pi_bridge")),
		sup:        sup,
		sessionDir: sup.cfg.SessionDir,
		hub:        newEventHub(),
		password:   password,
	}
	sup.agg.setEventTap(func(sessionID string, raw []byte) {
		if sessionID != "" {
			b.hub.publish(sessionID, raw)
		}
	})
	return b
}

// handler builds the authenticated mux.
func (b *piBridge) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pi/sessions", b.handleSessionList)
	mux.HandleFunc("/v1/pi/sessions/", b.handleSessionRoutes)
	mux.HandleFunc("/v1/pi/control/rpc", b.handleControlRPC)
	return b.basicAuth(mux)
}

// basicAuth enforces the workspace password with the same Basic
// contract opencode serve enforced (username agentd.AuthUsername).
// Constant-time compare; no password configured (dev) fails CLOSED
// for remote requests — a bridge without a password never serves.
func (b *piBridge) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.password == "" || b.password == "change-me" {
			http.Error(w, `{"error":"bridge password unconfigured"}`, http.StatusServiceUnavailable)
			return
		}
		user, pass, ok := r.BasicAuth()
		passOK := ok && user == agentd.AuthUsername &&
			subtleConstantTimeEq(pass, b.password)
		if !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="llmsafespaces"`)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func subtleConstantTimeEq(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// serve binds the harness port and serves until ln closes. Called on
// the supervisor's root context; shutdown tears the server down.
func (b *piBridge) serve(ln net.Listener) error {
	b.srv = &http.Server{Handler: b.handler(), ReadHeaderTimeout: 10 * time.Second}
	return b.srv.Serve(ln)
}

func (b *piBridge) close() {
	if b.srv != nil {
		_ = b.srv.Close()
	}
}

// --- handlers ------------------------------------------------------------

func (b *piBridge) handleSessionList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	ids, err := piScanSessionDir(b.sessionDir)
	if err != nil {
		http.Error(w, `{"error":"session dir scan failed"}`, http.StatusInternalServerError)
		return
	}
	type fileMeta struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		ModifiedAt string `json:"modifiedAt"`
		SizeBytes  int64  `json:"sizeBytes"`
	}
	out := make([]fileMeta, 0, len(ids))
	for _, id := range ids {
		path, err := piSessionPath(b.sessionDir, id)
		if err != nil {
			continue
		}
		info, err := os.Stat(path) //nolint:gosec // within the session dir
		if err != nil {
			continue
		}
		out = append(out, fileMeta{
			ID: id, Name: filepath.Base(path),
			ModifiedAt: info.ModTime().UTC().Format(time.RFC3339Nano),
			SizeBytes:  info.Size(),
		})
	}
	writeJSONBridge(w, http.StatusOK, map[string]any{"sessions": out})
}

// handleSessionRoutes dispatches /v1/pi/sessions/{id}[/{rpc,file,events}].
func (b *piBridge) handleSessionRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/pi/sessions/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, `{"error":"session id required"}`, http.StatusBadRequest)
		return
	}
	id := parts[0]
	if !bridgeSessionIDRe.MatchString(id) {
		http.Error(w, `{"error":"invalid session id charset"}`, http.StatusBadRequest)
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodPost:
		b.handleSessionCreate(w, r, id)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		b.handleSessionRemove(w, r, id)
	case len(parts) == 2 && parts[1] == "rpc" && r.Method == http.MethodPost:
		b.handleSessionRPC(w, r, id)
	case len(parts) == 2 && parts[1] == "file" && r.Method == http.MethodGet:
		b.handleSessionFile(w, r, id)
	case len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet:
		b.handleSessionEvents(w, r, id)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// handleSessionCreate spawns the session's pinned child (attach
// semantics: an existing id resumes its persisted session — the S4
// CreateSession contract is spawn-with-id, identical here).
func (b *piBridge) handleSessionCreate(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := b.sup.policy.Attach(r.Context(), id); err != nil {
		writeJSONBridge(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSONBridge(w, http.StatusCreated, map[string]any{"ok": true, "sessionID": id})
}

// handleSessionRemove unlinks the persisted file and releases the
// live child ("Sessions can be removed by deleting their .jsonl
// files", session-format.md — the S4 RemoveSessionFile contract).
func (b *piBridge) handleSessionRemove(w http.ResponseWriter, r *http.Request, id string) {
	path, err := piSessionPath(b.sessionDir, id)
	if err != nil {
		// Already gone: idempotent success (delete-of-absent).
		writeJSONBridge(w, http.StatusOK, map[string]any{"ok": true, "sessionID": id})
		return
	}
	b.sup.policy.Release(id)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		writeJSONBridge(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSONBridge(w, http.StatusOK, map[string]any{"ok": true, "sessionID": id})
}

// handleSessionRPC issues one command against the session's child.
// The body is the caller-built command object; the response is the
// child's full response record (the S4 call() decodes the envelope
// client-side — the bridge never inspects it).
func (b *piBridge) handleSessionRPC(w http.ResponseWriter, r *http.Request, id string) {
	var cmd map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&cmd); err != nil {
		http.Error(w, `{"error":"command body must be one JSON object"}`, http.StatusBadRequest)
		return
	}
	handle, err := b.sup.policy.Attach(r.Context(), id)
	if err != nil {
		writeJSONBridge(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	resp, err := handle.Call(r.Context(), cmd)
	if err != nil {
		writeJSONBridge(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// handleControlRPC issues one sessionless command against an
// ephemeral --no-session child (the S4 Control contract). Spawn-per-
// request is the recorded S5 cost posture: control commands are rare
// (model catalog), an ephemeral child never pollutes the session dir
// or the aggregator (no session id, no tap), and S7's validation epic
// measures whether a pooled control child is warranted.
func (b *piBridge) handleControlRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var cmd map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&cmd); err != nil {
		http.Error(w, `{"error":"command body must be one JSON object"}`, http.StatusBadRequest)
		return
	}
	cfg := b.sup.cfg
	cfg.SessionID = ""
	cfg.Ephemeral = true
	child := newPiChild(cfg, b.log, nil)
	if err := child.start(); err != nil {
		writeJSONBridge(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("control child spawn: %v", err)})
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = child.Shutdown(ctx, 2*time.Second, time.Second)
	}()
	resp, err := child.Call(r.Context(), cmd)
	if err != nil {
		writeJSONBridge(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// handleSessionFile streams the persisted JSONL (the VerifyDelivery /
// CountMessages / title-scan path — works while no child lives).
func (b *piBridge) handleSessionFile(w http.ResponseWriter, r *http.Request, id string) {
	path, err := piSessionPath(b.sessionDir, id)
	if err != nil {
		http.Error(w, `{"error":"session file not found"}`, http.StatusNotFound)
		return
	}
	f, err := os.Open(path) //nolint:gosec // within the session dir
	if err != nil {
		http.Error(w, `{"error":"session file unreadable"}`, http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "") // header flush before the body copy
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// handleSessionEvents streams the session's live records over SSE.
// Subscribing before prompting is the documented order (rpc.md) —
// the endpoint subscribes FIRST, then attaches the child so replay
// from zero is impossible by construction for records emitted after
// the subscribe.
func (b *piBridge) handleSessionEvents(w http.ResponseWriter, r *http.Request, id string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming unsupported"}`, http.StatusInternalServerError)
		return
	}
	ch, cancel := b.hub.subscribe(id)
	defer cancel()
	// Attach so a session nobody prompted yet has a live child to
	// prompt; an attach failure for a persisted session still streams
	// (the next attach attempt rides the rpc endpoint).
	if _, err := b.sup.policy.Attach(r.Context(), id); err != nil {
		b.log.Debug("pi bridge: events attach failed; streaming anyway",
			zap.String("session", id), zap.Error(err))
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, ": subscribed %s\n\n", id)
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case raw := <-ch:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeJSONBridge(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
