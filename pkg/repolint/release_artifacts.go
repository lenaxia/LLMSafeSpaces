// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ReleaseArtifactCompleteness guards the release workflow's artifact
// invariants. Root cause (v0.19.1, 2026-08-20): release.yml built six
// images but agentd was built only by the tag CI; the tag run failed on
// an unrelated flake, the release went GREEN, and agentd:0.19.1 was never
// published — "release succeeded" and "all artifacts exist" silently
// diverged. The fix (PR #1008) moved agentd into release.yml AND wired it
// into signing, scanning, SBOM, and the release notes; this check makes
// every piece of that wiring a lint-enforced invariant so the next image
// (or the next refactor of these loops) cannot silently regress it.
//
// Invariants, for EVERY *_IMAGE env var defined in the workflow's env
// block:
//
//  1. Its component appears in the sign-images, scan-images, and
//     generate-sbom iteration loops (`for img in …`).
//  2. Its image ref appears in the create-release body's image table.
//  3. Every merge-* job in the workflow is in create-release's needs
//     (and in sign-images' needs — the signed set must equal the built
//     set).
//
// Parsing is deliberately textual (regex over the YAML body): the loops
// and the heredoc table are shell strings, not YAML structure.

type releaseWorkflow struct {
	env  map[string]string // *_IMAGE env var name -> image path
	jobs map[string]releaseJob
}

type releaseJob struct {
	needs []string
	body  string // full textual body (run scripts, heredocs) for loop checks
}

func parseReleaseWorkflow(path string) (*releaseWorkflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	w := &releaseWorkflow{env: map[string]string{}, jobs: map[string]releaseJob{}}

	// env block: top-level `  NAME: value` pairs (2-space indent) — the
	// images are all under the workflow-level env.
	envRe := regexp.MustCompile(`(?m)^  ([A-Z_]+_IMAGE): (\S+)$`)
	for _, m := range envRe.FindAllStringSubmatch(string(data), -1) {
		w.env[m[1]] = m[2]
	}
	if len(w.env) == 0 {
		return nil, fmt.Errorf("no *_IMAGE env vars found — parser drift?")
	}

	// jobs: split on `^  job-name:` at 2-space indent.
	jobRe := regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]+):$`)
	idx := jobRe.FindAllStringSubmatchIndex(string(data), -1)
	for i, pair := range idx {
		name := string(data[pair[2]:pair[3]])
		end := len(data)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		body := string(data[pair[1]:end])
		w.jobs[name] = releaseJob{body: body}
	}
	if len(w.jobs) == 0 {
		return nil, fmt.Errorf("no jobs found — parser drift?")
	}

	// needs: parse from each job body.
	needsRe := regexp.MustCompile(`(?m)^    needs: \[(.*)\]$`)
	for name, job := range w.jobs {
		if m := needsRe.FindStringSubmatch(job.body); m != nil {
			for _, n := range strings.Split(m[1], ",") {
				job.needs = append(job.needs, strings.TrimSpace(n))
			}
			w.jobs[name] = job
		}
	}
	return w, nil
}

// componentFor maps an env var to its loop name (API_IMAGE→api,
// RELAY_PROXY_IMAGE→relay-proxy — loop names use hyphens). The base no
// longer appears: design 0053 D5 moved it off the release train into
// base-image.yml.
func componentFor(envName string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSuffix(envName, "_IMAGE")), "_", "-")
}

func loopHas(body, component string) bool {
	loopRe := regexp.MustCompile(`for img in ([a-z0-9 -]+); do`)
	for _, m := range loopRe.FindAllStringSubmatch(body, -1) {
		for _, tok := range strings.Fields(m[1]) {
			if tok == component {
				return true
			}
		}
	}
	return false
}

func hasNeed(job releaseJob, target string) bool {
	for _, n := range job.needs {
		if n == target {
			return true
		}
	}
	return false
}

// RunReleaseArtifactCompleteness returns one failure line per violated
// invariant. Empty = clean.
func RunReleaseArtifactCompleteness(root string) []string {
	path := filepath.Join(root, ".github", "workflows", "release.yml")
	w, err := parseReleaseWorkflow(path)
	if err != nil {
		return []string{fmt.Sprintf("release artifacts: cannot parse %s: %v", path, err)}
	}

	var fails []string
	envNames := make([]string, 0, len(w.env))
	for n := range w.env {
		envNames = append(envNames, n)
	}
	sort.Strings(envNames)

	for _, envName := range envNames {
		comp := componentFor(envName)
		image := w.env[envName]
		for _, jobName := range []string{"sign-images", "scan-images", "generate-sbom"} {
			job, ok := w.jobs[jobName]
			if !ok {
				fails = append(fails, fmt.Sprintf("release artifacts: job %s missing from workflow", jobName))
				continue
			}
			if !loopHas(job.body, comp) {
				fails = append(fails, fmt.Sprintf("release artifacts: %s does not process %s (%s) — every image must be signed, scanned, and SBOM'd", jobName, comp, envName))
			}
			_ = image
		}
		cr, ok := w.jobs["create-release"]
		if !ok {
			fails = append(fails, "release artifacts: create-release job missing from workflow")
			continue
		}
		if !strings.Contains(cr.body, image+":${VERSION}") {
			fails = append(fails, fmt.Sprintf("release artifacts: create-release image table missing %s (%s)", image, envName))
		}
	}

	// Every merge-* job must gate the release (and signing).
	var mergeJobs []string
	for name := range w.jobs {
		if strings.HasPrefix(name, "merge-") {
			mergeJobs = append(mergeJobs, name)
		}
	}
	sort.Strings(mergeJobs)
	for _, target := range []string{"create-release", "sign-images", "scan-images", "generate-sbom"} {
		job, ok := w.jobs[target]
		if !ok {
			continue // already reported above for the fixed set
		}
		for _, m := range mergeJobs {
			if !hasNeed(job, m) {
				fails = append(fails, fmt.Sprintf("release artifacts: %s does not need %s — release success must require every artifact job", target, m))
			}
		}
	}

	return fails
}
