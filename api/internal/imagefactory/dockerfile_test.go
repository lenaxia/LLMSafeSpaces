// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"strings"
	"testing"
)

func sampleBase() Base {
	return Base{Name: "bookworm", Version: "0.20.1", Image: "ghcr.io/acme/base", Tag: "0.20.1"}
}

func sampleResolvedApt() ResolvedValues {
	return ResolvedValues{
		"ffmpeg":  {Type: ExtensionTypeApt, Value: "ffmpeg"},
		"buildah": {Type: ExtensionTypeApt, Value: "buildah"},
	}
}

func sampleResolvedMise() ResolvedValues {
	return ResolvedValues{
		"python313": {Type: ExtensionTypeMise, Value: "python@3.13"},
		"bun":       {Type: ExtensionTypeMise, Value: "bun@1.1.0"},
	}
}

func sampleResolvedFile() ResolvedValues {
	return ResolvedValues{
		"motd": {Type: ExtensionTypeFile, Value: "welcome\n", FileSpec: &FileSpec{Path: "/etc/motd"}},
		"binx": {Type: ExtensionTypeFile, Value: "#!/bin/sh\n", FileSpec: &FileSpec{Path: "/usr/local/bin/x", Mode: "0755"}},
	}
}

func TestRenderDockerfile_Happy_Empty(t *testing.T) {
	t.Parallel()
	out, err := RenderDockerfile(ResolvedValues{}, sampleBase())
	if err != nil {
		t.Fatalf("RenderDockerfile: %v", err)
	}
	mustContain(t, out, "FROM ghcr.io/acme/base:0.20.1")
	mustContain(t, out, "USER sandbox")
	mustContain(t, out, "WORKDIR /workspace")
	mustEndWith(t, out, `ENTRYPOINT ["/usr/local/bin/entrypoint-opencode.sh"]`)
	// No apt/mise/file blocks when none provided.
	if strings.Contains(out, "apt-get") {
		t.Errorf("empty resolved must not emit apt block:\n%s", out)
	}
}

func TestRenderDockerfile_DigestPinnedBase(t *testing.T) {
	t.Parallel()
	b := Base{Name: "bookworm", Version: "0.20.1", Image: "img", Digest: "sha256:deadbeef"}
	out, err := RenderDockerfile(ResolvedValues{}, b)
	if err != nil {
		t.Fatalf("RenderDockerfile: %v", err)
	}
	mustContain(t, out, "FROM img@sha256:deadbeef")
}

func TestRenderDockerfile_Apt(t *testing.T) {
	t.Parallel()
	out, err := RenderDockerfile(sampleResolvedApt(), sampleBase())
	if err != nil {
		t.Fatalf("RenderDockerfile: %v", err)
	}
	mustContain(t, out, "apt-get install")
	mustContain(t, out, "ffmpeg")
	mustContain(t, out, "buildah")
}

