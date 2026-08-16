// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

// Workflow shell-syntax guard.
//
// Three consecutive CI incidents (#878, #881, and the missing-quote fix
// after it) shipped from .github/workflows/ci.yml's build-agentd step,
// each a shell-level defect invisible to YAML parsing:
//
//   1. a doubled digest prefix (@sha256:sha256:… → invalid reference),
//   2. a comment placed inside a backslash continuation (ate the source
//      argument → "no sources specified"),
//   3. a dropped closing quote (unexpected EOF at run time).
//
// YAML validity proves nothing about the embedded shell. This test
// extracts every `run: |` block from the repo's workflow files,
// substitutes the ${{ }} expressions GitHub would expand with plausible
// dummy values, and runs `bash -n` over the rendered script. It would
// have caught (3) outright and made (1)/(2) visible in the rendered
// output; more importantly it locks in the render-and-check discipline.
//
// It intentionally does NOT execute anything — syntax only.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

type workflowJob struct {
	Steps []struct {
		Name string `json:"name"`
		Run  string `json:"run"`
	} `json:"steps"`
}

type workflowFile struct {
	Jobs map[string]workflowJob `json:"jobs"`
}

// expressionRe matches ${{ ... }} blocks.
var expressionRe = regexp.MustCompile(`\$\{\{.*?\}\}`)

// dummySubstitutions maps exact expression texts to plausible values.
// Generic fallbacks cover the rest by shape (sha → hex, digest →
// sha256:hex, anything else → a safe single token).
var dummySubstitutions = map[string]string{
	"${{ github.sha }}":                                             strings.Repeat("a", 40),
	"${{ steps.build.outputs.digest }}":                             "sha256:" + strings.Repeat("b", 64),
	"${{ matrix.platform }}":                                        "linux/amd64",
	"${{ env.REGISTRY }}":                                           "ghcr.io",
	"${{ github.event_name }}":                                      "push",
	"${{ github.ref }}":                                             "refs/heads/main",
	"${{ github.ref_name }}":                                        "main",
	"${{ github.repository }}":                                      "lenaxia/LLMSafeSpaces",
	"${{ github.workspace }}":                                       "/workspace",
	"${{ github.run_id }}":                                          "123456",
	"${{ github.actor }}":                                           "ci",
	"${{ secrets.GITHUB_TOKEN }}":                                   "dummy-token",
	"${{ env.AGENTD_IMAGE }}":                                       "lenaxia/llmsafespaces/agentd",
	"${{ matrix.runner }}":                                          "ubuntu-latest",
	"${{ needs.prepare.outputs.version }}":                          "0.0.0-test",
	"${{ needs.prepare.outputs.timestamp }}":                        "19700101T000000Z",
	"${{ matrix.platform == 'linux/arm64' && 'arm64' || 'amd64' }}": "amd64",
}

var hex40Re = regexp.MustCompile(`\$\{\{.*?[Ss][Hh][Aa].*?\}\}`)

func substituteExpressions(script string) string {
	script = expressionRe.ReplaceAllStringFunc(script, func(expr string) string {
		if v, ok := dummySubstitutions[expr]; ok {
			return v
		}
		if hex40Re.MatchString(expr) {
			if strings.Contains(expr, "digest") {
				return "sha256:" + strings.Repeat("c", 64)
			}
			return strings.Repeat("d", 40)
		}
		// Generic fallback: a single safe token. Shell-unsafe characters
		// must never come from the substitution itself.
		return "dummyvalue"
	})
	return script
}

func TestWorkflowRunBlocksAreValidShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH; skipping workflow shell syntax check")
	}
	root := repoRoot(t)
	wfDir := filepath.Join(root, ".github", "workflows")
	entries, err := filepath.Glob(filepath.Join(wfDir, "*.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, entries, "no workflow files found")

	checked := 0
	for _, path := range entries {
		raw, err := os.ReadFile(path)
		require.NoError(t, err, path)
		var wf workflowFile
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			// Non-workflow YAML (rare) — skip silently.
			continue
		}
		for jobName, job := range wf.Jobs {
			for _, step := range job.Steps {
				if step.Run == "" {
					continue
				}
				checked++
				rendered := substituteExpressions(step.Run)
				name := step.Name
				if name == "" {
					name = "(unnamed)"
				}
				cmd := exec.Command("bash", "-n")
				cmd.Stdin = strings.NewReader(rendered)
				out, err := cmd.CombinedOutput()
				require.NoError(t, err,
					"%s job %q step %q: rendered shell is not syntactically valid (this exact class of bug broke CI three times — see worklog for #878/#881):\nrendered:\n%s\nbash: %s",
					filepath.Base(path), jobName, name, rendered, out)
			}
		}
	}
	require.Positive(t, checked, "sanity: expected to check at least one run block")
}
