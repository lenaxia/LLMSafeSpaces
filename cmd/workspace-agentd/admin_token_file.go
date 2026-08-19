// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Admin-mux bearer token resolution (#887 D5.1).
//
// The token historically arrived as pod-spec env (AGENTD_ADMIN_TOKEN),
// which opencode inherits and passes — verbatim — to every tool process
// it spawns (extendEnv). Any bash tool could printenv the bearer for the
// admin mux (:4098) and authenticate. The file delivery mode keeps the
// token out of the environment entirely:
//
//	AGENTD_ADMIN_TOKEN_FILE  → read + trim the file (preferred)
//	AGENTD_ADMIN_TOKEN       → env fallback (legacy pods, local dev)
//
// Both variables are additionally scrubbed from the opencode spawn env
// (buildEnvFrom) so neither the value nor its path rides the inheritance
// chain, regardless of which mode delivered it.

import (
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"
)

// adminTokenFileEnv / adminTokenEnv name the delivery variables.
const (
	adminTokenFileEnv = "AGENTD_ADMIN_TOKEN_FILE"
	adminTokenEnv     = "AGENTD_ADMIN_TOKEN"
)

// adminOnlyEnvKeys are removed from the environment agentd hands to the
// opencode child (and thereby to every tool process). agentd-only
// credentials have no business in workspace-visible env.
var adminOnlyEnvKeys = []string{adminTokenEnv, adminTokenFileEnv}

// adminToken resolves the admin-mux bearer token: file first, then env
// (legacy), then empty (dev mode — gate disabled; design 0051 D5.2 makes
// that a boot failure separately).
func adminToken() string {
	if path := os.Getenv(adminTokenFileEnv); path != "" {
		tok, err := readAdminTokenFile(path)
		if err == nil {
			return tok
		}
		// Unreadable file with the path explicitly set is a wiring bug —
		// fall through to env rather than silently disabling the gate,
		// and LOG the degradation (fail-closed boot per D5.2 catches the
		// no-token case; this covers env-fallback).
		log.Warn("admin token file unreadable — falling back to env delivery (#887 D5.1)",
			zap.String("path", path), zap.Error(err))
		if envTok := os.Getenv(adminTokenEnv); envTok != "" {
			return envTok
		}
		return ""
	}
	return os.Getenv(adminTokenEnv)
}

// readAdminTokenFile reads and trims the token file. Mode is not enforced
// here — the controller installs it 0400; enforcing what we can't set
// would break local-dev runs on shared test files.
func readAdminTokenFile(path string) (string, error) {
	//nolint:gosec // G304: the path comes from the operator-controlled
	// AGENTD_ADMIN_TOKEN_FILE env (pod spec), never from request input.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// resolveAdminTokenForBoot enforces design 0051 D5.2 — agentd refuses to
// start without an admin-mux credential (historically an unset
// AGENTD_ADMIN_TOKEN silently DISABLED the bearer gate on /v1/readyz and
// /v1/statusz: a wiring gap that failed open) — and returns the token
// resolved ONCE for the process lifetime. Every consumer (mux wrap,
// health probe) reads the boot-resolved value, so no file/env re-read can
// diverge from what the gate verified (TOCTOU closed, #934 review).
// Escape: explicit AGENTD_ALLOW_NO_ADMIN_TOKEN=1 (dev/kind) → "" no error.
func resolveAdminTokenForBoot() (string, error) {
	tok := adminToken()
	if tok != "" {
		return tok, nil
	}
	if os.Getenv("AGENTD_ALLOW_NO_ADMIN_TOKEN") == "1" {
		return "", nil
	}
	return "", fmt.Errorf("no admin token: set AGENTD_ADMIN_TOKEN_FILE (file delivery, #887 D5.1) or AGENTD_ADMIN_TOKEN (legacy env); set AGENTD_ALLOW_NO_ADMIN_TOKEN=1 only for dev clusters")
}

// scrubAdminEnv removes agentd-only credential entries from an env slice
// (used on the merged parent+secrets env before spawning opencode).
func scrubAdminEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		drop := false
		for _, banned := range adminOnlyEnvKeys {
			if key == banned {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}
