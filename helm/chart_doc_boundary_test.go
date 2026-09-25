// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// The glued-doc class pin (r2 on #1566, missing-test 3): a missing
// document separator + template whitespace-trimming can fuse TWO
// objects into ONE rendered document; helm/kubectl decode such a doc
// with last-wins semantics (sigs.k8s.io/yaml) and the FIRST object is
// silently dropped at apply — the api-leader-election apply-loss this
// PR peeled, and the api-platform-info RoleBinding loss r2 caught
// still live in rbac.yaml. This pin decodes EVERY rendered document
// with gopkg.in/yaml.v3, which hard-errors on duplicate mapping keys:
// any glued doc anywhere in the render fails HERE, mechanically, at
// the render layer — before install, before the gate.

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
)

// helmRenderRaw mirrors helmTemplate's invocation (same synthetic
// delivery pins, same kube-version pin) but returns the RAW combined
// output — the doc-splitting behavior under test must see exactly
// what `helm template | kubectl apply -f -` sees.
func helmRenderRaw(t *testing.T, valuesYAML string) []byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	args := []string{"template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion}
	if valuesYAML != "" {
		dir := t.TempDir()
		valuesPath := filepath.Join(dir, "values.yaml")
		require.NoError(t, writeFile(valuesPath, valuesYAML))
		args = append(args, "-f", valuesPath)
	}
	args = append(args,
		"--set", "controller.agentdDelivery.image="+testAgentdPin,
		"--set", "controller.opencodeDelivery.image="+testOpencodePin)
	cmd := exec.Command("helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "helm template failed: %s", stderr.String())
	return stdout.Bytes()
}

func TestEveryRenderedDocIsSingleObject(t *testing.T) {
	// Two postures: the shipped default (rbac.create on, namespace
	// scope — exercises the platform-info boundary) and the relay-only
	// fleet variant (the relay-safe ClusterRoleBinding surface).
	for name, values := range map[string]string{
		"shipped-default": "",
		"fleet-namespace": "controller:\n  inferenceRelay:\n    enabled: true\n  watchNamespaces: llmsafespaces\n",
	} {
		t.Run(name, func(t *testing.T) {
			docs := splitYAMLDocs(helmRenderRaw(t, values))
			require.NotEmpty(t, docs)
			objects := 0
			for i, d := range docs {
				if len(bytes.TrimSpace(d)) == 0 {
					continue
				}
				var m map[string]any
				// yaml.v3 hard-errors on duplicate mapping keys — a
				// glued doc (two objects concatenated, last-wins under
				// the sigs.k8s.io/yaml decode helm/kubectl apply uses)
				// fails HERE with "already defined".
				require.NoError(t, yamlv3.Unmarshal(d, &m),
					"rendered doc #%d is not a single YAML object — glued objects are silently last-wins at apply (the #1566 apply-loss class):\n%s",
					i, truncateForMessage(d))
				if m == nil {
					continue // comment-only chunk
				}
				require.Contains(t, m, "kind", "rendered doc #%d has no kind: %v", i, keysOfAny(m))
				objects++
			}
			require.Greater(t, objects, 0, "the render must produce at least one object")
		})
	}
}

func truncateForMessage(b []byte) string {
	const max = 400
	s := string(b)
	if len(s) > max {
		return s[:max] + "\n…(truncated)"
	}
	return s
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
