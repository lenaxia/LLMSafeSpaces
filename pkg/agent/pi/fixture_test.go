// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// fixture_test.go — the recorded fixture transport: a Transport +
// SessionClient implementation over the testdata artifacts. This is
// the pi-seam application of the pkg/abi/abitest stance (a reference
// implementation the contract suite runs against): command responses
// replay the RECORDED envelopes (get_state/get_available_models/
// get_session_stats captured live from the pinned pi v1.5.2 against a
// fixture OpenAI endpoint on 2026-11-13; the session file is the real
// persisted artifact), get_entries is COMPUTED from the persisted file
// (honoring the durable since cursor exactly as rpc-commands.md
// specifies), and Events replays the recorded/doc-derived event
// stream. prompt/steer/follow_up append real entries to the file so
// history, cursor, and verification flows evolve like a live session.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

const fixtureSessionID = "aaaabbbb-cccc-dddd-eeee-ffff00001111"

// fixtureTransport implements Transport over a session dir on disk.
type fixtureTransport struct {
	t             *testing.T
	dir           string // the session dir (files named <ts>_<id>.jsonl)
	mu            sync.Mutex
	calls         []fixtureCall            // every command issued, in order
	subs          map[string][]*fixtureSub // per-session live subscribers
	down          bool                     // "pi is down": all RPC surfaces fail
	drainResponse []string                 // texts clear_queue returns (queued input)
	eventsScript  []byte                   // raw records replayed per subscription
	clock         func() time.Time         // appended-entry timestamps
}

type fixtureSub struct {
	ch     chan EventRecord
	closed bool
}

type fixtureCall struct {
	Session string
	Command string
	Cmd     map[string]any
}

func newFixtureTransport(t *testing.T) *fixtureTransport {
	t.Helper()
	dir := t.TempDir()
	ft := &fixtureTransport{
		t:     t,
		dir:   dir,
		subs:  map[string][]*fixtureSub{},
		clock: time.Now,
	}
	ft.seedFile(fixtureSessionID, "testdata/fixture_session.jsonl")
	events, err := os.ReadFile("testdata/events_stream.jsonl")
	if err != nil {
		t.Fatalf("read events fixture: %v", err)
	}
	ft.eventsScript = events
	return ft
}

// seedFile copies a fixture .jsonl into the session dir under a
// recorded-style name.
func (ft *fixtureTransport) seedFile(sessionID, src string) {
	ft.t.Helper()
	in, err := os.ReadFile(src)
	if err != nil {
		ft.t.Fatalf("read seed %s: %v", src, err)
	}
	name := fmt.Sprintf("2026-11-13T07-15-23-825Z_%s.jsonl", sessionID)
	if err := os.WriteFile(filepath.Join(ft.dir, name), in, 0o644); err != nil {
		ft.t.Fatalf("seed %s: %v", name, err)
	}
}

func (ft *fixtureTransport) record(session, command string, cmd map[string]any) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.calls = append(ft.calls, fixtureCall{Session: session, Command: command, Cmd: cmd})
}

// commandsOf returns the recorded command names issued against one
// session (or workspace-level, session "").
func (ft *fixtureTransport) commandsOf(session string) []string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	var out []string
	for _, c := range ft.calls {
		if c.Session == session {
			out = append(out, c.Command)
		}
	}
	return out
}

func (ft *fixtureTransport) lastCommand(session, command string) map[string]any {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	for i := len(ft.calls) - 1; i >= 0; i-- {
		if ft.calls[i].Session == session && ft.calls[i].Command == command {
			return ft.calls[i].Cmd
		}
	}
	return nil
}

// setDown simulates the pi runtime being down (files remain readable).
func (ft *fixtureTransport) setDown() {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.down = true
}

func (ft *fixtureTransport) isDown() bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.down
}

// --- Transport ----------------------------------------------------------

func (ft *fixtureTransport) Session(ctx context.Context, userID, workspaceID, sessionID string) (SessionClient, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	if ft.isDown() {
		return nil, fmt.Errorf("fixture: pi is down")
	}
	if ft.sessionFile(sessionID) == "" {
		return nil, errSessionFileNotFound(sessionID)
	}
	return &fixtureClient{ft: ft, session: sessionID}, nil
}

func (ft *fixtureTransport) CreateSession(ctx context.Context, userID, workspaceID, sessionID string) (SessionClient, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	if ft.isDown() {
		return nil, fmt.Errorf("fixture: pi is down")
	}
	name := fmt.Sprintf("2026-11-13T11-00-00-000Z_%s.jsonl", sessionID)
	hdr := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-11-13T11:00:00.000Z","cwd":"/workspace"}`+"\n", sessionID)
	if err := os.WriteFile(filepath.Join(ft.dir, name), []byte(hdr), 0o644); err != nil {
		return nil, err
	}
	return &fixtureClient{ft: ft, session: sessionID}, nil
}