// TestRenderDockerfile_AptMultiPkgStructural catches the && vs \ separator
// bug that a substring check cannot: every package must be a continuation
// line (ends with \), never an && operand. Without this, 2+ apt packages
// produce a Dockerfile where only the first is installed and the rest are
// executed as commands.
func TestRenderDockerfile_AptMultiPkgStructural(t *testing.T) {
	t.Parallel()
	out, err := RenderDockerfile(sampleResolvedApt(), sampleBase())
	if err != nil {
		t.Fatalf("RenderDockerfile: %v", err)
	}
	lines := strings.Split(out, "\n")
	// Find the apt block.
	var aptLines []string
	inApt := false
	for _, line := range lines {
		if strings.Contains(line, "apt-get install") {
			inApt = true
		}
		if inApt {
			aptLines = append(aptLines, line)
			if strings.Contains(line, "rm -rf /var/lib/apt/lists") {
				break
			}
		}
	}
	if len(aptLines) < 4 {
		t.Fatalf("apt block too short, expected install + 2 pkgs + cleanup:\n%s", out)
	}
	// Every package line must end with " \" (backslash continuation), never "&&".
	for i, line := range aptLines {
		if strings.Contains(line, "apt-get install") || strings.Contains(line, "rm -rf") {
			continue
		}
		trimmed := strings.TrimRight(line, " \t")
		if !strings.HasSuffix(trimmed, `\`) {
			t.Errorf("apt package line %d must end with backslash continuation (not &&):\n  %q", i, line)
		}
		if strings.Contains(line, " && ") {
			t.Errorf("apt package line %d must NOT contain && (packages are arguments, not commands):\n  %q", i, line)
		}
	}
}

func TestRenderDockerfile_Mise(t *testing.T) {
	t.Parallel()
	out, err := RenderDockerfile(sampleResolvedMise(), sampleBase())
	if err != nil {
		t.Fatalf("RenderDockerfile: %v", err)
	}
	mustContain(t, out, "mise install --system")
	mustContain(t, out, "python@3.13")
	mustContain(t, out, "bun@1.1.0")
	mustContain(t, out, "mise reshim")
}

func TestRenderDockerfile_File_Base64AndChmod(t *testing.T) {
	t.Parallel()
	out, err := RenderDockerfile(sampleResolvedFile(), sampleBase())
	if err != nil {
		t.Fatalf("RenderDockerfile: %v", err)
	}
	mustContain(t, out, "base64 -d")
	mustContain(t, out, "chmod 0644") // default mode
	mustContain(t, out, "chmod 0755")
}

func TestRenderDockerfile_Deterministic(t *testing.T) {
	t.Parallel()
	rv := ResolvedValues{
		"ffmpeg":    {Type: ExtensionTypeApt, Value: "ffmpeg"},
		"python313": {Type: ExtensionTypeMise, Value: "python@3.13"},
		"motd":      {Type: ExtensionTypeFile, Value: "hi\n", FileSpec: &FileSpec{Path: "/etc/motd", Mode: "0644"}},
	}
	a, err := RenderDockerfile(rv, sampleBase())
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	// Re-render with a fresh map (Go randomizes iteration); same bytes.
	b, err := RenderDockerfile(rv, sampleBase())
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a != b {
		t.Fatalf("render must be deterministic regardless of map ordering\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

func TestRenderDockerfile_RejectsInvalidResolved(t *testing.T) {
	t.Parallel()
	bad := ResolvedValues{"x": {Type: "bogus", Value: "v"}}
	if _, err := RenderDockerfile(bad, sampleBase()); err == nil {
		t.Fatal("expected error for invalid resolved values")
	}
}

func TestRenderDockerfile_UnknownExtensionTypeRejected(t *testing.T) {
	t.Parallel()
	rv := ResolvedValues{"x": {Type: ExtensionType("yaml"), Value: "v"}}
	_, err := RenderDockerfile(rv, sampleBase())
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestRenderDockerfile_OrderStableAcrossReruns(t *testing.T) {
	t.Parallel()
	// 50 iterations to catch map-iteration randomness.
	rv := ResolvedValues{
		"a": {Type: ExtensionTypeApt, Value: "a"},
		"b": {Type: ExtensionTypeApt, Value: "b"},
		"c": {Type: ExtensionTypeMise, Value: "c@1"},
		"d": {Type: ExtensionTypeFile, Value: "d", FileSpec: &FileSpec{Path: "/d", Mode: "0644"}},
		"e": {Type: ExtensionTypeApt, Value: "e"},
	}
	first, _ := RenderDockerfile(rv, sampleBase())
	for i := 0; i < 50; i++ {
		got, _ := RenderDockerfile(rv, sampleBase())
		if got != first {
			t.Fatalf("iteration %d diverged from first render", i)
		}
	}
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("expected output to contain %q\n--- got ---\n%s", sub, s)
	}
}

func mustEndWith(t *testing.T, s, suffix string) {
	t.Helper()
	if !strings.HasSuffix(strings.TrimSpace(s), suffix) {
		t.Fatalf("expected output to end with %q\n--- got ---\n%s", suffix, s)
	}
}
