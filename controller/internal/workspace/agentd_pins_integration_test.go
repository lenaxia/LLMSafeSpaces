// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Integration + contract tests for agentd pin resolution.

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

// TestFetchIndexAnnotations_LocalRegistry round-trips the production
// fetcher against a real in-process OCI registry: push an annotated
// index, fetch by digest, read annotations back.
func TestFetchIndexAnnotations_LocalRegistry(t *testing.T) {
	reg := registry.New()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)

	// httptest binds 127.0.0.1, which registry name parsing rejects as a
	// hostname-with-port only when written bare — rewrite to localhost.
	repoName := "localhost" + srv.URL[len("http://127.0.0.1"):] + "/agentd/test"
	amd64 := "a" + repeatChars("a", 63)
	arm64 := "b" + repeatChars("b", 63)
	idx := mutate.Annotations(empty.Index, map[string]string{
		annotationKeyAMD64: amd64,
		annotationKeyARM64: arm64,
		annotationVersion:  "9.9.9-test",
	}).(v1.ImageIndex)
	tag, err := name.NewTag(repoName + ":test")
	require.NoError(t, err)
	require.NoError(t, remote.WriteIndex(tag, idx))

	head, err := remote.Head(tag)
	require.NoError(t, err)
	ref := repoName + "@" + head.Digest.String()

	ann, err := fetchIndexAnnotations(context.Background(), ref)
	require.NoError(t, err)
	require.Equal(t, amd64, ann[annotationKeyAMD64])
	require.Equal(t, arm64, ann[annotationKeyARM64])
	require.Equal(t, "9.9.9-test", ann[annotationVersion])
}

func TestFetchIndexAnnotations_Unreachable(t *testing.T) {
	_, err := fetchIndexAnnotations(context.Background(), "ghcr.invalid/agentd/x@sha256:"+repeatChars("0", 64))
	require.Error(t, err)
}

func TestFetchIndexAnnotations_TagOnlyRefRejected(t *testing.T) {
	_, err := fetchIndexAnnotations(context.Background(), "ghcr.io/lenaxia/llmsafespaces/agentd:dev")
	require.Error(t, err, "annotation resolution requires a digest-pinned reference")
	require.Contains(t, err.Error(), "digest-pinned")
}

// TestAgentdAnnotationKeys_MatchCIWorkflow is the CI↔Go contract guard:
// the annotation keys stamped by .github/workflows/ci.yml's merge-agentd
// job must be byte-identical to the Go constants. A typo on either side
// ships as a fleet-wide controller boot failure — and because
// merge-agentd only runs on non-PR events, it would only be discovered
// on main's first post-merge run.
func TestAgentdAnnotationKeys_MatchCIWorkflow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	require.NoError(t, err)
	body := string(raw)

	ciAMD64 := extractAnnoKey(t, body, `--annotation "index:([^=]+\.sha256-amd64)=\$\{amd64\}"`)
	ciARM64 := extractAnnoKey(t, body, `--annotation "index:([^=]+\.sha256-arm64)=\$\{arm64\}"`)
	ciVersion := extractAnnoKey(t, body, `--annotation "index:([^=]+)=\$\{\{ needs\.prepare\.outputs\.version \}\}"`)

	require.Equal(t, annotationKeyAMD64, ciAMD64,
		"ci.yml merge-agentd amd64 annotation key must match the Go constant exactly")
	require.Equal(t, annotationKeyARM64, ciARM64,
		"ci.yml merge-agentd arm64 annotation key must match the Go constant exactly")
	require.Equal(t, "dev.llmsafespaces/version", ciVersion)
}

func extractAnnoKey(t *testing.T, body, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(body)
	require.NotNil(t, m, "pattern %s not found in ci.yml — merge-agentd annotation stamping changed; update this test AND agentd_pins.go constants together", pattern)
	return m[1]
}

func repeatChars(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
