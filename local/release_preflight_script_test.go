// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// release_preflight_script_test.go — structure + behavior pins for
// local/release-preflight.sh (issue #1237's pre-flight for candidate
// config bumps). Same philosophy as dev_preview_script_test.go: bash
// syntax, the load-bearing assertion rows, and the offline checks are
// deterministic; the registry checks are exercised against an in-process
// stub standing in for the ghcr v2 API.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const preflightScript = "release-preflight.sh"

func preflightRun(t *testing.T, valuesBody string, extraArgs ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.yaml")
	if err := os.WriteFile(path, []byte(valuesBody), 0o644); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"./" + preflightScript, path}, extraArgs...)
	cmd := exec.Command("bash", args...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	// The script is fail-loud by design; a non-zero exit is a RESULT,
	// not a test harness error.
	runErr := cmd.Run()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if code == 0 && runErr != nil {
		code = 1
	}
	return code, out.String()
}

// repoSeedBaseVersion reads the catalog seed default row so fixtures
// stay aligned with the tree across future seed bumps.
func repoSeedBaseVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "api", "internal", "imagefactory", "catalog.seed.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)  - name: \S+.*?version: "([^"]+)"`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("catalog seed default row not found")
	}
	return m[1]
}

func repoPinnedOpencode(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "runtimes", "opencode", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ARG OPENCODE_VERSION=([0-9.]+)$`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("OPENCODE_VERSION pin not found")
	}
	return m[1]
}

// preflightHappyValues builds a candidate whose every coordinate is
// coherent with this repo (seed base tag, repo opencode pin, distinct
// delivery digests).
func preflightHappyValues(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`
api:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/api
    tag: "0.30.1"
    digest: ""
controller:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/controller
    tag: "0.30.1"
    digest: ""
  agentdDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:1111111111111111111111111111111111111111111111111111111111111111"
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
  opencodeDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/opencode:%s@sha256:2222222222222222222222222222222222222222222222222222222222222222"
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
frontend:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/frontend
    tag: "0.30.1"
    digest: ""
runtimeEnvironments:
  base:
    image:
      repository: ghcr.io/lenaxia/llmsafespaces/base
      tag: "%s"
      digest: ""
`, repoPinnedOpencode(t), repoSeedBaseVersion(t))
}

