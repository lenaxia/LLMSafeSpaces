// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package version is the single source of truth for the build version string.
// The variables here are overridden at build time via -ldflags:
//
//	go build \
//		-ldflags "-X github.com/lenaxia/llmsafespaces/pkg/version.Version=v0.5.1
//		          -X github.com/lenaxia/llmsafespaces/pkg/version.CommitSHA=<sha>
//		          -X github.com/lenaxia/llmsafespaces/pkg/version.BuildTime=<ts>"
//
// When unset (e.g. an un-stamped local build), all three report "unknown" so
// a missing injection is obvious. The release workflow sets them from the git
// tag, sha, and build time.
//
// Each component that exposes a version surface (API /livez,
// /v1/admin/platform-info; workspace-agentd /v1/healthz; the controller
// startup log; the relay healthz endpoints) reads from here so we never have
// two competing version strings.
package version

import (
	"fmt"
	"runtime/debug"
	"sync"
)

// Version is the build version. Overridden at build time. See package doc.
var Version = "unknown"

// CommitSHA is the git commit the binary was built from. Overridden at
// build time. See package doc.
var CommitSHA = "unknown"

// BuildTime is the UTC build timestamp. Overridden at build time. See
// package doc.
var BuildTime = "unknown"

// resolveOnce guards the fallback resolution: ldflags vars can be set for
// some fields and not others, and ReadBuildInfo is not free, so resolution
// runs at most once, lazily, from the first caller that needs a resolved
// value (String / Commit / BuildTimeString).
var resolveOnce sync.Once

// resolve fills in "unknown" fields from Go's embedded build info:
//
//   - CommitSHA from vcs.revision (present when the binary was built from a
//     git checkout with -buildvcs on its default setting). The CI Docker
//     build copies only go.mod/cmd/pkg into the build stage — no .git — so
//     in CI the -X injection is the only commit source and this fallback
//     matters for local `go build` runs.
//   - Version from the module version when built as a dependency
//     (bi.Main.Version like "v0.15.7"). "(devel)" is the norm for
//     in-repo builds and is ignored.
func resolve() {
	if CommitSHA != "unknown" && Version != "unknown" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if CommitSHA == "unknown" {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				sha := s.Value
				if len(sha) > 8 {
					sha = sha[:8]
				}
				CommitSHA = sha
				break
			}
		}
	}
	if Version == "unknown" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = bi.Main.Version
	}
}

// Commit returns the resolved commit SHA ("unknown" when it cannot be
// determined from either ldflags or build info — which is itself a
// build-pipeline bug worth investigating).
func Commit() string {
	resolveOnce.Do(resolve)
	return CommitSHA
}

// String returns the full build identity, e.g. "v0.15.7+gd93e52ea".
//
// Components that remain unknown after fallback resolution are OMITTED, not
// rendered as "unknown": a bare "v0.15.7" from String() therefore means "the
// commit could not be determined", making un-stamped builds visibly
// deficient instead of silently masquerading as a clean release. (Incident
// 2026-08-15: a devel hash build reported "0.15.7" via /v1/healthz and the
// commit never surfaced anywhere, making deployed-code identification require
// binary disassembly.)
func String() string {
	resolveOnce.Do(resolve)
	s := Version
	if sha := CommitSHA; sha != "unknown" && sha != "" {
		s += "+g" + sha
	}
	return s
}

// Full returns the one-line human identity including build time, e.g.
// "v0.15.7+gd93e52ea (built 2026-08-15T20:43:36Z)". Used where a single log
// line should carry the complete build identity (controller startup banner).
func Full() string {
	resolveOnce.Do(resolve)
	if BuildTime == "unknown" || BuildTime == "" {
		return String()
	}
	return fmt.Sprintf("%s (built %s)", String(), BuildTime)
}
