// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// dev_preview_headers_test.go — design 0062 §6's test arms, red-first:
// the tool validation table (§2), the injection unit tests (§3, via
// httptest through devPreviewHandler), the X-Forwarded disposition
// arm (§1/§8), the unit-simulable storage-lifecycle arm (§3 — boot
// reload, 0600, atomic rename, the memory-backed-class source-scan),
// the literal-only schema pin (§2 — no field other than a literal
// value string), the MCP dispatch/registration arms, and the
// in-package integration arm (full JSON-RPC roundtrip: wire → tool →
// store → file → injected at the agentd hop). The pod-tier lifecycle
// arms (container-restart survival via the emptyDir, suspend/pod-death
// wipe) are e2e-tier per the design's own §6 tiering.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newTestStore builds a store over a temp dir (each test its own file).
func newTestStore(t *testing.T) *devPreviewHeaderStore {
	t.Helper()
	return newDevPreviewHeaderStore(filepath.Join(t.TempDir(), "dev-preview-headers.json"))
}

// swapDefaultDevPreviewHeaders points the process-wide default store
// (what the MCP tool dispatch and the mux wiring read) at an isolated
// temp file for one test, restoring the previous default on cleanup —
// the resyncBaseURLAtomic swap precedent.
func swapDefaultDevPreviewHeaders(t *testing.T) *devPreviewHeaderStore {
	t.Helper()
	old := currentDevPreviewHeaders()
	s := newTestStore(t)
	devPreviewHeadersAtomic.Store(s)
	t.Cleanup(func() { devPreviewHeadersAtomic.Store(old) })
	return s
}

// --- §2: the tool validation table ----------------------------------

func TestDevPreviewHeadersValidationTable(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		value   string
		wantErr string
	}{
		{"valid", "X-Service-Key", "abc123", ""},
		{"canonicalized lower", "x-service-key", "v", ""}, // set canonicalizes; list reports canonical
		{"valid token punctuation", "X-Custom-Header_1.0", "v", ""},
		{"empty name", "", "v", "header name"},
		{"non-canonical junk", "X Service Key", "v", "header name"},
		{"name control char", "X-Bad\x01Key", "v", "header name"},
		{"name too long", "X-" + strings.Repeat("a", 130), "v", "128"},
		{"denied Authorization", "Authorization", "Basic x", "reserved"},
		{"denied Authorization lower", "authorization", "Basic x", "reserved"},
		{"denied Cookie", "Cookie", "a=b", "reserved"},
		{"denied Set-Cookie", "Set-Cookie", "a=b", "reserved"},
		{"denied Host", "Host", "evil", "reserved"},
		{"denied Connection", "Connection", "close", "reserved"},
		{"denied Upgrade", "Upgrade", "websocket", "reserved"},
		{"denied TE hop-by-hop", "TE", "trailers", "reserved"},
		{"denied Trailer hop-by-hop", "Trailer", "X", "reserved"},
		{"denied Transfer-Encoding", "Transfer-Encoding", "chunked", "reserved"},
		{"denied Keep-Alive hop-by-hop", "Keep-Alive", "x", "reserved"},
		{"denied Proxy-Connection", "Proxy-Connection", "x", "reserved"},
		{"denied Sec-WebSocket-Key", "Sec-WebSocket-Key", "x", "reserved"},
		{"denied Sec-WebSocket-Version", "Sec-WebSocket-Version", "13", "reserved"},
		{"denied Sec-WebSocket-Extensions", "Sec-WebSocket-Extensions", "x", "reserved"},
		{"value empty", "X-K", "", "value"},
		{"value too large", "X-K", strings.Repeat("a", 4*1024+1), "4 KiB"},
		{"value invalid bytes", "X-K", "bad\x00value", "valid header bytes"},
		{"value CR bytes", "X-K", "a\rb", "valid header bytes"},
		{"value LF bytes", "X-K", "a\nb", "valid header bytes"},
		{"value tab ok", "X-K", "a\tb", ""},
		{"value high-byte ok", "X-K", "caf\xc3\xa9", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			err := s.set(tc.header, tc.value)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("set(%q, %q): unexpected error: %v", tc.header, tc.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("set(%q, %q): expected an error containing %q, got nil", tc.header, tc.value, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("set(%q, %q): error %q does not contain %q", tc.header, tc.value, err.Error(), tc.wantErr)
			}
		})
	}
}

