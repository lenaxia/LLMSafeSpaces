// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerfiles_NoTargetArchDefault is the regression test for #462:
// arm64 image variants were advertising arm64 in their manifest but
// containing x86-64 ELF binaries. Root cause: several Go Dockerfiles
// pinned `ARG TARGETARCH=amd64` at the file scope, which can mask the
// buildx-injected per-platform value and cause silent amd64 builds.
//
// This test walks every Dockerfile* in the repo and fails if any has
// the anti-pattern. It runs as part of `go test ./pkg/repolint/...`,
// which is part of `make test` and CI. Adding a new Dockerfile with
// `ARG TARGETARCH=amd64` (or `ARG TARGETOS=linux` etc.) will fail this
// test and the PR will be rejected.
//
// Allowed form: `ARG TARGETARCH` (no default; buildx injects per-platform).
// Allowed form: `ARG TARGETARCH=` (empty default; buildx overrides).
// Disallowed form: `ARG TARGETARCH=amd64` (hardcoded default masks buildx).
// Disallowed form: `ARG TARGETARCH=arm64` (same problem, opposite arch).
//
// Per #462's fix, the standard Go-cross-compile pattern is:
//
//	FROM --platform=$BUILDPLATFORM golang:... AS builder
//	ARG TARGETARCH
//	RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build ...
//
// `--platform=$BUILDPLATFORM` forces the builder to run on the host arch
// (fast, no QEMU); Go cross-compiles via GOARCH=TARGETARCH.
func TestDockerfiles_NoTargetArchDefault(t *testing.T) {
	root := repoRoot(t)

	// Find every Dockerfile* in the repo.
	var dockerfiles []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip vendor/node_modules/etc.
			name := info.Name()
			if name == "node_modules" || name == "vendor" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(path)
		if base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") {
			dockerfiles = append(dockerfiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("filepath.Walk: %v", err)
	}
	if len(dockerfiles) == 0 {
		t.Fatal("no Dockerfiles found — test setup is wrong")
	}

	// Anti-pattern: ARG TARGET<SOMETHING>=<value> at the file scope (not
	// inside a stage). The buildx auto-args are TARGETOS, TARGETARCH,
	// TARGETVARIANT, BUILDOS, BUILDARCH, BUILDVARIANT, BUILDPLATFORM,
	// TARGETPLATFORM. A hardcoded default on any of them masks the
	// buildx-injected value.
	//
	// Matches lines like:
	//   ARG TARGETARCH=amd64
	//   ARG TARGETOS=linux
	// Allowed (no default or empty default):
	//   ARG TARGETARCH
	//   ARG TARGETARCH=
	//   ARG GOPROXY=https://proxy.golang.org  (not a buildx auto-arg)
	for _, df := range dockerfiles {
		rel, _ := filepath.Rel(root, df)
		content, err := os.ReadFile(df)
		if err != nil {
			t.Fatalf("%s: read: %v", rel, err)
		}

		for lineNum, line := range strings.Split(string(content), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "ARG ") {
				continue
			}
			// Strip the "ARG " prefix and any inline comment.
			arg := strings.TrimSpace(trimmed[4:])
			arg = strings.SplitN(arg, "#", 2)[0]
			arg = strings.TrimSpace(arg)

			// Check if this is one of the buildx auto-args.
			for _, auto := range []string{"TARGETPLATFORM", "TARGETOS", "TARGETARCH", "TARGETVARIANT", "BUILDPLATFORM", "BUILDOS", "BUILDARCH", "BUILDVARIANT"} {
				if !strings.HasPrefix(arg, auto) {
					continue
				}
				rest := arg[len(auto):]
				// Allowed: empty (just the name), or "=" followed by empty.
				// Disallowed: "=value" where value is non-empty.
				if rest == "" {
					break // ARG TARGETARCH — fine
				}
				if strings.HasPrefix(rest, "=") {
					value := strings.TrimSpace(rest[1:])
					// Strip quotes if present.
					value = strings.Trim(value, `"'`)
					if value != "" {
						t.Errorf("%s:%d: %s has hardcoded default %q — buildx auto-args MUST NOT have a default value (#462). Use `ARG %s` (no default; buildx injects per-platform).",
							rel, lineNum+1, auto, value, auto)
					}
					break
				}
				// arg starts with auto but isn't `auto`, `auto=`, or `auto=value`.
				// Probably a different variable name that happens to start with the
				// same prefix (e.g. TARGETARCHFOO). Skip.
			}
		}
	}
}

// repoRoot is shared with sequence_test.go (defined there).