func (ft *fixtureTransport) Control(ctx context.Context, userID, workspaceID string) (SessionClient, error) {
	if ft.isDown() {
		return nil, fmt.Errorf("fixture: pi is down")
	}
	return &fixtureClient{ft: ft, session: ""}, nil
}

func (ft *fixtureTransport) ListSessionFiles(ctx context.Context, userID, workspaceID string) ([]SessionFileMeta, error) {
	entries, err := os.ReadDir(ft.dir)
	if err != nil {
		return nil, err
	}
	var out []SessionFileMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, SessionFileMeta{
			ID:         sessionIDFromName(e.Name()),
			Name:       e.Name(),
			ModifiedAt: info.ModTime(),
			SizeBytes:  info.Size(),
		})
	}
	return out, nil
}

func (ft *fixtureTransport) OpenSessionFile(ctx context.Context, userID, workspaceID, sessionID string) (io.ReadCloser, error) {
	name := ft.sessionFile(sessionID)
	if name == "" {
		return nil, errSessionFileNotFound(sessionID)
	}
	return os.Open(filepath.Join(ft.dir, name))
}

func (ft *fixtureTransport) RemoveSessionFile(ctx context.Context, userID, workspaceID, sessionID string) error {
	name := ft.sessionFile(sessionID)
	if name == "" {
		return errSessionFileNotFound(sessionID)
	}
	return os.Remove(filepath.Join(ft.dir, name))
}

// sessionFile finds the newest persisted file for one id (the
// supervisor's newest-wins rule).
func (ft *fixtureTransport) sessionFile(sessionID string) string {
	entries, err := os.ReadDir(ft.dir)
	if err != nil {
		return ""
	}
	var newest string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if sessionIDFromName(e.Name()) != sessionID {
			continue
		}
		if newest == "" || e.Name() > newest {
			newest = e.Name()
		}
	}
	return newest
}

// --- SessionClient ------------------------------------------------------

type fixtureClient struct {
	ft      *fixtureTransport
	session string
}

func (fc *fixtureClient) Call(ctx context.Context, command map[string]any) (json.RawMessage, error) {
	name, _ := command["type"].(string)
	fc.ft.record(fc.session, name, command)
	switch name {
	case "get_state":
		return fc.replaySessionEnvelope("testdata/get_state_response.json")
	case "get_session_stats":
		return fc.replaySessionEnvelope("testdata/get_session_stats_response.json")
	case "get_available_models":
		return fc.ft.replayEnvelope("testdata/get_available_models_response.json")
	case "get_entries":
		return fc.getEntries(command)
	case "prompt", "steer", "follow_up":
		return fc.deliver(command, name)
	case "set_session_name":
		msg, _ := command["name"].(string)
		fc.ft.appendEntry(fc.session, map[string]any{"type": "session_info", "id": ftNewID(fc.ft), "parentId": nil, "timestamp": fc.ft.now().Format("2006-01-02T15:04:05.000Z"), "name": msg})
		return okEnvelope(name, nil), nil
	case "set_model":
		return okEnvelope(name, nil), nil
	case "clear_queue":
		fc.ft.mu.Lock()
		drain := append([]string(nil), fc.ft.drainResponse...)
		fc.ft.mu.Unlock()
		return okEnvelope(name, map[string]any{"steering": drain, "followUp": []string{}}), nil
	case "abort":
		fc.ft.broadcast(fc.session, []byte(`{"type":"agent_settled","aborted":true}`))
		return okEnvelope(name, nil), nil
	default:
		return okEnvelope(name, nil), nil
	}
}

// replaySessionEnvelope replays a recorded response with the requesting
// session's identity stamped in (a child pinned via --session-id
// reports its own sessionId/sessionFile — the recorded capture cannot
// know it).
func (fc *fixtureClient) replaySessionEnvelope(path string) (json.RawMessage, error) {
	raw, err := fc.ft.replayEnvelope(path)
	if err != nil {
		return nil, err
	}
	if fc.session == "" {
		return raw, nil
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return raw, nil
	}
	var data map[string]any
	if err := json.Unmarshal(env["data"], &data); err != nil {
		return raw, nil
	}
	data["sessionId"] = fc.session
	data["sessionFile"] = fmt.Sprintf("/fixture/sessions/2026-11-13T07-15-23-825Z_%s.jsonl", fc.session)
	env["data"], _ = json.Marshal(data)
	out, err := json.Marshal(env)
	if err != nil {
		return raw, nil
	}
	return out, nil
}

func (fc *fixtureClient) Events(ctx context.Context) (<-chan EventRecord, error) {
	sub := &fixtureSub{ch: make(chan EventRecord, 64)}
	fc.ft.mu.Lock()
	fc.ft.subs[fc.session] = append(fc.ft.subs[fc.session], sub)
	fc.ft.mu.Unlock()
	go func() {
		<-ctx.Done()
		fc.ft.mu.Lock()
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
		fc.ft.mu.Unlock()
	}()
	return sub.ch, nil
}