// §2: X-Forwarded-* is deliberately NOT on the denylist — the stdlib
// has already stripped inbound copies before Rewrite, so an agent-set
// literal is deliverable (§8's free forward-mode).
func TestDevPreviewHeadersForwardedNamesAccepted(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"X-Forwarded-User", "X-Forwarded-Proto", "Forwarded"} {
		if err := s.set(name, "v"); err != nil {
			t.Fatalf("set(%q): %v", name, err)
		}
	}
}

// §2: the ≤20-entries cap.
func TestDevPreviewHeadersEntryCap(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 20; i++ {
		if err := s.set(fmt.Sprintf("X-Header-%d", i), "v"); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	err := s.set("X-Header-21", "v")
	if err == nil || !strings.Contains(err.Error(), "20") {
		t.Fatalf("21st entry: expected the cap error, got %v", err)
	}
	// Overwriting an existing entry does not consume a new slot.
	if err := s.set("X-Header-0", "v2"); err != nil {
		t.Fatalf("overwrite within cap: %v", err)
	}
}

// §2: clear by name and clear-all ("*"); list reports canonical names.
func TestDevPreviewHeadersClearAndList(t *testing.T) {
	s := newTestStore(t)
	if err := s.set("x-service-key", "abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.set("X-Other", "def"); err != nil {
		t.Fatal(err)
	}
	got := s.list()
	if len(got) != 2 || got[0].Name != "X-Other" || got[1].Name != "X-Service-Key" {
		t.Fatalf("list (sorted, canonical): %+v", got)
	}
	if err := s.clear("X-SERVICE-KEY"); err != nil { // canonicalized match
		t.Fatal(err)
	}
	if len(s.list()) != 1 {
		t.Fatalf("clear by name: %+v", s.list())
	}
	if err := s.set("x-service-key", "abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.clear("*"); err != nil {
		t.Fatal(err)
	}
	if len(s.list()) != 0 {
		t.Fatalf("clear all: %+v", s.list())
	}
	// Clearing an absent name is a no-op success (idempotent), per the
	// plain-tool semantics — there is no failure path (§3).
	if err := s.clear("X-Absent"); err != nil {
		t.Fatalf("clear absent: %v", err)
	}
	// A structurally INVALID name is refused — symmetric with set: an
	// invalid name can never be stored, but accepting it silently is
	// asymmetric and hides caller error (r1 correctness-2).
	for _, junk := range []string{"X Bad", "", "X-Bad\x01Name"} {
		if err := s.clear(junk); err == nil || !strings.Contains(err.Error(), "header name") {
			t.Fatalf("clear(%q): expected a header-name error, got %v", junk, err)
		}
	}
}

// --- §3: storage ------------------------------------------------------

// The unit-simulable lifecycle arm: a fresh store construction re-reads
// the JSON at boot.
func TestDevPreviewHeadersBootReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev-preview-headers.json")
	s1 := newDevPreviewHeaderStore(path)
	if err := s1.set("X-Service-Key", "abc123"); err != nil {
		t.Fatal(err)
	}
	s2 := newDevPreviewHeaderStore(path)
	entries := s2.list()
	if len(entries) != 1 || entries[0].Name != "X-Service-Key" || entries[0].Value != "abc123" {
		t.Fatalf("boot reload: %+v", entries)
	}
	if v, ok := s2.headers()["X-Service-Key"]; !ok || v != "abc123" {
		t.Fatalf("headers() after reload: %v", s2.headers())
	}
}

