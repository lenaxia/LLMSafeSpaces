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

	var runs string
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			runs += step.Run + "\n"
		}
	}
	require.NotEmpty(t, runs, "envtest.yml must have run steps")

	for pkg, pathPrefix := range envtestSuites {
		require.Regexp(t, regexp.MustCompile(regexp.QuoteMeta(pkg)), runs,
			"envtest.yml must execute the %s suite — a tagged suite not wired into CI never runs (PR #890 round-4 finding)", pkg)

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
