// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// TestDockerfiles_BaseImagesDigestPinned is the enforcement test for
// #1330: every Dockerfile FROM that references a registry image MUST
// carry tag@sha256:<digest>. Unpinned tags make CI hostage to Docker Hub
// outages (a moved tag = silently different base) and contradicted the
// supply-chain posture README-LLM.md claims for the runtime images.
//
// It also enforces the same digest-pinning bar on `# syntax=...`
// frontend directives: the directive forces a docker/dockerfile frontend
// image fetch from docker.io on every fresh CI runner — the same
// availability dependency the issue was filed for. The repo's
// Dockerfiles use no post-builtin-frontend features (grep-verified: no
// heredocs, RUN --mount, COPY --parents, ADD --checksum), so the
// directives were REMOVED (r2, reviewer finding 1; same disposition as
// the prior attempt documented in the issue thread, verified by full
// builds). If a directive is ever re-introduced for a new frontend
// feature, it must be digest-pinned like any other base.
//
// Exemptions (the only ones):
//   - `FROM scratch` — the empty image, no registry round-trip.
//   - A base that names an earlier stage in the same file
//     (`FROM builder AS ...`) — stage-local alias, not a registry ref.
//
// Pins must be the manifest-LIST digest (OCI index / docker manifest
// list), not a single-arch manifest digest: CI builds amd64+arm64 via
// buildx, and a per-arch digest only resolves on one platform. The tag
// is kept in front of @sha256: for readability (and is enforced: a
// digest without a tag is a finding, matching renovate's
// name:tag@sha256 output shape).
//
// Renovate keeps the pins fresh (`docker:pinDigests` preset groups
// digest bumps into the "docker base images" PRs); this test is the
// ratchet that guarantees no NEW unpinned FROM or syntax directive
// lands in between.
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
		if base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") || strings.HasSuffix(base, ".Dockerfile") {
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

	for _, df := range dockerfiles {
		rel, _ := filepath.Rel(root, df)
		content, err := os.ReadFile(df)
		if err != nil {
			t.Fatalf("%s: read: %v", rel, err)
		}
		for _, finding := range lintDockerfileContent(string(content)) {
			t.Errorf("%s:%s (#1330) — pin as <tag>@sha256:<manifest-list digest>, or drop the registry dependency; see README-LLM.md supply-chain posture",
				rel, finding)
		}
	}
}

// A well-formed pin: tag kept for readability, then @sha256: + exactly
// 64 lowercase hex chars, at the end of the base reference.
var digestSuffixRe = regexp.MustCompile(`^@sha256:[0-9a-f]{64}$`)

// lintDockerfileContent runs the #1330 pins over one Dockerfile and
// returns one finding per violation, prefixed with the 1-based line
// number ("13: ..."). It is pure so the table-driven unit tests can
// exercise the FULL path — including the stage-alias exemption, which
// no Dockerfile in the live tree currently uses (r1 review finding 2).
func lintDockerfileContent(content string) []string {
	var findings []string
	lines := strings.Split(content, "\n")

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

	for lineNum, line := range lines {
		trimmed := strings.TrimSpace(line)
		// `# syntax=<image ref>` — a registry fetch, same pin bar as FROM.
		// Matched per BuildKit's directive grammar (r2 review finding 1):
		// '#' prefix, any leading whitespace (space/tab), lowercase name,
		// optional padding around '=' — so `#syntax=…` and `#\tsyntax=…`
		// cannot bypass the bar. A non-lowercase `#SYNTAX=` is NOT a
		// build-honored directive (BuildKit matches lowercase) and is
		// treated as a plain comment.
		if ref, isDirective := buildkitSyntaxDirective(trimmed); isDirective {
			if !pinnedImageRef(ref) {
				findings = append(findings, fmt.Sprintf("%d: syntax directive %q is not digest-pinned", lineNum+1, ref))
			}
			continue
		}

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
		if !pinnedImageRef(base) {
			findings = append(findings, fmt.Sprintf("%d: base image %q is not digest-pinned", lineNum+1, base))
		}
	}
	return findings
}

