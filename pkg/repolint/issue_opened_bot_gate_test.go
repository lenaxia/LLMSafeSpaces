// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint_test

// The 2026-10-16 Issue Opened incident (#1709, run 38862526813): every
// issue filed by the platform's delivery app (agentic-actor-coder[bot])
// failed the responder run — opencode github run's pre-flight
// assertPermissions() requires the issue AUTHOR to be an admin/write
// collaborator, which app-bot accounts structurally cannot be
// (permission: none regardless of installation rights). 22+ red runs
// and one failure-annotation comment per platform-filed issue before
// anyone noticed. The fix (#1713) has three legs; these pins hold all
// of them: reverting any leg re-opens the red-run burn.

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const issueOpenedPath = ".github/workflows/issue-opened.yml"

// issueOpenedWorkflow unmarshals the responder workflow's respond job
// structurally — the r2 lesson from the tag-race pins: line-greps can
// be evaded by YAML respelling.
type issueOpenedWorkflow struct {
	Jobs struct {
		Respond struct {
			If    string `yaml:"if"`
			Steps []struct {
				Name            string `yaml:"name"`
				If              string `yaml:"if"`
				ContinueOnError *bool  `yaml:"continue-on-error"`
			} `yaml:"steps"`
		} `yaml:"respond"`
	} `yaml:"jobs"`
}

func parseIssueOpened(t *testing.T) issueOpenedWorkflow {
	t.Helper()
	var wf issueOpenedWorkflow
	if err := yaml.Unmarshal([]byte(readWorkflow(t, issueOpenedPath)), &wf); err != nil {
		t.Fatalf("%s does not parse: %v", issueOpenedPath, err)
	}
	if len(wf.Jobs.Respond.Steps) == 0 {
		t.Fatalf("%s has no respond job steps — the responder moved? extend the pin, do not skip it", issueOpenedPath)
	}
	return wf
}

// TestIssueOpenedWorkflow_BotAuthorGateIsActive: bot-filed issues are
// owned by platform worker lanes; the responder must SKIP them (the
// author-permission assert can never pass for an app bot, so a run
// only produces a red X plus an annotation comment). Pinned at both
// levels deliberately: the structural read catches a dropped job-if;
// the comment-blind activeLines check catches a gate that exists only
// commented-out in the raw source.
func TestIssueOpenedWorkflow_BotAuthorGateIsActive(t *testing.T) {
	wf := parseIssueOpened(t)
	if !strings.Contains(wf.Jobs.Respond.If, "user.type") ||
		!strings.Contains(wf.Jobs.Respond.If, "!= 'Bot'") {
		t.Errorf("respond job if = %q — lost the bot-author gate; platform-filed issues would burn a red run + annotation comment per issue again (#1709)", wf.Jobs.Respond.If)
	}
	if n := activeLines(readWorkflow(t, issueOpenedPath), `if: github\.event\.issue\.user\.type != 'Bot'`); n < 1 {
		t.Errorf("%s has %d ACTIVE bot-gate if-lines (want ≥1) — a commented-out gate must not pass (#1709)", issueOpenedPath, n)
	}
}

// TestIssueOpenedWorkflow_OpenCodeStepFailsSoft: an agent failure must
// degrade to "no AI response", not a red run — continue-on-error on
// the Run OpenCode step.
func TestIssueOpenedWorkflow_OpenCodeStepFailsSoft(t *testing.T) {
	for _, step := range parseIssueOpened(t).Jobs.Respond.Steps {
		if step.Name != "Run OpenCode" {
			continue
		}
		if step.ContinueOnError == nil || !*step.ContinueOnError {
			t.Errorf("Run OpenCode step lacks continue-on-error: true — an agent failure (e.g. the author-permission assert) red-runs the workflow and re-opens the annotation-comment noise (#1709)")
		}
		return
	}
	t.Fatalf("%s has no Run OpenCode step — the agent step moved? extend the pin, do not skip it", issueOpenedPath)
}

// TestIssueOpenedWorkflow_FooterRunsAlways: the command-reference
// footer must survive an agent-step failure (if: always()) — a soft
// failure must not silently drop the responder's thread bookkeeping.
func TestIssueOpenedWorkflow_FooterRunsAlways(t *testing.T) {
	for _, step := range parseIssueOpened(t).Jobs.Respond.Steps {
		if !strings.HasPrefix(step.Name, "Post AI command reference") {
			continue
		}
		if !strings.Contains(step.If, "always()") {
			t.Errorf("footer step if = %q — lacks always(); a failed agent step would skip the command-reference comment (#1709)", step.If)
		}
		return
	}
	t.Fatalf("%s has no Post AI command reference step — the footer moved? extend the pin, do not skip it", issueOpenedPath)
}
