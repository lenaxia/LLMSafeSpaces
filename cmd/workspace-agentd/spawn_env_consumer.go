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
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// secretsEnvPathFromEnv resolves the secrets-env location (the same
// override the materializer and reload handler use — single coordinate,
// so US-4b's relocation is a controller env change, not new code paths).
func secretsEnvPathFromEnv() string {
	return envOrDefault("LLMSAFESPACES_SECRETS_ENV_PATH", agentd.SecretsEnvPath)
}

// parseSecretsEnvDelta returns the variables the bash-sourceable env
// file introduces, excluding anything already present in THIS process's
// environment and excluding shell noise (SHLVL/PWD/OLDPWD/_). Parsing
// reuses the bash-source + env -0 machinery from buildEnvFrom — a pure
// Go re-implementation would have to mirror bash quoting rules exactly,
// which is the class of bug that produced G2.
func parseSecretsEnvDelta(path string) (map[string]string, error) {
	if _, err := os.Stat(path); err != nil {
		return map[string]string{}, nil // absent = no env-secrets: normal
	}
	parent := os.Environ()
	parentSet := make(map[string]struct{}, len(parent))
	for _, e := range parent {
		if i := strings.IndexByte(e, '='); i > 0 {
			parentSet[e[:i]] = struct{}{}
		}
	}
	const noise = "SHLVL\x00PWD\x00OLDPWD\x00_\x00"
	//nolint:gosec,noctx // G204: constant script body, path bound to $1; boot/reload-time call — same shape as buildEnvFrom
	out, err := trackedOutput(exec.Command("bash", "-c",
		`set -a; source "$1"; env -0`,
		"_", path,
	))
	if err != nil {
		return nil, fmt.Errorf("parse secrets-env %s: %w", path, err)
	}
	delta := map[string]string{}
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		i := strings.IndexByte(rec, '=')
		if i <= 0 {
			continue
		}
		key, val := rec[:i], rec[i+1:]
		if _, inParent := parentSet[key]; inParent {
			continue
		}
		if strings.Contains(noise, key+"\x00") {
			continue
		}
		delta[key] = val
	}
	return delta, nil
}

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
// mode: push the fresh secrets delta, then request the restart with the
// closed reason enum. Re-reads the file AT RESTART TIME so deferred
// (session-aware) restarts hand off the latest materialization.
type socketReloadProc struct {
	cc             *controlClient
	secretsEnvPath string
}

func newSocketReloadProc(cc *controlClient, secretsEnvPath string) *socketReloadProc {
	return &socketReloadProc{cc: cc, secretsEnvPath: secretsEnvPath}
}

func (s *socketReloadProc) restart() {
	if delta, err := parseSecretsEnvDelta(s.secretsEnvPath); err != nil {
		log.Warn("sidecar reload: secrets-env re-read failed; restarting without a new env handoff",
			zap.String("path", s.secretsEnvPath), zap.Error(err))
	} else if len(delta) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.cc.SpawnEnv(ctx, delta); err != nil {
			log.Warn("sidecar reload: spawn_env push failed; restart proceeds with the previous env",
				zap.Error(err))
		}
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, _ = s.cc.Restart(ctx, "credential_reload", int(defaultRestartGrace/time.Second))
}

// parentPlusDelta merges a spawn delta onto a parent env block: parent
// entries win on key conflict (platform vars must not be overridable by
// user secrets — buildEnvFrom parity), delta keys absent from the parent
// are appended.
func parentPlusDelta(parent []string, delta map[string]string) []string {
	present := make(map[string]struct{}, len(parent))
	for _, e := range parent {
		if i := strings.IndexByte(e, '='); i > 0 {
			present[e[:i]] = struct{}{}
		}
	}
	out := make([]string, 0, len(parent)+len(delta))
	out = append(out, parent...)
	for k, v := range delta {
		if _, dup := present[k]; dup {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}
