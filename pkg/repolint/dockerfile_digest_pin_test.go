// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDockerfiles_BaseImagesDigestPinned is the enforcement test for
// #1330: every Dockerfile FROM that references a registry image MUST
// carry @sha256:<digest>. Unpinned tags make CI hostage to Docker Hub
// outages (a moved tag = silently different base) and contradicted the
// supply-chain posture README-LLM.md claims for the runtime images.
//
// This test walks every Dockerfile* in the repo (same walk as
// TestDockerfiles_NoTargetArchDefault in dockerfile_arch_test.go) and
// fails on any FROM whose base reference lacks a digest.
//
// Exemptions (the only ones):
//   - `FROM scratch` — the empty image, no registry round-trip.
//   - A base that names an earlier stage in the same file
//     (`FROM builder AS ...`) — stage-local alias, not a registry ref.
//
// Pins must be the manifest-LIST digest (OCI index / docker manifest
// list), not a single-arch manifest digest: CI builds amd64+arm64 via
// buildx, and a per-arch digest only resolves on one platform.
//
// Renovate keeps the pins fresh (`docker:pinDigests` preset groups
// digest bumps into the "docker base images" PRs); this test is the
// ratchet that guarantees no NEW unpinned FROM lands in between.
func TestDockerfiles_BaseImagesDigestPinned(t *testing.T) {
	root := repoRoot(t)

	var dockerfiles []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
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

	// A well-formed pin: tag kept for readability, then @sha256: + exactly
	// 64 lowercase hex chars, at the end of the base reference.
	digestRe := regexp.MustCompile(`^@sha256:[0-9a-f]{64}$`)

	for _, df := range dockerfiles {
		rel, _ := filepath.Rel(root, df)
		content, err := os.ReadFile(df)
		if err != nil {
			t.Fatalf("%s: read: %v", rel, err)
		}

		lines := strings.Split(string(content), "\n")
		// Pass 1: collect stage names (`FROM ... AS name`) so pass 2 can
		// exempt stage-local references.
		stages := map[string]bool{}
		for _, line := range lines {
			base, rest, ok := fromBase(line)
			if !ok {
				continue
			}
			_ = base
			if alias, has := stageAlias(rest); has {
				stages[strings.ToLower(alias)] = true
			}
		}

		// Pass 2: every registry FROM must carry a digest.
		for lineNum, line := range lines {
			base, _, ok := fromBase(line)
			if !ok {
				continue
			}
			if strings.EqualFold(base, "scratch") {
				continue
			}
			if stages[strings.ToLower(base)] {
				continue
			}
			ref, digest, found := strings.Cut(base, "@")
			if !found || ref == "" || !digestRe.MatchString("@"+digest) {
				t.Errorf("%s:%d: base image %q is not digest-pinned (#1330) — pin as <tag>@sha256:<manifest-list digest>; see README-LLM.md supply-chain posture",
					rel, lineNum+1, base)
			}
		}
	}
}

// fromBase parses a Dockerfile FROM line and returns the base image
// reference plus the remainder of the line (for `AS name` extraction).
// Flag tokens (--platform=...) are skipped. ok=false for non-FROM lines
// and comments.
func fromBase(line string) (base, rest string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "FROM") {
		return "", "", false
	}
	fields = fields[1:]
	for len(fields) > 0 && strings.HasPrefix(fields[0], "--") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return "", "", false
	}
	return fields[0], strings.Join(fields[1:], " "), true
}

// stageAlias extracts the stage name from the tokens after the base
// reference (`AS builder`). Docker treats stage names as
// case-insensitive; callers lowercase before comparing.
func stageAlias(rest string) (name string, ok bool) {
	fields := strings.Fields(rest)
	for i, f := range fields {
		if strings.EqualFold(f, "AS") && i+1 < len(fields) {
			return fields[i+1], true
		}
	}
	return "", false
}
