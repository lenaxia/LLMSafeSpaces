// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// test_sh_hygiene_test.go — pins for local/test.sh's disposable-workspace
// hygiene (nightly 35437562027 adjudication): the API-created disposable
// sandbox from Test 8a leaked through the rest of the job (census pod
// 77cf2232-…, Running 6m28s at the us-70 AC-1c failure) because the
// create-response extraction read the DISPLAY name ("disposable-e2e")
// instead of the CR id (uuid.New() per the API create contract), so the
// DELETE targeted a workspace that did not exist and the warn-only path
// left the CR — and its pod's node CPU requests — standing. On the
// 1-node kind runner those leaked requests are downstream-suite margin.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const testShScript = "test.sh"

func TestTestSh_BashSyntax(t *testing.T) {
	bash := requireBash(t)
	out, err := exec.Command(bash, "-n", testShScript).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

// TestTestSh_DisposableExtractionPrefersCRID executes the script's REAL
// create-response extraction against a fixture shaped like
// types.WorkspaceResponse (id = CR name UUID, name = display name): it
// must yield the id, never the display name.
func TestTestSh_DisposableExtractionPrefersCRID(t *testing.T) {
	bash := requireBash(t)
	src := mustRead(t, testShScript)
	extract := regexp.MustCompile(`(?s)DISPOSABLE_SB=\$\(python3 -c "\n(.*?)"\)`).FindStringSubmatch(src)
	if extract == nil {
		t.Fatal("disposable-workspace extraction not found in test.sh — did Test 8a change shape?")
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "create-sb.json")
	if err := os.WriteFile(fixture, []byte(`{"id":"77cf2232-2763-452c-ae38-bc7152550e30","name":"disposable-e2e"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	prog := strings.ReplaceAll(extract[1], "/tmp/llmsafespaces-create-sb.json", fixture)
	out, err := exec.Command(bash, "-c", "python3 -c \""+prog+"\"").CombinedOutput()
	if err != nil {
		t.Fatalf("execute extraction: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "77cf2232-2763-452c-ae38-bc7152550e30" {
		t.Fatalf("extraction must yield the CR id (uuid.New() create contract), got %q — a display-name DELETE misses the workspace and the pod leaks (nightly 35437562027 census)", got)
	}
}

// TestTestSh_DisposableKubectlBackstop pins the belt-and-braces cleanup:
// whatever the API DELETE returned, a kubectl delete of the disposable
// workspace must follow, mirroring Test 13's own hygiene pattern.
func TestTestSh_DisposableKubectlBackstop(t *testing.T) {
	src := mustRead(t, testShScript)
	const backstop = `kc -n "${NS}" delete workspace "${DISPOSABLE_SB}" --ignore-not-found >/dev/null 2>&1 || true`
	if !strings.Contains(src, backstop) {
		t.Fatalf("test.sh must carry the kubectl backstop delete after the API DELETE:\n  %s\n— the API delete can warn-and-leak (nightly 35437562027)", backstop)
	}
	deleteAt := strings.Index(src, `log "  DELETE /api/v1/workspaces/${DISPOSABLE_SB}"`)
	backstopAt := strings.Index(src, backstop)
	if deleteAt < 0 || backstopAt < deleteAt {
		t.Fatal("the backstop delete must come after the API DELETE attempt")
	}
}