// Boot-load honors the same validation as the tool: a hand-edited or
// corrupt file cannot smuggle reserved/oversized entries into memory.
func TestDevPreviewHeadersBootLoadDropsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev-preview-headers.json")
	raw := `{
		"X-Good": "ok",
		"Authorization": "Basic smuggled",
		"X-Too-Big": "` + strings.Repeat("a", 4*1024+1) + `",
		"X-Bad\u0001Name": "v"
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := newDevPreviewHeaderStore(path).list()
	if len(entries) != 1 || entries[0].Name != "X-Good" || entries[0].Value != "ok" {
		t.Fatalf("boot load must keep only the valid entry: %+v", entries)
	}
}

// An unparseable file starts empty (the state is advisory tooling
// state on memory-backed storage; §3's failure semantics: none).
func TestDevPreviewHeadersBootLoadCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev-preview-headers.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if entries := newDevPreviewHeaderStore(path).list(); len(entries) != 0 {
		t.Fatalf("corrupt file must load empty: %+v", entries)
	}
	// And the first mutation repairs the file to valid JSON.
	s := newDevPreviewHeaderStore(path)
	if err := s.set("X-K", "v"); err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("post-mutation file not valid JSON: %v", err)
	}
}

// The boot-load cap (r1 correctness-1): a hand-edited file with more
// than 20 VALID entries cannot bypass the tool's ≤20-entry contract —
// the boot loop keeps the first 20 by sorted name and drops the rest,
// deterministically.
func TestDevPreviewHeadersBootLoadCapsEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev-preview-headers.json")
	file := make(map[string]string, 22)
	for i := 0; i < 22; i++ {
		file[fmt.Sprintf("X-Header-%02d", i)] = fmt.Sprintf("v%d", i)
	}
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := newDevPreviewHeaderStore(path).list()
	if len(entries) != 20 {
		t.Fatalf("boot load must cap at 20 entries, got %d", len(entries))
	}
	if entries[0].Name != "X-Header-00" || entries[19].Name != "X-Header-19" {
		t.Fatalf("the kept entries must be the first 20 by sorted name: %s…%s", entries[0].Name, entries[19].Name)
	}
	for _, e := range entries {
		if e.Name == "X-Header-20" || e.Name == "X-Header-21" {
			t.Fatalf("beyond-cap entry survived boot load: %+v", e)
		}
	}
}

// Boundary ACCEPTS (r1 missing-test 3): exactly-128-char names and
// exactly-4096-byte values are valid — a cap regression to >= would
// otherwise pass the rejection-side rows alone.
func TestDevPreviewHeadersBoundaryAccepts(t *testing.T) {
	s := newTestStore(t)
	name := "X-" + strings.Repeat("a", 126) // exactly 128
	if len(name) != 128 {
		t.Fatalf("fixture: name is %d chars", len(name))
	}
	if err := s.set(name, "v"); err != nil {
		t.Fatalf("128-char name must be accepted: %v", err)
	}
	if err := s.set("X-Max-Value", strings.Repeat("a", 4096)); err != nil {
		t.Fatalf("4096-byte value must be accepted: %v", err)
	}
}

// Mode 0600 and the atomic-rename write (no .tmp residue, file is JSON).
func TestDevPreviewHeadersFileShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev-preview-headers.json")
	s := newDevPreviewHeaderStore(path)
	if err := s.set("X-K", "v"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v, want 0600", fi.Mode().Perm())
	}
	var m map[string]string
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if m["X-K"] != "v" {
		t.Fatalf("content: %v", m)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp residue after atomic rename: %v", err)
	}
}

// A failed persist leaves the in-memory state untouched (no partial
// state: the mutation is all-or-nothing).
func TestDevPreviewHeadersPersistFailureAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev-preview-headers.json")
	s := newDevPreviewHeaderStore(path)
	if err := s.set("X-K", "v"); err != nil {
		t.Fatal(err)
	}
	// Occupy the deterministic temp name with a directory: the write
	// fails, the set must fail, and the previous state must survive.
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.set("X-New", "v2"); err == nil {
		t.Fatal("expected persist failure to surface")
	}
	entries := s.list()
	if len(entries) != 1 || entries[0].Name != "X-K" || entries[0].Value != "v" {
		t.Fatalf("failed set must not mutate state: %+v", entries)
	}
	// The same atomicity holds on clear (r1 missing-test 4).
	if err := s.clear("X-K"); err == nil {
		t.Fatal("expected persist failure to surface on clear")
	}
	if entries := s.list(); len(entries) != 1 || entries[0].Name != "X-K" {
		t.Fatalf("failed clear must not mutate state: %+v", entries)
	}
}

// The memory-backed-class source-scan: the state path is under
// /sandbox-runtime (the emptyDir-memory class), NOT the PVC-durable
// /platform the sessionstate precedent uses — the values must die with
// the pod (§3's deliberate choice).
func TestDevPreviewHeadersMemoryBackedClassSourceScan(t *testing.T) {
	if !strings.HasPrefix(defaultDevPreviewHeadersPath, "/sandbox-runtime/") {
		t.Fatalf("default path %q is not under /sandbox-runtime (the memory-backed class)", defaultDevPreviewHeadersPath)
	}
	if strings.Contains(defaultDevPreviewHeadersPath, "/platform") {
		t.Fatalf("default path %q must NOT be under the PVC-durable /platform", defaultDevPreviewHeadersPath)
	}
}

// The nil store behaves as zero configuration (existing dev-preview
// callers pass nil until they wire the shared default).
func TestDevPreviewHeadersNilStoreEmpty(t *testing.T) {
	var s *devPreviewHeaderStore
	if entries := s.list(); len(entries) != 0 {
		t.Fatalf("nil store list: %+v", entries)
	}
	if hdrs := s.headers(); len(hdrs) != 0 {
		t.Fatalf("nil store headers: %v", hdrs)
	}
	if n := s.count(); n != 0 {
		t.Fatalf("nil store count: %d", n)
	}
}

// --- §3/§1/§8: injection through the real handler --------------------

// throughPreview drives one request through devPreviewHandler to a
// fresh probe backend and returns the headers the backend received.
func throughPreview(t *testing.T, s *devPreviewHeaderStore, hdr map[string]string) http.Header {
	t.Helper()
	recv := make(chan http.Header, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recv <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	handler := devPreviewHandler("pw", s)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://agentd/v1/dev-preview/%d/", portOf(t, backend)), nil)
	req.Header.Set("Authorization", "Basic "+basicAuth("pw"))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview proxy status: %d", rec.Code)
	}
	return <-recv
}

// portOf extracts the backend's TCP port for the preview URL path.
func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	addr := srv.Listener.Addr().String()
	var port int
	if _, err := fmt.Sscanf(addr, "127.0.0.1:%d", &port); err != nil {
		t.Fatalf("backend addr %q: %v", addr, err)
	}
	return port
}

func TestDevPreviewHeadersInjection(t *testing.T) {
	s := newTestStore(t)
	if err := s.set("X-Service-Key", "abc123"); err != nil {
		t.Fatal(err)
	}

	got := throughPreview(t, s, map[string]string{
		"Content-Type": "text/html", // G34-allowlisted caller content
		"Accept":       "*/*",
	})
	if got.Get("X-Service-Key") != "abc123" {
		t.Fatalf("configured header not injected: %v", got)
	}
	if got.Get("Content-Type") != "text/html" {
		t.Fatalf("caller content not forwarded: %v", got)
	}
}

// Last-writer over the allowlisted three: a configured Content-Type
// replaces the caller's.
func TestDevPreviewHeadersLastWriter(t *testing.T) {
	s := newTestStore(t)
	if err := s.set("Content-Type", "application/json"); err != nil {
		t.Fatal(err)
	}

	got := throughPreview(t, s, map[string]string{"Content-Type": "text/html"})
	if got.Get("Content-Type") != "application/json" {
		t.Fatalf("configured header must be last-writer: %v", got)
	}
}

// The tunnel's own Authorization stays stripped even though the store
// cannot contain it (the denylist is belt; the strip is suspenders).
func TestDevPreviewHeadersAuthorizationStillStripped(t *testing.T) {
	s := newTestStore(t)

	got := throughPreview(t, s, nil) // the helper sets Basic auth
	if got.Get("Authorization") != "" {
		t.Fatalf("Authorization leaked to the dev server: %v", got)
	}
}

// §1/§8's disposition arm: an unconfigured X-Forwarded-For sent by the
// caller never arrives (the stdlib strips it before Rewrite); a
// CONFIGURED X-Forwarded-User IS delivered (the free forward-mode).
func TestDevPreviewHeadersForwardedDisposition(t *testing.T) {
	s := newTestStore(t)

	// Unconfigured: stripped by the stdlib before Rewrite.
	got := throughPreview(t, s, map[string]string{"X-Forwarded-For": "203.0.113.9"})
	if got.Get("X-Forwarded-For") != "" {
		t.Fatalf("unconfigured X-Forwarded-For must not reach the service (stdlib strip): %v", got)
	}

	// Configured: delivered intact (deliberately not on the denylist).
	if err := s.set("X-Forwarded-User", "agent"); err != nil {
		t.Fatal(err)
	}
	got = throughPreview(t, s, map[string]string{"X-Forwarded-For": "203.0.113.9"})
	if got.Get("X-Forwarded-User") != "agent" {
		t.Fatalf("configured X-Forwarded-User must be delivered: %v", got)
	}
	if got.Get("X-Forwarded-For") != "" {
		t.Fatalf("caller X-Forwarded-For still absent alongside the configured forward: %v", got)
	}
}

// WS-upgrade injection (r1 missing-test 2): Rewrite runs on the
// upgrade path too — the configured header must be present on the
// backend's 101 handshake request, so a future refactor confining the
// injection loop to the non-upgrade branch fails this pin.
func TestDevPreviewHeadersInjectedOnWSUpgrade(t *testing.T) {
	s := newTestStore(t)
	if err := s.set("X-Service-Key", "ws-key"); err != nil {
		t.Fatal(err)
	}

	recv := make(chan http.Header, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recv <- r.Header.Clone()
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("up"))
	}))
	t.Cleanup(backend.Close)

	agentd := httptest.NewServer(devPreviewHandler("pw", s))
	t.Cleanup(agentd.Close)

	wsURL := "ws://" + agentd.Listener.Addr().String() + "/v1/dev-preview/" + fmt.Sprint(portOf(t, backend)) + "/ws"
	header := http.Header{}
	header.Set("Authorization", "Basic "+basicAuth("pw"))
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		body := ""
		if resp != nil && resp.Body != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		t.Fatalf("WS dial through agentd failed: %v (status=%v body=%s)", err, resp, body)
	}
	defer conn.Close()

	select {
	case got := <-recv:
		if got.Get("X-Service-Key") != "ws-key" {
			t.Fatalf("configured header missing on the upgrade request: %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend never received the upgrade request")
	}
}

// --- §2: the MCP tool -------------------------------------------------

func TestDevPreviewHeadersToolDispatch(t *testing.T) {
	s := swapDefaultDevPreviewHeaders(t)
	ctx := context.Background()

	// list on a fresh store → empty array.
	out, err := callMCPTool(ctx, mcpTestPassword, "dev_preview_headers", map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("fresh list: %q", out)
	}

	// set → list reports the canonicalized entry.
	if _, err := callMCPTool(ctx, mcpTestPassword, "dev_preview_headers", map[string]any{
		"action": "set", "name": "x-service-key", "value": "abc123",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	out, err = callMCPTool(ctx, mcpTestPassword, "dev_preview_headers", map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, `"name":"X-Service-Key"`) || !strings.Contains(out, `"value":"abc123"`) {
		t.Fatalf("list after set: %q", out)
	}
	if _, ok := s.headers()["X-Service-Key"]; !ok {
		t.Fatalf("set must reach the shared default store: %v", s.headers())
	}

	// clear one → gone.
	if _, err := callMCPTool(ctx, mcpTestPassword, "dev_preview_headers", map[string]any{
		"action": "clear", "name": "X-Service-Key",
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(s.list()) != 0 {
		t.Fatalf("clear must remove the entry: %+v", s.list())
	}

	// clear "*" with entries → all gone.
	if _, err := callMCPTool(ctx, mcpTestPassword, "dev_preview_headers", map[string]any{
		"action": "set", "name": "X-A", "value": "1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCPTool(ctx, mcpTestPassword, "dev_preview_headers", map[string]any{
		"action": "set", "name": "X-B", "value": "2",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCPTool(ctx, mcpTestPassword, "dev_preview_headers", map[string]any{
		"action": "clear", "name": "*",
	}); err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if len(s.list()) != 0 {
		t.Fatalf("clear * must remove every entry: %+v", s.list())
	}
}

func TestDevPreviewHeadersToolDispatchErrors(t *testing.T) {
	swapDefaultDevPreviewHeaders(t)
	ctx := context.Background()

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing action", map[string]any{}, "action"},
		{"unknown action", map[string]any{"action": "fetch"}, "action"},
		{"non-string action", map[string]any{"action": 42}, "action"},
		{"set without name", map[string]any{"action": "set", "value": "v"}, "name"},
		{"set without value", map[string]any{"action": "set", "name": "X-K"}, "value"},
		{"set reserved via tool", map[string]any{"action": "set", "name": "Authorization", "value": "Basic x"}, "reserved"},
		{"clear without name", map[string]any{"action": "clear"}, "name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callMCPTool(ctx, mcpTestPassword, "dev_preview_headers", tc.args)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// The registration shape: the tool is on tools/list with EXACTLY the
// action/name/value properties (the literal-only surface, §2) and an
// enum-constrained action.
func TestDevPreviewHeadersToolRegistered(t *testing.T) {
	req, _ := json.Marshal(mcpRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(req))

	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	tools, _ := resp.Result.(map[string]any)["tools"].([]any)
	var tool map[string]any
	for _, raw := range tools {
		if m, _ := raw.(map[string]any); m["name"] == "dev_preview_headers" {
			tool = m
		}
	}
	if tool == nil {
		t.Fatal("dev_preview_headers not registered on tools/list")
	}
	schema, _ := tool["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "action,name,value" {
		t.Fatalf("schema properties must be exactly action,name,value: %v", names)
	}
	action, _ := props["action"].(map[string]any)
	enum, _ := action["enum"].([]any)
	if len(enum) != 3 {
		t.Fatalf("action enum must constrain set/clear/list: %v", enum)
	}
}

// devPreviewHeadersSchemaFields introspects the production tool
// definition's schema (the same var tools/list serves).
func devPreviewHeadersSchemaFields() []string {
	props, _ := devPreviewHeadersTool.InputSchema["properties"].(map[string]any)
	fields := make([]string, 0, len(props))
	for k := range props {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	return fields
}

// The literal-only pin: the tool's input schema has NO field other
// than the literal action/name/value strings — no Secret reference,
// no env expansion, no file indirection: literals-only is structural
// (§2's entire security guard), and this pin holds it that way.
func TestDevPreviewHeadersLiteralOnlySchema(t *testing.T) {
	for _, banned := range []string{"secretKeyref", "secret", "env", "file", "ref", "path", "resolve"} {
		for _, f := range devPreviewHeadersSchemaFields() {
			l := strings.ToLower(f)
			if strings.Contains(l, strings.ToLower(banned)) {
				t.Fatalf("schema field %q contains reference vocabulary %q — literals only, by construction", f, banned)
			}
		}
	}
}

// The in-package integration arm: a full JSON-RPC tools/call through
// mcpHandler (Basic-auth gate, #1561 strict wire) lands in the shared
// store, persists to the JSON file, and is injected at the agentd hop
// on the next preview request; clear reverts the injection.
func TestDevPreviewHeadersJSONRPCRoundtrip(t *testing.T) {
	s := swapDefaultDevPreviewHeaders(t)

	call := func(args map[string]any) mcpResponse {
		t.Helper()
		params, _ := json.Marshal(map[string]any{"name": "dev_preview_headers", "arguments": args})
		body, _ := json.Marshal(mcpRequest{JSONRPC: "2.0", ID: 7, Method: "tools/call", Params: params})
		w := httptest.NewRecorder()
		mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(body))
		var resp mcpResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal %s: %v", w.Body.String(), err)
		}
		if resp.Error != nil {
			t.Fatalf("tools/call error: %+v", resp.Error)
		}
		return resp
	}

	call(map[string]any{"action": "set", "name": "X-Service-Key", "value": "abc123"})

	// Persisted to the JSON state (0600, memory-backed class).
	var m map[string]string
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("state file not JSON: %v", err)
	}
	if m["X-Service-Key"] != "abc123" {
		t.Fatalf("state file content: %v", m)
	}

	// Injected at the agentd hop.
	got := throughPreview(t, s, nil)
	if got.Get("X-Service-Key") != "abc123" {
		t.Fatalf("configured header not injected after MCP set: %v", got)
	}

	// Clear reverts the injection.
	call(map[string]any{"action": "clear", "name": "X-Service-Key"})
	got = throughPreview(t, s, nil)
	if got.Get("X-Service-Key") != "" {
		t.Fatalf("cleared header still injected: %v", got)
	}
}

// --- §5: the feature_status integration --------------------------------

// TestDevPreviewHeadersFeatureStatus pins §5's entry: the five-field
// contract, source "tool" (local tool state, not projected platform
// config), controllable true (the agent owns this surface), active
// reflecting whether any header is configured.
func TestDevPreviewHeadersFeatureStatus(t *testing.T) {
	s := swapDefaultDevPreviewHeaders(t)

	out, err := mcpFeatureStatus()
	if err != nil {
		t.Fatal(err)
	}
	var flags []map[string]any
	if err := json.Unmarshal([]byte(out), &flags); err != nil {
		t.Fatalf("feature_status output not JSON: %v", err)
	}
	var entry map[string]any
	for _, f := range flags {
		if f["feature"] == "dev_preview_headers" {
			entry = f
		}
	}
	if entry == nil {
		t.Fatalf("dev_preview_headers entry missing from feature_status: %s", out)
	}
	if entry["source"] != "tool" {
		t.Fatalf("source: %v, want tool", entry["source"])
	}
	if entry["controllable"] != true {
		t.Fatalf("controllable: %v, want true", entry["controllable"])
	}
	if entry["active"] != false {
		t.Fatalf("active with zero entries: %v, want false", entry["active"])
	}
	if detail, _ := entry["source_detail"].(string); !strings.Contains(detail, "0 entries") {
		t.Fatalf("source_detail: %v", entry["source_detail"])
	}

	if err := s.set("X-Service-Key", "abc123"); err != nil {
		t.Fatal(err)
	}
	out, err = mcpFeatureStatus()
	if err != nil {
		t.Fatal(err)
	}
	flags = nil
	if err := json.Unmarshal([]byte(out), &flags); err != nil {
		t.Fatal(err)
	}
	for _, f := range flags {
		if f["feature"] != "dev_preview_headers" {
			continue
		}
		if f["active"] != true {
			t.Fatalf("active with one entry: %v, want true", f["active"])
		}
		if detail, _ := f["source_detail"].(string); !strings.Contains(detail, "1 entries") {
			t.Fatalf("source_detail: %v", f["source_detail"])
		}
		return
	}
	t.Fatal("entry vanished after set")
}