// pinnedImageRef reports whether ref is a well-formed <tag>@sha256:<64
// lowercase hex> image reference: a digest present, valid, preceded by
// a non-empty reference whose LAST PATH SEGMENT carries a tag (a
// registry port such as registry:5000/img is not a tag; digest-only
// refs without a tag lose the readability the pins exist for — matches
// renovate's name:tag@sha256 output shape).
func pinnedImageRef(ref string) bool {
	name, digest, found := strings.Cut(ref, "@")
	if !found || name == "" {
		return false
	}
	if last := name[strings.LastIndex(name, "/")+1:]; !strings.Contains(last, ":") {
		return false
	}
	return digestSuffixRe.MatchString("@" + digest)
}

// buildkitDirectiveRe mirrors BuildKit's directive grammar
// (frontend/dockerfile/parser/directives.go): after '#' and any leading
// whitespace, `name\s*=\s*value` with a lowercase-alpha name.
var buildkitDirectiveRe = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)

// buildkitSyntaxDirective reports whether line is a build-honored
// `syntax` directive and returns its image reference.
func buildkitSyntaxDirective(line string) (ref string, ok bool) {
	if !strings.HasPrefix(line, "#") {
		return "", false
	}
	rest := strings.TrimLeftFunc(line[1:], unicode.IsSpace)
	m := buildkitDirectiveRe.FindStringSubmatch(rest)
	if m == nil || m[1] != "syntax" {
		return "", false
	}
	return m[2], true
}

// fromBase parses a Dockerfile FROM line and returns the base image
// reference plus the remainder of the line (for `AS name` extraction).
// Flag tokens (--platform=...) are skipped. ok=false for non-FROM lines
// and comments. Instruction keywords are case-insensitive, as in
// Dockerfile proper.
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

