// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

// Guard guard: the envtest suites must actually run in CI.
//
// PR #890 review rounds 3–5: a build-tagged envtest suite can be added,
// referenced in docs, and pass locally — while no CI workflow executes
// it (the `paths:` filter didn't even include the directory). vet
// type-checks but never runs tests. This guard parses
// .github/workflows/envtest.yml and fails unless each registered
// package below appears in some `go test` step's arguments. Deleting
// the envtest test file, the workflow step, or the paths entry (making
// the step unreachable for changes to that package) all fail here.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

type envtestWorkflow struct {
	// sigs.k8s.io/yaml converts YAML `on` to the JSON key "true" (YAML
	// 1.1 boolean) — the classic gotcha; the tag must be "true".
	On struct {
		PullRequest struct {
			Paths []string `json:"paths"`
		} `json:"pull_request"`
	} `json:"true"`
	Jobs map[string]struct {
		Steps []struct {
			Name string `json:"name"`
			Run  string `json:"run"`
		} `json:"steps"`
	} `json:"jobs"`
}

// envtestSuites maps CI-executed test packages to a path-prefix that
// must appear in the workflow's pull_request paths trigger (so changes
// to the package actually reach the workflow).
var envtestSuites = map[string]string{
	"./pkg/apis/llmsafespaces/v1/":     "pkg/apis/",
	"./controller/internal/webhooks/":  "controller/internal/webhooks/",
	"./controller/internal/workspace/": "controller/internal/workspace/",
}

func TestEnvtestWorkflow_RunsAllRegisteredSuites(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "envtest.yml"))
	require.NoError(t, err)

	var wf envtestWorkflow
	require.NoError(t, yaml.Unmarshal(raw, &wf), "envtest.yml must parse")

	// runsByLine keeps each step's run block as searchable lines so a
	// flag preceding the package path (-tags envtest go test ... ./pkg)
	// is still within the matched line.
	var lines []string
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			for _, l := range strings.Split(step.Run, "\n") {
				lines = append(lines, l)
			}
		}
	}
	require.NotEmpty(t, lines, "envtest.yml must have run steps")

	for pkg, pathPrefix := range envtestSuites {
		m := ""
		for _, l := range lines {
			if strings.Contains(l, pkg) {
				m = l
				break
			}
		}
		require.NotEmpty(t, m,
			"envtest.yml must execute the %s suite — a tagged suite not wired into CI never runs (PR #890 round-4 finding)", pkg)
		// The step must set -tags envtest — without it the tagged suite
		// compiles to nothing and the step passes vacuously.
		require.Contains(t, m, "-tags envtest",
			"the %s step must run with -tags envtest (build-tagged files are invisible otherwise)", pkg)
		// Non-vacuous step: the step's -run pattern must match a test
		// function that exists in the package (catches renamed/deleted
		// test funcs leaving a CI step that matches nothing and passes).
		runPat := regexp.MustCompile(`-run (\S+)`).FindString(m)
		require.NotEmpty(t, runPat, "the %s step must select tests via -run", pkg)
		pattern := regexp.MustCompile(`-run (\S+)`).FindStringSubmatch(m)[1]
		funcs := testFuncsIn(t, filepath.Join(root, filepath.FromSlash(pkg[2:])))
		require.NotEmpty(t, funcs, "no test functions found under %s — is the package path right?", pkg)
		matched := false
		for _, f := range funcs {
			if regexpMatch(pattern, f) {
				matched = true
				break
			}
		}
		require.True(t, matched,
			"envtest.yml step '-run %s' matches NO test function in %s (found %d funcs) — a vacuous step passes forever", pattern, pkg, len(funcs))

		found := false
		for _, p := range wf.On.PullRequest.Paths {
			if p == pathPrefix+"**" || p == pathPrefix {
				found = true
				break
			}
		}
		require.True(t, found,
			"envtest.yml pull_request paths must include %s** — without it, changes to %s never trigger the workflow that runs its suite", pathPrefix, pkg)
	}
}

// testFuncsIn returns Test* function names declared in _test.go files
// under dir (including build-tagged ones — the suite runs with tags).
func testFuncsIn(t *testing.T, dir string) []string {
	t.Helper()
	var funcs []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range regexp.MustCompile(`(?m)^func (Test\w+)\(`).FindAllStringSubmatch(string(data), -1) {
			funcs = append(funcs, m[1])
		}
		return nil
	})
	require.NoError(t, err)
	return funcs
}

// regexpMatch: Go test -run patterns are anchored regexes over test
// names; a simple substring+anchor approximation suffices for the
// simple identifiers used here (TestEnvtest, TestEnvtestAgentdPins).
func regexpMatch(pattern, name string) bool {
	p := strings.TrimSuffix(pattern, "$")
	p = strings.TrimPrefix(p, "^")
	return strings.Contains(name, p)
}
