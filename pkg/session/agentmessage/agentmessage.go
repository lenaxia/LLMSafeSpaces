// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agentmessage implements the v1 agent-message provenance
// sentinel contract (issue #1465): inter-session messages delivered by
// the agentd send_message tool carry a machine-detectable origin marker
// so frontends and receiving agents can distinguish agent traffic from
// human input.
//
// Sentinel format v1 — a single line at the START of the delivered
// message, followed by one newline and the message text:
//
//	<!-- lsp:agent-message-v1 {"fromSession":"ses_..."} -->
//
// The format is a stable API contract locked by the golden fixtures in
// testdata/ (authoritative), following the attachment-manifest-v1
// precedent. Changes are additive-only; any format change requires a
// new version marker while this parser keeps supporting v1. Unknown
// versions and unknown JSON keys parse as plain text (forward
// compatibility). fromSession is always present in a valid v1
// sentinel; workspace is a legal v1 key that v1 senders never emit —
// it exists so a future cross-workspace sender (#1260) needs no v2
// (omitted implies a local, same-workspace sender).
//
// Values ride as JSON, so structurally hostile content (the -->
// terminator, newlines, quotes) is escaped by the encoder and can
// never break the line — but session IDs come from the live session
// list at the sender (agentd validates existence by ID), and the
// sentinel is provenance metadata, NOT authentication: every session
// in a workspace shares the same pod credential, so a sender can
// always claim a sibling's ID. Compose is idempotent: any existing
// leading v1 sentinel is stripped before the fresh one is prepended,
// so Compose(Compose(m)) == Compose(m) and a re-sent message never
// double-sentinels.
package agentmessage

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// sentinelPrefix opens a v1 sentinel line; the JSON payload and the
// closing " -->" follow on the same line.
const (
	sentinelPrefix = "<!-- lsp:agent-message-v1 "
	sentinelSuffix = " -->"
)

// ErrEmptyFromSession rejects composing without the mandatory field.
var ErrEmptyFromSession = errors.New("fromSession is required")

// sentinelLinePattern matches one complete v1 sentinel line. The
// greedy payload match makes trailing content after the closing -->
// disqualify the line (strict single-line form), and json.Unmarshal
// then rejects anything whose payload is not a valid v1 Origin.
var sentinelLinePattern = regexp.MustCompile(
	`^<!-- lsp:agent-message-v1 (\{.*\}) -->$`)

// Origin is the provenance metadata carried by the sentinel.
// FromSession is the sending session's ID (mandatory). Workspace names
// the sending workspace for cross-workspace senders (future, #1260);
// omitted means the sender is local to the receiving workspace. Mode
// labels HOW the origin was established (#1469 hybrid ruling):
// "injected" — stamped by the platform's harness plugin from the
// harness's own session identity; "self-declared" — supplied by the
// sending model and validated by-ID (the degraded-pod fallback). The
// sentinel is metadata, never authentication: both modes carry the
// same trust boundary (a session can claim a sibling's ID under the
// shared pod credential) — the mode makes the DEGREE of platform
// attestation visible, nothing more.
type Origin struct {
	FromSession string `json:"fromSession"`
	Workspace   string `json:"workspace,omitempty"`
	Mode        string `json:"mode,omitempty"`
}

// Origin modes (#1469 hybrid ruling).
const (
	ModeInjected     = "injected"
	ModeSelfDeclared = "self-declared"
)

// Compose prepends a v1 sentinel carrying from to message, stripping
// any existing leading v1 sentinel first (idempotent re-sends). The
// JSON encoding keeps the sentinel a single line for any origin value.
func Compose(message string, from Origin) (string, error) {
	if strings.TrimSpace(from.FromSession) == "" {
		return "", fmt.Errorf("%w", ErrEmptyFromSession)
	}
	if from.Mode == "" {
		return "", fmt.Errorf("%w: mode is required", ErrEmptyFromSession)
	}
	payload, err := json.Marshal(from)
	if err != nil {
		return "", fmt.Errorf("agent-message-v1: encode origin: %w", err)
	}
	return sentinelPrefix + string(payload) + sentinelSuffix + "\n" + stripLeadingSentinel(message), nil
}

// Parse detects a leading v1 sentinel line and returns the origin,
// whether one was found, and the message text with the sentinel line
// (and its newline) removed. Anything else — no sentinel, interior or
// forged lines, unknown versions, malformed payloads, a missing or
// non-string fromSession, a non-string workspace — returns found=false
// with the text unchanged: the message payload is never broken by an
// unparseable sentinel. Known keys are matched CASE-SENSITIVELY and
// type-strictly (parity with the TS port, locked by shared fixtures);
// unknown keys are tolerated (additive-only contract).
func Parse(text string) (Origin, bool, string) {
	if !strings.HasPrefix(text, sentinelPrefix) {
		return Origin{}, false, text
	}
	line, rest, hasRest := strings.Cut(text, "\n")
	m := sentinelLinePattern.FindStringSubmatch(line)
	if m == nil {
		return Origin{}, false, text
	}
	origin, ok := decodeOrigin([]byte(m[1]))
	if !ok {
		return Origin{}, false, text
	}
	if !hasRest {
		rest = ""
	}
	return origin, true, rest
}

// decodeOrigin applies the strict v1 payload contract: fromSession
// must be present as a JSON string (non-blank); workspace and mode,
// when present, must be strings; unknown keys are ignored. mode is
// OPTIONAL on parse (pre-mode sentinels — none shipped, but the
// additive contract tolerates them — parse with an empty mode).
func decodeOrigin(payload []byte) (Origin, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Origin{}, false
	}
	from, ok := raw["fromSession"]
	if !ok {
		return Origin{}, false
	}
	var origin Origin
	if err := json.Unmarshal(from, &origin.FromSession); err != nil {
		return Origin{}, false
	}
	if strings.TrimSpace(origin.FromSession) == "" {
		return Origin{}, false
	}
	for key, dst := range map[string]*string{
		"workspace": &origin.Workspace,
		"mode":      &origin.Mode,
	} {
		if v, present := raw[key]; present {
			if err := json.Unmarshal(v, dst); err != nil {
				return Origin{}, false
			}
		}
	}
	return origin, true
}

// stripLeadingSentinel removes a leading v1 sentinel line (and its
// newline) so Compose never stacks sentinels.
func stripLeadingSentinel(message string) string {
	if _, found, rest := Parse(message); found {
		return rest
	}
	return message
}