// deliver simulates a run: append the user + assistant entries to the
// persisted file (current-clock timestamps so since-floors behave),
// replay the event script to subscribers, and answer with the started
// disposition.
func (fc *fixtureClient) deliver(command map[string]any, name string) (json.RawMessage, error) {
	text, _ := command["message"].(string)
	now := fc.ft.now()
	fc.ft.appendEntry(fc.session, map[string]any{
		"type": "message", "id": ftNewID(fc.ft), "parentId": nil,
		"timestamp": now.Format("2006-01-02T15:04:05.000Z"),
		"message":   map[string]any{"role": "user", "content": text, "timestamp": now.UnixMilli()},
	})
	fc.ft.replayScript(fc.session)
	then := fc.ft.now()
	fc.ft.appendEntry(fc.session, map[string]any{
		"type": "message", "id": ftNewID(fc.ft), "parentId": nil,
		"timestamp": then.Format("2006-01-02T15:04:05.000Z"),
		"message": map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "persisted reply"}},
			"api": "openai-completions", "provider": "fixture", "model": "fixture-model",
			"usage": map[string]any{"input": 111, "output": 7, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 118,
				"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}},
			"stopReason": "stop", "timestamp": then.UnixMilli(),
		},
	})
	return okEnvelope(name, map[string]any{"disposition": "started"}), nil
}

func (ft *fixtureTransport) now() time.Time {
	ft.mu.Lock()
	c := ft.clock
	ft.mu.Unlock()
	return c().UTC()
}

// getEntries computes the entries response from the persisted file,
// honoring the durable since cursor (entries strictly after the id).
func (fc *fixtureClient) getEntries(command map[string]any) (json.RawMessage, error) {
	since, _ := command["since"].(string)
	name := fc.ft.sessionFile(fc.session)
	if name == "" {
		return nil, &piCallError{command: "get_entries", message: "session not found"}
	}
	f, err := os.Open(filepath.Join(fc.ft.dir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var (
		entries  []map[string]any
		leaf     string
		skipping = since != ""
		found    = !skipping
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(line, &probe); err != nil || probe.Type == "session" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if skipping {
			if probe.ID == since {
				skipping = false
				found = true
			}
			leaf = probe.ID
			continue
		}
		entries = append(entries, e)
		leaf = probe.ID
	}
	if skipping || !found {
		return nil, &piCallError{command: "get_entries", message: "since not found"}
	}
	var leafAny any
	if leaf != "" {
		leafAny = leaf
	}
	return json.Marshal(map[string]any{"type": "response", "command": "get_entries", "success": true,
		"data": map[string]any{"entries": entries, "leafId": leafAny}})
}

// --- helpers ------------------------------------------------------------

func (ft *fixtureTransport) replayEnvelope(path string) (json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

func (ft *fixtureTransport) replayScript(session string) {
	ft.mu.Lock()
	subs := append([]*fixtureSub(nil), ft.subs[session]...)
	ft.mu.Unlock()
	sc := bufio.NewScanner(bytes.NewReader(ft.eventsScript))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		annotated := injectSessionID(line, session)
		ft.mu.Lock()
		for _, sub := range subs {
			if sub.closed {
				continue
			}
			select {
			case sub.ch <- EventRecord{Raw: annotated}:
			default:
			}
		}
		ft.mu.Unlock()
	}
}

func (ft *fixtureTransport) broadcast(session string, raw []byte) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	for _, sub := range ft.subs[session] {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- EventRecord{Raw: injectSessionID(raw, session)}:
		default:
		}
	}
}

// appendEntry appends one JSON line to the session's persisted file,
// chained onto the current leaf (pi appends at the active position —
// the file's last line is always the newest append).
func (ft *fixtureTransport) appendEntry(session string, e map[string]any) {
	name := ft.sessionFile(session)
	if name == "" {
		return
	}
	path := filepath.Join(ft.dir, name)
	parent := ft.lastEntryID(path)
	e["parentId"] = parent
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(raw, '\n'))
}

// lastEntryID reads the last non-header entry id in a session file.
func (ft *fixtureTransport) lastEntryID(path string) any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var last any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var probe struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(sc.Bytes(), &probe); err != nil || probe.Type == "session" || probe.ID == "" {
			continue
		}
		last = probe.ID
	}
	return last
}

var ftIDCounter int64

func ftNewID(ft *fixtureTransport) string {
	return fmt.Sprintf("f%07d", func() int64 {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		ftIDCounter++
		return ftIDCounter
	}())
}

func okEnvelope(command string, data any) json.RawMessage {
	m := map[string]any{"type": "response", "command": command, "success": true}
	if data != nil {
		m["data"] = data
	}
	raw, _ := json.Marshal(m)
	return raw
}

// ensure agent import is exercised even in reduced builds (the seam
// under test).
var _ = agent.ErrSessionNotFound