func TestReleasePreflightScript_BashSyntax(t *testing.T) {
	if out, err := exec.Command("bash", "-n", preflightScript).CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// TestReleasePreflightScript_StructurePins pins the four check rows and
// the fail-loud mechanics — a dropped check must fail here, not in the
// next incident review.
func TestReleasePreflightScript_StructurePins(t *testing.T) {
	raw, err := os.ReadFile(preflightScript)
	if err != nil {
		t.Fatalf("read %s: %v", preflightScript, err)
	}
	s := string(raw)
	rows := []struct {
		fragment string
		why      string
	}{
		{`GHCR_API:`, "registry endpoint must be overridable (tests, mirrors)"},
		{`is not CalVer YYYY.MM.x`, "P3: the incident-2026-09-02 detector"},
		{`catalog seed default row`, "P3: the single-source drift detector"},
		{`reuses a platform component digest`, "P2: the incident-2026-09-01 detector"},
		{`does not resolve`, "P1: the nonexistent-tag detector"},
		{`does not belong to this image`, "P2: foreign-digest index membership"},
		{`repo-validated pin`, "P4: unvalidated-opencode detector"},
		{`opencode-binary-contract.sh`, "P4: the behavioral contract script invocation"},
		{`--offline`, "explicit degraded mode must exist and be loud"},
		{`never use as a merge gate`, "offline degradation must carry its warning"},
		{`agentdDelivery.image is empty`, "P2: the absent-agentd-ref detector (mandatory delivery pin)"},
		{`the delivery pin is mandatory`, "delivery refs are mandatory, not skippable"},
		{`P4 opencode ref is digest-only`, "P4: honest digest-only message — never claim an unperformed comparison"},
		{`RESULT: FAIL`, "fail-loud exit contract"},
	}
	for _, r := range rows {
		if !strings.Contains(s, r.fragment) {
			t.Errorf("script lost its %s — fragment %q not found", r.why, r.fragment)
		}
	}
}

func TestReleasePreflightScript_OfflineChecks(t *testing.T) {
	t.Run("happy path passes offline checks", func(t *testing.T) {
		code, out := preflightRun(t, preflightHappyValues(t), "--offline")
		if code != 0 {
			t.Fatalf("expected exit 0, got %d:\n%s", code, out)
		}
		for _, want := range []string{
			"P3 base tag", "P2 offline", "P4 opencode coordinate aligned",
			"WARNING: --offline", "PASS (offline checks only)",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("platform semver base tag fails (incident 3)", func(t *testing.T) {
		values := strings.Replace(preflightHappyValues(t), `tag: "`+repoSeedBaseVersion(t)+`"`, `tag: "0.26.0"`, 1)
		code, out := preflightRun(t, values, "--offline")
		if code != 1 {
			t.Fatalf("expected exit 1, got %d:\n%s", code, out)
		}
		if !strings.Contains(out, "not CalVer") {
			t.Errorf("output must name the CalVer violation:\n%s", out)
		}
	})

	t.Run("calver base tag drifting from the seed fails", func(t *testing.T) {
		values := strings.Replace(preflightHappyValues(t), `tag: "`+repoSeedBaseVersion(t)+`"`, `tag: "2026.01.0"`, 1)
		code, out := preflightRun(t, values, "--offline")
		if code != 1 {
			t.Fatalf("expected exit 1, got %d:\n%s", code, out)
		}
		if !strings.Contains(out, "drift") || !strings.Contains(out, repoSeedBaseVersion(t)) {
			t.Errorf("output must name the drift and the seed value:\n%s", out)
		}
	})

	t.Run("agentd pin reusing the controller digest fails (incident 2)", func(t *testing.T) {
		values := strings.Replace(preflightHappyValues(t),
			`    tag: "0.30.1"
    digest: ""
  agentdDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:1111111111111111111111111111111111111111111111111111111111111111"`,
			`    tag: "0.30.1"
    digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"
  agentdDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:1111111111111111111111111111111111111111111111111111111111111111"`, 1)
		code, out := preflightRun(t, values, "--offline")
		if code != 1 {
			t.Fatalf("expected exit 1, got %d:\n%s", code, out)
		}
		if !strings.Contains(out, "reuses a platform component digest") {
			t.Errorf("output must name the collision:\n%s", out)
		}
	})

	t.Run("opencode coordinate off the validated pin fails", func(t *testing.T) {
		values := strings.Replace(preflightHappyValues(t),
			"opencode:"+repoPinnedOpencode(t)+"@",
			"opencode:1.2.3@", 1)
		code, out := preflightRun(t, values, "--offline")
		if code != 1 {
			t.Fatalf("expected exit 1, got %d:\n%s", code, out)
		}
		if !strings.Contains(out, "repo-validated pin") {
			t.Errorf("output must name the pin mismatch:\n%s", out)
		}
	})

	t.Run("empty base tag fails", func(t *testing.T) {
		values := strings.Replace(preflightHappyValues(t), `tag: "`+repoSeedBaseVersion(t)+`"`, `tag: ""`, 1)
		code, out := preflightRun(t, values, "--offline")
		if code != 1 {
			t.Fatalf("expected exit 1, got %d:\n%s", code, out)
		}
		if !strings.Contains(out, "empty") {
			t.Errorf("output must name the empty tag:\n%s", out)
		}
	})

	t.Run("missing file is a usage error", func(t *testing.T) {
		cmd := exec.Command("bash", "./"+preflightScript, "/nonexistent/values.yaml")
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("expected usage failure, got 0:\n%s", out)
		}
	})

	// Round-4 finding 3: a Dockerfile without the ARG pin must reach the
	// documented P4 die — not abort silently under pipefail+errexit.
	t.Run("dockerfile without the ARG pin fails P4 loudly", func(t *testing.T) {
		fixture := t.TempDir()
		seed, err := os.ReadFile(filepath.Join("..", "api", "internal", "imagefactory", "catalog.seed.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(fixture, "api", "internal", "imagefactory"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture, "api", "internal", "imagefactory", "catalog.seed.yaml"), seed, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(fixture, "runtimes", "opencode"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture, "runtimes", "opencode", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "candidate.yaml")
		if err := os.WriteFile(path, []byte(preflightHappyValues(t)), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "./"+preflightScript, path, "--offline")
		cmd.Env = append(os.Environ(), "LSS_REPO_ROOT="+fixture)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected failure, got pass:\n%s", out)
		}
		for _, want := range []string{
			"P4 cannot read the repo opencode pin",
			"RESULT: FAIL", // the documented exit path, not a silent abort
		} {
			if !strings.Contains(string(out), want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})
}

// stubRegistry serves the ghcr v2 API surface the script consumes:
// /token (anonymous pull token) and /v2/<repo>/manifests/<ref> with a
// Docker-Content-Digest header and (for index-annotation reads) a JSON
// index body. indexDigests maps ref->digest; indexBodies maps
// ref->manifest body; any ref in neither map 404s.
func stubRegistry(t *testing.T, indexDigests, indexBodies map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"stub-token"}`)
		case strings.HasPrefix(r.URL.Path, "/v2/"):
			ref := strings.TrimPrefix(r.URL.Path, "/v2/")
			digest, ok := indexDigests[ref]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
			body, hasBody := indexBodies[ref]
			if hasBody {
				w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			}
			w.WriteHeader(http.StatusOK)
			if hasBody {
				fmt.Fprint(w, body)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// indexBodyWithAnnotations renders an OCI index carrying the CI-stamped
// per-arch binary sha256 annotations for the named artifact.
func indexBodyWithAnnotations(artifact, amd64, arm64 string) string {
	return fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","annotations":{"dev.llmsafespaces/%s.sha256-amd64":"%s","dev.llmsafespaces/%s.sha256-arm64":"%s"}}`,
		artifact, amd64, artifact, arm64)
}

func TestReleasePreflightScript_OnlineResolution(t *testing.T) {
	const (
		apiTag       = "lenaxia/llmsafespaces/api/manifests/0.30.1"
		ctrlTag      = "lenaxia/llmsafespaces/controller/manifests/0.30.1"
		feTag        = "lenaxia/llmsafespaces/frontend/manifests/0.30.1"
		baseTag      = "lenaxia/llmsafespaces/base/manifests/" // + seed version
		agentdDig    = "lenaxia/llmsafespaces/agentd/manifests/sha256:1111111111111111111111111111111111111111111111111111111111111111"
		opencodeTg   = "lenaxia/llmsafespaces/opencode/manifests/" // + pin tag
		opencodeDg   = "lenaxia/llmsafespaces/opencode/manifests/sha256:2222222222222222222222222222222222222222222222222222222222222222"
		idx2222      = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		idxSomething = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	)

	baseRef := baseTag + repoSeedBaseVersion(t)
	ocTagRef := opencodeTg + repoPinnedOpencode(t)

	t.Run("all coordinates resolve", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			ocTagRef: idx2222, opencodeDg: idx2222,
		}, nil)
		cmd := exec.Command("bash", "./"+preflightScript, onlineCandidate(t, srv.URL))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("expected pass, got %v:\n%s", err, out.String())
		}
		for _, want := range []string{"P1", "resolves", "RESULT: PASS"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output missing %q:\n%s", want, out.String())
			}
		}
	})

	t.Run("nonexistent base tag fails (incident 3, online)", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			agentdDig: idx2222, ocTagRef: idx2222, opencodeDg: idx2222,
			// baseRef deliberately absent → 404
		}, nil)
		cmd := exec.Command("bash", "./"+preflightScript, onlineCandidate(t, srv.URL))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err == nil {
			t.Fatalf("expected failure, got pass:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "does not resolve") {
			t.Errorf("output must name the unresolved tag:\n%s", out.String())
		}
	})

	t.Run("foreign digest under the named image fails (incident 2, online)", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething,
			// agentdDig deliberately absent → the agentd repo has no such digest
			ocTagRef: idx2222, opencodeDg: idx2222,
		}, nil)
		cmd := exec.Command("bash", "./"+preflightScript, onlineCandidate(t, srv.URL))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err == nil {
			t.Fatalf("expected failure, got pass:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "does not belong to this image") {
			t.Errorf("output must name the foreign digest:\n%s", out.String())
		}
		if strings.Contains(out.String(), "membership verified by P1 resolution") {
			t.Errorf("a digest P1 just refuted must NOT be reported as membership-verified:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "RESULT: FAIL") {
			t.Errorf("a foreign-digest failure must still print the RESULT verdict:\n%s", out.String())
		}
	})

	t.Run("stale tag+digest pair fails coherence", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			ocTagRef:   idxSomething, // the tag now resolves to a DIFFERENT index than the pin
			opencodeDg: idx2222,
		}, nil)
		cmd := exec.Command("bash", "./"+preflightScript, onlineCandidate(t, srv.URL))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err == nil {
			t.Fatalf("expected failure, got pass:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "stale or foreign digest") {
			t.Errorf("output must name the stale pin:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "RESULT: FAIL") {
			t.Errorf("a reported failure must still print the RESULT verdict:\n%s", out.String())
		}
	})

	// binaryPinCandidate: happy values with agentd break-glass per-arch
	// pins set — the values.yaml caveat posture.
	binaryPinCandidate := func(t *testing.T, amd64, arm64 string) string {
		t.Helper()
		return strings.Replace(preflightHappyValues(t),
			`  agentdDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:1111111111111111111111111111111111111111111111111111111111111111"
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""`,
			`  agentdDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:1111111111111111111111111111111111111111111111111111111111111111"
    binarySHA256Amd64: "`+amd64+`"
    binarySHA256Arm64: "`+arm64+`"`, 1)
	}
	amd64Hex := strings.Repeat("aaaa0000", 8)
	arm64Hex := strings.Repeat("bbbb0000", 8)
	annotatedIndex := map[string]string{
		agentdDig: indexBodyWithAnnotations("agentd", amd64Hex, arm64Hex),
	}

	t.Run("binary pins matching the CI-stamped annotations pass", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			ocTagRef: idx2222, opencodeDg: idx2222,
		}, annotatedIndex)
		cmd := exec.Command("bash", "./"+preflightScript,
			onlineValues(t, srv.URL, binaryPinCandidate(t, amd64Hex, arm64Hex)))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("expected pass, got %v:\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "match the CI-stamped index annotations") {
			t.Errorf("output must confirm the annotation match:\n%s", out.String())
		}
	})

	t.Run("binary pin disagreeing with the index annotation fails (incident 2 class)", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			ocTagRef: idx2222, opencodeDg: idx2222,
		}, annotatedIndex)
		cmd := exec.Command("bash", "./"+preflightScript,
			onlineValues(t, srv.URL, binaryPinCandidate(t, strings.Repeat("cccc1111", 8), arm64Hex))) // wrong amd64 pin
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err == nil {
			t.Fatalf("expected failure, got pass:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "disagrees with the CI-stamped index annotation") {
			t.Errorf("output must name the annotation mismatch:\n%s", out.String())
		}
		if strings.Contains(out.String(), "match the CI-stamped index annotations") {
			t.Errorf("a reported mismatch must NOT be followed by a match ok:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "RESULT: FAIL") {
			t.Errorf("a binary-pin failure must still print the RESULT verdict (no mid-run abort):\n%s", out.String())
		}
	})

	// Round-4 finding 1: BOTH artifacts carrying bad pins must both be
	// reported, and the RESULT verdict must still print — the terminal
	// `&& ok` returned 1 and the bare call aborted the script mid-run.
	t.Run("both artifacts with mismatched pins report both failures and the summary", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			ocTagRef: idx2222, opencodeDg: idx2222,
		}, map[string]string{
			agentdDig:  indexBodyWithAnnotations("agentd", amd64Hex, arm64Hex),
			opencodeDg: indexBodyWithAnnotations("opencode", amd64Hex, arm64Hex),
		})
		wrongAmd64 := strings.Repeat("cccc1111", 8)
		wrongArm64 := strings.Repeat("ffff4444", 8)
		values := strings.Replace(binaryPinCandidate(t, wrongAmd64, arm64Hex),
			`  opencodeDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/opencode:`+repoPinnedOpencode(t)+`@sha256:2222222222222222222222222222222222222222222222222222222222222222"
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""`,
			`  opencodeDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/opencode:`+repoPinnedOpencode(t)+`@sha256:2222222222222222222222222222222222222222222222222222222222222222"
    binarySHA256Amd64: "`+amd64Hex+`"
    binarySHA256Arm64: "`+wrongArm64+`"`, 1)
		cmd := exec.Command("bash", "./"+preflightScript, onlineValues(t, srv.URL, values))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err == nil {
			t.Fatalf("expected failure, got pass:\n%s", out.String())
		}
		for _, want := range []string{
			"agentdDelivery.binarySHA256Amd64",   // the FIRST artifact's failure
			"opencodeDelivery.binarySHA256Arm64", // the sibling must not be skipped
			"disagrees with the CI-stamped index annotation",
			"RESULT: FAIL", // the verdict must still print after both checks ran
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output missing %q:\n%s", want, out.String())
			}
		}
		if strings.Contains(out.String(), "match the CI-stamped index annotations") {
			t.Errorf("a reported mismatch must NOT be accompanied by a match ok:\n%s", out.String())
		}
	})

	// Round-4 finding 2: after a P1 tag failure the coherence check must
	// not claim a live-index match it never performed.
	t.Run("P1 tag 404 with a digest pin set claims no live-index match", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			// ocTagRef deliberately absent → the opencode tag 404s
			opencodeDg: idx2222,
		}, nil)
		cmd := exec.Command("bash", "./"+preflightScript, onlineCandidate(t, srv.URL))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err == nil {
			t.Fatalf("expected failure, got pass:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "does not resolve") {
			t.Errorf("output must name the unresolved tag:\n%s", out.String())
		}
		if strings.Contains(out.String(), "pin matches the live index digest") {
			t.Errorf("a tag that failed P1 must not be reported as a live-index match:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "RESULT: FAIL") {
			t.Errorf("a P1 failure must still print the RESULT verdict:\n%s", out.String())
		}
	})

	// Round-6 finding: digest-only opencode refs (the mainline shape —
	// release.yml's merge-opencode block emits @digest with no tag) must
	// not claim "aligned with the repo pin": no comparison is possible
	// (the index annotations carry no opencode version), so the message
	// must say what IS verified and where the binding actually lives.
	t.Run("digest-only opencode ref claims no repo-pin alignment", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			// no ocTagRef: the mainline ref carries no tag at all
			opencodeDg: idx2222,
		}, nil)
		values := strings.Replace(preflightHappyValues(t),
			"opencode:"+repoPinnedOpencode(t)+"@", "opencode@", 1)
		cmd := exec.Command("bash", "./"+preflightScript, onlineValues(t, srv.URL, values))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("digest-only is the mainline shape and must pass, got %v:\n%s", err, out.String())
		}
		if strings.Contains(out.String(), "aligned with the repo pin") {
			t.Errorf("a digest-only ref carries no version to compare — alignment must not be claimed:\n%s", out.String())
		}
		for _, want := range []string{
			"P4 opencode ref is digest-only",
			"merge-opencode", // the procedural binding
			"RESULT: PASS",   // mainline shape still passes
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output missing %q:\n%s", want, out.String())
			}
		}
	})

	// Round-5 finding B: a candidate missing a mandatory delivery block
	// must FAIL, not silently PASS — the chart render fails on an empty
	// delivery ref, so a green pre-flight here would be skip=green on an
	// undeployable candidate.
	t.Run("candidate missing a delivery block fails (mandatory pin)", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			ocTagRef: idx2222, opencodeDg: idx2222,
		}, nil)
		pin := repoPinnedOpencode(t)
		scenarios := []struct {
			name    string
			block   string
			wantDie string
		}{
			{
				name: "agentd absent",
				block: `  agentdDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:1111111111111111111111111111111111111111111111111111111111111111"
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
`,
				wantDie: "agentdDelivery.image is empty",
			},
			{
				name: "opencode absent",
				block: `  opencodeDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/opencode:` + pin + `@sha256:2222222222222222222222222222222222222222222222222222222222222222"
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
`,
				wantDie: "opencodeDelivery.image is empty",
			},
		}
		for _, sc := range scenarios {
			cmd := exec.Command("bash", "./"+preflightScript,
				onlineValues(t, srv.URL, strings.Replace(preflightHappyValues(t), sc.block, "", 1)))
			var out strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Run(); err == nil {
				t.Errorf("%s: a missing mandatory delivery block must FAIL, got PASS:\n%s", sc.name, out.String())
			}
			if !strings.Contains(out.String(), sc.wantDie) {
				t.Errorf("%s: output must name the empty mandatory ref (%q):\n%s", sc.name, sc.wantDie, out.String())
			}
			if !strings.Contains(out.String(), "RESULT: FAIL") {
				t.Errorf("%s: output must print the RESULT verdict:\n%s", sc.name, out.String())
			}
			if strings.Contains(out.String(), "RESULT: PASS") {
				t.Errorf("%s: an undeployable candidate must never PASS:\n%s", sc.name, out.String())
			}
		}
	})

	t.Run("set pin on an un-annotated arch fails loud (no silent skip)", func(t *testing.T) {
		// Index annotated for amd64 ONLY; the candidate pins a foreign
		// arm64 hash — the round-3 Finding-B repro. Skip-must-not-pass.
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			ocTagRef: idx2222, opencodeDg: idx2222,
		}, map[string]string{agentdDig: indexBodyWithAnnotations("agentd", amd64Hex, "")})
		cmd := exec.Command("bash", "./"+preflightScript,
			onlineValues(t, srv.URL, binaryPinCandidate(t, amd64Hex, strings.Repeat("dddd2222", 8))))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err == nil {
			t.Fatalf("a set pin with no annotation on its arch must FAIL, got pass:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "no arm64 annotation") {
			t.Errorf("output must name the un-verifiable arch:\n%s", out.String())
		}
		if strings.Contains(out.String(), "match the CI-stamped index annotations") {
			t.Errorf("an un-verified pin must not produce a match ok:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "RESULT: FAIL") {
			t.Errorf("a binary-pin failure must still print the RESULT verdict (no mid-run abort):\n%s", out.String())
		}
	})

	t.Run("annotation fetch failure with pins set fails loud", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething,
			// agentdDig absent → P1 fails AND index_annotations cannot read
			ocTagRef: idx2222, opencodeDg: idx2222,
		}, nil)
		cmd := exec.Command("bash", "./"+preflightScript,
			onlineValues(t, srv.URL, binaryPinCandidate(t, amd64Hex, arm64Hex)))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err == nil {
			t.Fatalf("expected failure, got pass:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "cannot read index annotations") {
			t.Errorf("output must name the annotation fetch failure:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "RESULT: FAIL") {
			t.Errorf("an annotation-fetch failure must still print the RESULT verdict:\n%s", out.String())
		}
	})

	t.Run("opencode binary pin disagreeing with its index annotation fails (artifact symmetry)", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			ocTagRef: idx2222, opencodeDg: idx2222,
		}, map[string]string{opencodeDg: indexBodyWithAnnotations("opencode", amd64Hex, arm64Hex)})
		values := strings.Replace(preflightHappyValues(t),
			`  opencodeDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/opencode:`+repoPinnedOpencode(t)+`@sha256:2222222222222222222222222222222222222222222222222222222222222222"
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""`,
			`  opencodeDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/opencode:`+repoPinnedOpencode(t)+`@sha256:2222222222222222222222222222222222222222222222222222222222222222"
    binarySHA256Amd64: "`+strings.Repeat("eeee3333", 8)+`"
    binarySHA256Arm64: "`+arm64Hex+`"`, 1)
		cmd := exec.Command("bash", "./"+preflightScript, onlineValues(t, srv.URL, values))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err == nil {
			t.Fatalf("expected failure, got pass:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "opencodeDelivery.binarySHA256Amd64") ||
			!strings.Contains(out.String(), "disagrees with the CI-stamped index annotation") {
			t.Errorf("output must name the opencode-side mismatch:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "RESULT: FAIL") {
			t.Errorf("a binary-pin failure must still print the RESULT verdict (no mid-run abort):\n%s", out.String())
		}
	})

	t.Run("binary pins on an un-annotated index are the documented break-glass posture", func(t *testing.T) {
		srv := stubRegistry(t, map[string]string{
			apiTag: idxSomething, ctrlTag: idxSomething, feTag: idxSomething,
			baseRef: idxSomething, agentdDig: idx2222,
			ocTagRef: idx2222, opencodeDg: idx2222,
		}, map[string]string{agentdDig: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`})
		cmd := exec.Command("bash", "./"+preflightScript,
			onlineValues(t, srv.URL, binaryPinCandidate(t, amd64Hex, arm64Hex)))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("expected pass (break-glass posture), got %v:\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "break-glass posture") {
			t.Errorf("output must note the break-glass posture:\n%s", out.String())
		}
	})
}

func onlineCandidate(t *testing.T, apiURL string) string {
	t.Helper()
	return onlineValues(t, apiURL, preflightHappyValues(t))
}

func onlineValues(t *testing.T, apiURL, valuesBody string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.yaml")
	if err := os.WriteFile(path, []byte(valuesBody), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHCR_API", apiURL)
	return path
}
