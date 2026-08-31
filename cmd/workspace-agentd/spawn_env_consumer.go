// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_env_consumer.go — design 0051 US-4a: the US-0.2(a) IPC handoff,
// consumer side.
//
// In sidecar mode the secrets-env FILE becomes unreadable by uid-1000
// (US-4b relocates it to a sidecar-only mount); the secrets cross to the
// opencode child via the control socket instead:
//
//   - BOOT: the sidecar parses the materializer's secrets-env delta and
//     pushes it BEFORE its muxes serve — and since the kubelet gates the
//     workspace container on the sidecar's startup probe, the push lands
//     before the supervisor's first spawn.
//   - RELOAD: the socketReloadProc re-reads the FRESH file and pushes the
//     delta at the moment a restart actually fires — the deferred
//     (session-aware) path therefore hands off the LATEST env, and the
//     push-then-restart ordering guarantees the next spawn sees it.
//
// Composition rule (A.4 forbids env OUT of the supervisor — the sidecar
// cannot learn the supervisor's parent env to compose it there): the
// sidecar sends ONLY the delta; the supervisor merges parent + delta
// with platform vars winning, mirroring buildEnvFrom's file-delta
// semantics.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// parseSecretsEnvDelta returns the variables the bash-sourceable env
// file introduces, excluding anything already present in THIS process's
// environment and excluding shell noise (SHLVL/PWD/OLDPWD/_).
//
// US-4b: the parser is PURE GO — the sidecar image is FROM scratch (no
// bash exists there), so the US-4a bash-source implementation could only
// fail in real sidecar pods. This is not the G2 bug class (parsing
// arbitrary bash): the file has exactly ONE writer, the materializer's
// applyEnvSecret/applyAPIKey via secrets.FormatEnvLine, whose output is
// the canonical `export NAME='shellSingleQuote(value)'` form. The
// scanner below is the exact inverse of that encoder and rejects
// anything else as corruption (single writer → malformed means corrupt).
func parseSecretsEnvDelta(path string) (map[string]string, error) {
	//nolint:gosec // G304: path is the deployment-configured secrets-env
	// coordinate (env override → const default), same trust class as
	// buildEnvFrom's source target.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil // absent = no env-secrets: normal
		}
		return nil, fmt.Errorf("read secrets-env %s: %w", path, err)
	}
	parent := os.Environ()
	parentSet := make(map[string]struct{}, len(parent))
	for _, e := range parent {
		if i := strings.IndexByte(e, '='); i > 0 {
			parentSet[e[:i]] = struct{}{}
		}
	}
	const noise = "SHLVL\x00PWD\x00OLDPWD\x00_\x00"

	parsed, err := scanShellquoteExports(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse secrets-env %s: %w", path, err)
	}
	delta := map[string]string{}
	for _, kv := range parsed {
		if _, inParent := parentSet[kv.name]; inParent {
			continue
		}
		if strings.Contains(noise, kv.name+"\x00") {
			continue
		}
		delta[kv.name] = kv.value
	}
	return delta, nil
}

// nameValue is one decoded `export NAME='value'` entry.
type nameValue struct {
	name, value string
}

