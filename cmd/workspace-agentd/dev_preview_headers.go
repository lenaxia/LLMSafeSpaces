// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// dev_preview_headers.go — design 0062 (r30): the agent's own
// dev-preview header configuration, as literal values.
//
// The tool takes ONLY literal strings (action/name/value — there is
// no field that names a Secret, no env expansion, no file indirection;
// the schema pin in dev_preview_headers_test.go holds that surface).
// That absence is the entire security guard: no platform Secret is
// reachable by construction.
//
// State is a plain JSON map at /sandbox-runtime/dev-preview-headers.json
// — the memory-backed emptyDir class (wiped on pod death/suspend,
// surviving agentd container restarts), deliberately NOT the
// PVC-durable class: agent-owned literals should die with the pod.
// Mode 0600, written atomically (temp + os.Rename, the sessionstate
// cursor.go mechanics — no fsync: the storage class is tmpfs and the
// state is ephemeral by design).
//
// One consumer in both deployment topologies (this agentd process),
// so the path carries no LLMSAFESPACES_*_PATH override — the
// cross-process coordination those overrides exist for does not apply.

import (
	"encoding/json"
	"fmt"
	"net/textproto"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// defaultDevPreviewHeadersPath is the memory-backed state file (§3).
const defaultDevPreviewHeadersPath = "/sandbox-runtime/dev-preview-headers.json"

// §2's validation caps.
const (
	devPreviewHeadersMaxEntries  = 20
	devPreviewHeadersMaxNameLen  = 128
	devPreviewHeadersMaxValueLen = 4 * 1024
)

// devPreviewHeadersReserved is the §2 denylist in CANONICAL form
// (textproto.CanonicalMIMEHeaderKey — "TE" canonicalizes to "Te"):
// the tunnel's own Authorization, the cookie pair, Host, the
// hop-by-hop set (RFC 2616 §13.5.1 plus the non-standard
// Proxy-Connection), and Upgrade. Sec-WebSocket-* is a prefix match
// (below), covering every variant. X-Forwarded-*/Forwarded are
// deliberately ABSENT: the stdlib already stripped inbound copies
// before Rewrite, so agent-set forward headers are deliverable —
// §8's free forward-mode.
var devPreviewHeadersReserved = map[string]bool{
	"Authorization":       true,
	"Cookie":              true,
	"Set-Cookie":          true,
	"Host":                true,
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Proxy-Connection":    true,
}

// devPreviewHeadersAtomic holds the process-wide default store — the
// instance the MCP tool dispatch and the user-mux wiring share. Tests
// swap it (the resyncBaseURLAtomic precedent).
var devPreviewHeadersAtomic atomic.Value // *devPreviewHeaderStore

func init() {
	devPreviewHeadersAtomic.Store(newDevPreviewHeaderStore(defaultDevPreviewHeadersPath))
}

func currentDevPreviewHeaders() *devPreviewHeaderStore {
	return devPreviewHeadersAtomic.Load().(*devPreviewHeaderStore)
}

// devPreviewHeaderEntry is one configured header (the list action's
// machine-readable shape).
type devPreviewHeaderEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// devPreviewHeaderStore is the workspace's configured preview headers:
// an in-memory map persisted atomically to a JSON file. All methods
// are nil-safe (a nil store is zero configuration — the read paths
// return empty, the write paths refuse).
type devPreviewHeaderStore struct {
	mu      sync.RWMutex
	path    string
	entries map[string]string
}

// newDevPreviewHeaderStore builds a store over path, loading any
// existing state. Boot-load honors the same validation as the tool,
// IN FULL: per-entry rules (a hand-edited file cannot smuggle
// reserved/oversized/invalid names past the denylist) AND the ≤20-
// entry cap (beyond-cap valid entries drop — the kept set is the
// first 20 by sorted name, deterministic). An unparseable file loads
// empty. The state is advisory tooling state on memory-backed storage
// — §3's "failure semantics: none" — so a discard here starts the
// workspace header-clean rather than failing agentd or honoring
// smuggled entries; the next mutation rewrites the file valid.
func newDevPreviewHeaderStore(path string) *devPreviewHeaderStore {
	s := &devPreviewHeaderStore{path: path, entries: map[string]string{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return s
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if len(s.entries) >= devPreviewHeadersMaxEntries {
			break
		}
		canonical, err := validateDevPreviewHeader(name, m[name])
		if err != nil {
			continue
		}
		s.entries[canonical] = m[name]
	}
	return s
}

// set validates and stores one header (an existing name overwrites —
// within the entry cap). The mutation is all-or-nothing: a persist
// failure leaves both memory and file at the previous state.
func (s *devPreviewHeaderStore) set(name, value string) error {
	if s == nil {
		return fmt.Errorf("dev-preview header store is not available")
	}
	canonical, err := validateDevPreviewHeader(name, value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[canonical]; !exists && len(s.entries) >= devPreviewHeadersMaxEntries {
		return fmt.Errorf("dev-preview header limit reached (%d entries) — clear one before adding another", devPreviewHeadersMaxEntries)
	}
	next := copyDevPreviewHeaders(s.entries)
	next[canonical] = value
	if err := persistDevPreviewHeaders(s.path, next); err != nil {
		return fmt.Errorf("persisting dev-preview headers: %w", err)
	}
	s.entries = next
	return nil
}

// clear removes one header by name ("*" clears every entry). Clearing
// an absent name is an idempotent no-op; a structurally invalid name
// is refused (symmetric with set — an invalid name can never be
// stored, and silently accepting it hides caller error).
func (s *devPreviewHeaderStore) clear(name string) error {
	if s == nil {
		return fmt.Errorf("dev-preview header store is not available")
	}
	if name != "*" {
		if _, err := validateDevPreviewHeaderName(name); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := copyDevPreviewHeaders(s.entries)
	if name == "*" {
		next = map[string]string{}
	} else {
		delete(next, textproto.CanonicalMIMEHeaderKey(name))
	}
	if err := persistDevPreviewHeaders(s.path, next); err != nil {
		return fmt.Errorf("persisting dev-preview headers: %w", err)
	}
	s.entries = next
	return nil
}

// list returns the configured entries sorted by name (deterministic
// machine-readable output).
func (s *devPreviewHeaderStore) list() []devPreviewHeaderEntry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]devPreviewHeaderEntry, 0, len(s.entries))
	for name, value := range s.entries {
		out = append(out, devPreviewHeaderEntry{Name: name, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// headers returns a copy of the configured headers for injection.
func (s *devPreviewHeaderStore) headers() map[string]string {
	if s == nil {
		return map[string]string{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyDevPreviewHeaders(s.entries)
}

// count returns the number of configured entries.
func (s *devPreviewHeaderStore) count() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

func copyDevPreviewHeaders(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// persistDevPreviewHeaders writes the map atomically: deterministic
// temp file (0600) + rename, so a reader never observes a partial
// state. json.Marshal sorts map keys — the file is stable for
// byte-comparison.
func persistDevPreviewHeaders(path string, entries map[string]string) error {
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// validateDevPreviewHeader enforces §2's table and returns the
// canonicalized name. Every rule fails at tool-call time — there is
// no later failure path (a stored literal IS the injected value).
func validateDevPreviewHeader(name, value string) (string, error) {
	canonical, err := validateDevPreviewHeaderName(name)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("header value is required — clear removes an entry; an empty value is caller error, not a removal")
	}
	if len(value) > devPreviewHeadersMaxValueLen {
		return "", fmt.Errorf("header value exceeds the 4 KiB cap")
	}
	for i := 0; i < len(value); i++ {
		if !isValidHeaderFieldValueByte(value[i]) {
			return "", fmt.Errorf("header value contains bytes that are not valid header bytes (byte %d)", i)
		}
	}
	return canonical, nil
}

// validateDevPreviewHeaderName is the name half of §2's table:
// non-empty, ≤128 chars, printable token characters, canonicalized,
// not reserved. Shared by set (full validation) and clear (structural
// symmetry — an invalid name can never have been stored).
func validateDevPreviewHeaderName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("header name is required")
	}
	if len(name) > devPreviewHeadersMaxNameLen {
		return "", fmt.Errorf("header name exceeds the %d-character cap", devPreviewHeadersMaxNameLen)
	}
	for i := 0; i < len(name); i++ {
		if !isRFC7230TokenChar(name[i]) {
			return "", fmt.Errorf("header name %q is not a valid printable header name", name)
		}
	}
	canonical := textproto.CanonicalMIMEHeaderKey(name)
	if devPreviewHeadersReserved[canonical] || strings.HasPrefix(canonical, "Sec-Websocket-") {
		return "", fmt.Errorf("header name %q is reserved (tunnel/session machinery) and cannot be configured", canonical)
	}
	return canonical, nil
}

// isRFC7230TokenChar reports whether b is an RFC 7230 tchar — the
// byte set a header field-name may contain.
func isRFC7230TokenChar(b byte) bool {
	if 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' {
		return true
	}
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// isValidHeaderFieldValueByte implements exactly the byte semantics
// of x/net/httpguts.ValidHeaderFieldValue (the source of truth this
// mirrors): HTAB, SP, the visible ASCII range 0x21–0x7E, and
// obs-text 0x80–0xFF. CR/LF/NUL and the other control bytes are
// invalid — they cannot ride a header value.
func isValidHeaderFieldValueByte(b byte) bool {
	return b == '\t' || (0x20 <= b && b <= 0x7e) || b >= 0x80
}

// devPreviewHeadersTool is the tools/list registration (a package var
// so the schema pin in the tests introspects the exact schema the
// wire serves — no drift between the pin and the registration).
var devPreviewHeadersTool = mcpTool{
	Name:        "dev_preview_headers",
	Description: "Configure HTTP headers injected into this workspace's dev-preview requests (design 0062) — the fix for a previewed service that expects a header (a service API key, a test identity) and has no other way to receive one: the preview tunnel forwards only Content-Type/Accept/X-Request-ID from the caller. Actions: set {name, value} stores one LITERAL value (values are plain strings you supply — no secret references, no env expansion, no file indirection); clear {name} removes one entry, clear {name:\"*\"} removes all; list returns the sorted [{name, value}] array (echoing your own literals). Configured headers apply to EVERY preview port, last-writer over the caller's forwarded headers, injected at the agentd hop — including X-Forwarded-* names (an agent-set X-Forwarded-User reaches the service: free forward-mode). Limits: at most 20 entries; name at most 128 chars, canonicalized (Authorization, Cookie, Set-Cookie, Host, hop-by-hop, and Sec-WebSocket-* names are reserved); value at most 4 KiB of valid header bytes. State lives in memory-backed pod storage: it survives agentd container restarts but is wiped on suspend/pod deletion — re-set what you need after a resume. Not for: headers on non-preview traffic, or anything other than literal values you own.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"set", "clear", "list"},
				"description": "set stores one header; clear removes one entry (name \"*\" clears all); list returns the configured entries",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Header name — canonicalized, at most 128 chars; reserved names are refused (Authorization, Cookie, Set-Cookie, Host, hop-by-hop, Sec-WebSocket-*)",
			},
			"value": map[string]any{
				"type":        "string",
				"description": "The LITERAL header value — at most 4 KiB of valid header bytes; required for set, ignored otherwise",
			},
		},
		"required": []string{"action"},
	},
}

// mcpDevPreviewHeaders is the dev_preview_headers tool: set/clear/
// list over the shared default store. Tool-argument keys stay
// free-form per the wire's convention (the tool schema owns that
// layer); unknown keys are ignored, exactly like every other tool.
func mcpDevPreviewHeaders(args map[string]any) (string, error) {
	store := currentDevPreviewHeaders()
	action, _ := args["action"].(string)
	switch action {
	case "set":
		name, _ := args["name"].(string)
		value, _ := args["value"].(string)
		if name == "" {
			return "", fmt.Errorf("name is required for set")
		}
		if err := store.set(name, value); err != nil {
			return "", err
		}
		return marshalDevPreviewHeadersResult(map[string]any{
			"action":  "set",
			"name":    textproto.CanonicalMIMEHeaderKey(name),
			"entries": store.count(),
		})
	case "clear":
		name, _ := args["name"].(string)
		if name == "" {
			return "", fmt.Errorf("name is required for clear (a header name, or \"*\" for all)")
		}
		if err := store.clear(name); err != nil {
			return "", err
		}
		return marshalDevPreviewHeadersResult(map[string]any{
			"action":  "clear",
			"name":    name,
			"entries": store.count(),
		})
	case "list":
		return marshalDevPreviewHeadersResult(store.list())
	default:
		return "", fmt.Errorf("action must be one of set, clear, or list")
	}
}

func marshalDevPreviewHeadersResult(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