// TestLintDockerfileContent is the table-driven pin for the parser and
// the lint pass over synthetic Dockerfiles (r1 review finding 2: the
// helpers and the exemption paths had no unit coverage; the stage-alias
// exemption is not exercised by any live Dockerfile).
func TestLintDockerfileContent(t *testing.T) {
	const d64 = "3c3e25a4da13fd0478eed2df1eb35a0e667094a7124d3993a6a1d30f71c17e79" // valid 64 lowercase hex
	const d63 = "3c3e25a4da13fd0478eed2df1eb35a0e667094a7124d3993a6a1d30f71c17e7"  // truncated
	const dUp = "3C3E25A4DA13FD0478EED2DF1EB35A0E667094A7124D3993A6A1D30F71C17E79" // uppercase hex

	cases := []struct {
		name    string
		content string
		want    int // number of findings
	}{
		{
			name:    "tag+digest builder and distroless final is clean",
			content: "FROM --platform=$BUILDPLATFORM golang:1.26@sha256:" + d64 + " AS builder\nRUN go build\nFROM gcr.io/distroless/static:nonroot@sha256:" + d64 + "\n",
			want:    0,
		},
		{
			name:    "bare tag is a finding",
			content: "FROM golang:1.26\n",
			want:    1,
		},
		{
			name:    "namespaced bare tag is a finding",
			content: "FROM nginxinc/nginx-unprivileged:1.27-alpine\n",
			want:    1,
		},
		{
			name:    "scratch is exempt (mixed case too)",
			content: "FROM golang:1.26@sha256:" + d64 + " AS b\nFROM scratch\nFROM SCRATCH\n",
			want:    0,
		},
		{
			name:    "stage-local alias reference is exempt, unknown alias is a finding",
			content: "FROM golang:1.26@sha256:" + d64 + " AS builder\nFROM builder AS delivery\nFROM notastage\n",
			want:    1,
		},
		{
			name:    "stage alias exemption is case-insensitive",
			content: "FROM golang:1.26@sha256:" + d64 + " AS Builder\nFROM builder\n",
			want:    0,
		},
		{
			name:    "lowercase from instruction still parses",
			content: "from golang:1.26\n",
			want:    1,
		},
		{
			name:    "flag tokens (--platform) are skipped",
			content: "FROM --platform=$BUILDPLATFORM golang:1.26@sha256:" + d64 + " AS builder\n",
			want:    0,
		},
		{
			name:    "truncated digest is a finding",
			content: "FROM golang:1.26@sha256:" + d63 + "\n",
			want:    1,
		},
		{
			name:    "uppercase-hex digest is a finding",
			content: "FROM golang:1.26@sha256:" + dUp + "\n",
			want:    1,
		},
		{
			name:    "wrong digest algorithm is a finding",
			content: "FROM golang:1.26@sha512:" + d64 + "\n",
			want:    1,
		},
		{
			name:    "digest without a tag is a finding (keep the tag)",
			content: "FROM golang@sha256:" + d64 + "\n",
			want:    1,
		},
		{
			name:    "bare digest ref (@ with empty name) is a finding",
			content: "FROM @sha256:" + d64 + "\n",
			want:    1,
		},
		{
			name:    "build-arg base reference is a finding (pin or exempt deliberately)",
			content: "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n",
			want:    1,
		},
		{
			name:    "comments, blanks, and other instructions are ignored",
			content: "# FROM golang:1.26 — commented out, not a finding\n\nRUN echo hi\n# syntax comment that is not a directive\n",
			want:    0,
		},
		{
			name:    "unpinned syntax directive is a finding",
			content: "# syntax=docker/dockerfile:1.7\nFROM golang:1.26@sha256:" + d64 + "\n",
			want:    1,
		},
		{
			name:    "digest-pinned syntax directive is clean",
			content: "# syntax=docker/dockerfile:1.7@sha256:" + d64 + "\nFROM golang:1.26@sha256:" + d64 + "\n",
			want:    0,
		},
		{
			name:    "syntax directive with truncated digest is a finding",
			content: "# syntax=docker/dockerfile:1.7@sha256:" + d63 + "\nFROM golang:1.26@sha256:" + d64 + "\n",
			want:    1,
		},
		{
			// r2 review finding 1: BuildKit's directive parser (CutPrefix "#",
			// TrimLeftFunc IsSpace, `^([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)
			// honors unspaced, tabbed, and `=`-padded spellings — each is a live
			// docker.io frontend fetch and must not bypass the pin bar.
			name:    "unspaced #syntax= directive is a finding",
			content: "#syntax=docker/dockerfile:1.8\nFROM golang:1.26@sha256:" + d64 + "\n",
			want:    1,
		},
		{
			name:    "tab-prefixed directive is a finding",
			content: "#\tsyntax=docker/dockerfile:1.7\nFROM golang:1.26@sha256:" + d64 + "\n",
			want:    1,
		},
		{
			name:    "directive padded around = is a finding (BuildKit grammar allows spaces)",
			content: "# syntax = docker/dockerfile:1.7\nFROM golang:1.26@sha256:" + d64 + "\n",
			want:    1,
		},
		{
			name:    "unspaced digest-pinned #syntax= directive is clean",
			content: "#syntax=docker/dockerfile:1.8@sha256:" + d64 + "\nFROM golang:1.26@sha256:" + d64 + "\n",
			want:    0,
		},
		{
			name:    "#SYNTAX= is NOT a build-honored directive (BuildKit matches lowercase) — plain comment",
			content: "#SYNTAX=docker/dockerfile:1.7\nFROM golang:1.26@sha256:" + d64 + "\n",
			want:    0,
		},
		{
			// r2 review minor 1: a registry port is not a tag; keep-the-tag
			// policy must look at the last path segment only.
			name:    "registry port without a tag is a finding (port is not a tag)",
			content: "FROM registry:5000/img@sha256:" + d64 + "\n",
			want:    1,
		},
		{
			name:    "registry port WITH a tag is clean",
			content: "FROM registry:5000/img:1.2.3@sha256:" + d64 + "\n",
			want:    0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lintDockerfileContent(tc.content)
			if len(got) != tc.want {
				t.Fatalf("lintDockerfileContent findings = %d (%v), want %d", len(got), got, tc.want)
			}
		})
	}
}