// scanShellquoteExports decodes a stream of FormatEnvLine records:
//
//	"export " NAME "='" shellSingleQuote(value) "'\n"
//
// with shellSingleQuote(v) = "'" + v with each `'` replaced by `'\”`.
// Values may contain raw newlines (the quote spans lines). Any byte
// sequence that is not exactly this grammar is an error — the file's
// single writer never produces anything else, so a mismatch is
// corruption and must surface, not silently drop variables.
func scanShellquoteExports(data string) ([]nameValue, error) {
	var order []nameValue
	i := 0
	n := len(data)
	for i < n {
		rest := data[i:]
		const prefix = "export "
		if !strings.HasPrefix(rest, prefix) {
			return nil, fmt.Errorf("record at offset %d: missing %q prefix", i, prefix)
		}
		i += len(prefix)

		nameStart := i
		for i < n && (data[i] == '_' || data[i] >= 'a' && data[i] <= 'z' || data[i] >= 'A' && data[i] <= 'Z' || data[i] >= '0' && data[i] <= '9') {
			i++
		}
		name := data[nameStart:i]
		if name == "" || i >= n || data[i] != '=' || i+1 >= n || data[i+1] != '\'' {
			return nil, fmt.Errorf("record at offset %d: malformed name or missing opening quote", nameStart)
		}
		i += 2 // consume ='

		var value strings.Builder
		terminated := false
	parseValue:
		for i < n {
			switch data[i] {
			case '\'':
				// Either the escaped form '\'' (encoder: ' → '\'')
				// or the record's closing quote.
				if strings.HasPrefix(data[i:], `'\''`) {
					value.WriteByte('\'')
					i += 4
				} else {
					i++ // consume closing quote
					terminated = true
					break parseValue
				}
			default:
				value.WriteByte(data[i])
				i++
			}
		}
		if !terminated {
			return nil, fmt.Errorf("record %q: unterminated value", name)
		}
		if !strings.HasPrefix(data[i:], "\n") {
			return nil, fmt.Errorf("record %q: missing record terminator", name)
		}
		i++ // consume \n

		order = append(order, nameValue{name: name, value: value.String()})
	}
	return order, nil
}

// pushInitialSpawnEnv is SUPERSEDED by the spawn-time pull (US-70.1,
// design 0057 R2) and has no production caller: under native-sidecar
// startup gating the push dialed a control socket in a container that
// could not have started yet — structurally always "connection refused"
// (2026-08-30 fleet audit: 3/6 pods, suspend/resume re-breaks). Deletion
// lands with the US-70.5 demolition; no new code may call it.
//
// pushInitialSpawnEnv hands the boot-time secrets delta to the
// supervisor. Failures are logged and swallowed: an empty or unreadable
// file means no env-secrets — the pod still functions (same safe
// degradation as buildEnvFrom's missing file).
func pushInitialSpawnEnv(cc *controlClient, path string) error {
	delta, err := parseSecretsEnvDelta(path)
	if err != nil {
		return err
	}
	if len(delta) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return cc.SpawnEnv(ctx, delta)
}

// socketReloadProc is the reload path's restartableProcess in sidecar
// mode. US-70.1: the pre-restart secrets-env PUSH is gone — the
// supervisor pulls the fresh delta from the user mux at the moment the
// restarted child spawns (bounded wait + last-good cache), so the
// push-then-restart ordering and its dead-socket edge cases dissolve.
// The restart itself keeps the closed reason enum.
type socketReloadProc struct {
	cc *controlClient
}

func newSocketReloadProc(cc *controlClient) *socketReloadProc {
	return &socketReloadProc{cc: cc}
}

func (s *socketReloadProc) restart() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, _ = s.cc.Restart(ctx, "credential_reload", int(defaultRestartGrace/time.Second))
}

// effectiveDelta returns the subset of delta that parentPlusDelta will
// actually append: keys absent from the parent env block. Platform vars
// shadow delta keys (buildEnvFrom parity), and the shadowed keys drop
// out of the effective revision the supervisor records (design 0057 I4
// — the rev covers what actually landed in the child's env).
func effectiveDelta(parent []string, delta map[string]string) map[string]string {
	present := make(map[string]struct{}, len(parent))
	for _, e := range parent {
		if i := strings.IndexByte(e, '='); i > 0 {
			present[e[:i]] = struct{}{}
		}
	}
	out := make(map[string]string, len(delta))
	for k, v := range delta {
		if _, dup := present[k]; dup {
			continue
		}
		out[k] = v
	}
	return out
}

// parentPlusDelta merges a spawn delta onto a parent env block: parent
// entries win on key conflict (platform vars must not be overridable by
// user secrets — buildEnvFrom parity), delta keys absent from the parent
// are appended.
func parentPlusDelta(parent []string, delta map[string]string) []string {
	effective := effectiveDelta(parent, delta)
	out := make([]string, 0, len(parent)+len(effective))
	out = append(out, parent...)
	for k, v := range effective {
		out = append(out, k+"="+v)
	}
	return out
}
