// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// wait_broker_scripts.go — the wait-broker's three in-tree gate
// templates (claim-wait lifecycle, low-level design §4.3): the
// declarative primitives compile to #1812 gate scripts — python
// handlers returning {"proceed": bool, "vars": {}} — and NOTHING
// worker-authored ever reaches a trigger (the worker surface supplies
// only the parsed primitive + argument + repo identity; the broker
// renders from THESE templates, byte-pinned by the unit suite).
//
// Template posture (docs/automation/gate-script-patterns.md):
//   - Pattern 1: PLAIN fetches — no ETag opt-in, no 304 exception
//     path, no CDN-stale pinning; one call per observation.
//   - Pattern 3: state dedupe via the marker-file signature. The gate
//     envelope carries only {source, received_at} and trigger vars are
//     NOT readable by the script, so the script's own marker file at
//     the FLAT per-episode path is the sanctioned state channel.
//     Signature = "<seed>|<observed>" where seed = template version +
//     the declaration's wait_declared event id (a compile-time constant
//     rendered below): mark-on-observation, skip-on-match, PERSISTS
//     past proceed (deleting it would re-arm the transition predicate
//     and fire every poll). A long-satisfied condition fires ONCE per
//     declaration; a RE-DECLARED wait (new event id => new seed, fresh
//     or updated trigger script) fires again even on an unchanged
//     condition — never the fire-never stale-suppression shape.
//   - Pattern 4: quiet poller — every transient failure (fetch error,
//     timeout, rate limit) returns proceed=false and lets the 2-min
//     schedule retry; no failed-fire accounting, no noise.
//
// Litter posture, stated honestly: the broker (API process) has no
// workspace filesystem access and CANNOT sweep markers; every episode
// leaves exactly one flat ≤256 B marker file (satisfied path included
// — the marker IS the dedupe state, so it outlives its fire), bounded
// at one per episode, platform-unlinkable by name like every flat
// exchange file.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// waitGateTemplateVersion rides every rendered seed so a future
// template change re-arms sensors it re-renders (the signature's seed
// half changes; the observed half is re-compared on the next poll).
const waitGateTemplateVersion = "wgv1"

// waitBrokerTriggerPrefix is the broker's ownership label namespace:
// every trigger the broker owns carries this name prefix (the DB's
// name grammar forbids "/", so the canonical slash label
// wait-broker/<short_id>/<hash8> is rendered into the DESCRIPTION —
// the name is the ownership key, the description is its audit prose).
const waitBrokerTriggerPrefix = "wait-broker-"

// waitBrokerCronExpr is the sensor poll cadence — */2 (>= 2 min), the
// storm guard pinned by the design (§4.2): no broker-owned trigger may
// poll faster.
const waitBrokerCronExpr = "*/2 * * * *"

// waitBrokerAutoDisableAfter bounds a sensor's failure storm before
// the broker's re-materialize-once-then-degrade-loud posture engages.
const waitBrokerAutoDisableAfter = 5

// waitGateSeed composes the compile-time dedupe seed: template version
// + the declaration's wait_declared event id.
func waitGateSeed(declarationEventID string) string {
	return waitGateTemplateVersion + ":" + declarationEventID
}

// claimTokenHash8 renders the trigger-name/label identity half: the
// first 8 hex chars of sha256(claimToken) — hashed, never the raw
// token, so the ownership label cannot leak the in-flight guard.
func claimTokenHash8(claimToken string) string {
	sum := sha256.Sum256([]byte(claimToken))
	return hex.EncodeToString(sum[:])[:8]
}

// waitBrokerTriggerName renders the deterministic trigger name
// wait-broker-<short_id>-<hash8> (DB-grammar-compliant; deterministic
// ids + the (owner, name) unique constraint arbitrate cross-replica
// races — two replicas converge).
func waitBrokerTriggerName(shortID, claimToken string) string {
	return waitBrokerTriggerPrefix + shortID + "-" + claimTokenHash8(claimToken)
}

// waitBrokerMarkerPath renders the FLAT per-episode marker path in the
// routine target workspace's payload exchange dir (flat by design:
// nested exchange dirs are the umask-0755 no-group-write EACCES class
// board_file_params.go documents — a nested marker could never be
// platform-unlinked).
func waitBrokerMarkerPath(shortID, claimToken string) string {
	return "/exchange/wait-broker-" + shortID + "-" + claimTokenHash8(claimToken) + ".marker"
}

// waitBrokerLabel renders the canonical slash label (the audit shape
// the design names) into the trigger's description.
func waitBrokerLabel(shortID, claimToken string) string {
	return "wait-broker/" + shortID + "/" + claimTokenHash8(claimToken)
}

// The shared marker helpers every template embeds (Pattern 3).
const waitGateMarkerHelpers = `def _marker():
    try:
        with open(MARKER) as f:
            return f.read()
    except OSError:
        return None

def _mark(sig):
    try:
        with open(MARKER, "w") as f:
            f.write(sig)
    except OSError:
        pass  # unwritable marker: worst case is one extra fire — acceptable
`

// waitGateTemplateGitHub renders the PR-polling preamble shared by
// pr_merged/ci_green (anonymous by default; GITHUB_TOKEN/GH_TOKEN from
// the target workspace's materialized env is used when present).
const waitGateTemplateGitHubAuth = `def _gh(url):
    req = urllib.request.Request(url, headers={
        "Accept": "application/vnd.github+json",
        "User-Agent": "llmsafespaces-wait-broker",
    })
    tok = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    if tok:
        req.add_header("Authorization", "Bearer " + tok)
    return urllib.request.urlopen(req, timeout=10)
`

// waitGateTemplatePRMerged — pr_merged(N): proceed iff the PR is
// merged with a merge_commit_sha; vars carry the SHA.
const waitGateTemplatePRMerged = `import json, os, urllib.request

# wait-broker sensor pr_merged(__PR__) on __REPO__ — compiled by the platform's wait-broker (template ` + waitGateTemplateVersion + `) from a declarative condition; never worker-authored.
# Pattern 3 state dedupe: the marker file is the ONLY cross-fire state; signature = seed|merge_commit_sha; write-on-observation, skip-on-match, PERSISTS past proceed.
PR_URL = "__PR_URL__"
REPO = "__REPO__"
PR = "__PR__"
MARKER = "__MARKER__"
SEED = "__SEED__"

` + waitGateMarkerHelpers + waitGateTemplateGitHubAuth + `
def handler(input):
    try:
        with _gh(PR_URL) as r:
            pr = json.load(r)
        sha = pr.get("merge_commit_sha") or ""
        sig = SEED + "|" + sha
        if _marker() == sig:
            return {"proceed": False, "vars": {}}  # already observed this exact state
        _mark(sig)
        if bool(pr.get("merged")) and sha:
            return {"proceed": True, "vars": {"pr": PR, "repo": REPO, "merge_commit_sha": sha}}
        return {"proceed": False, "vars": {}}
    except Exception as e:
        # quiet poller: transient fetch errors skip this fire; the schedule is the retry
        return {"proceed": False, "vars": {"skipped_error": str(e)}}
`

// waitGateTemplateCIGreen — ci_green(N): fetch the PR's head SHA, then
// its check-runs; proceed iff total>0 and every conclusion is
// success|skipped|neutral; vars carry the SHA + counts.
const waitGateTemplateCIGreen = `import json, os, urllib.request

# wait-broker sensor ci_green(__PR__) on __REPO__ — compiled by the platform's wait-broker (template ` + waitGateTemplateVersion + `) from a declarative condition; never worker-authored.
# Pattern 3 state dedupe: signature = seed|head_sha|total|conclusions — every check-state change re-arms (a failing→passing transition fires); write-on-observation, PERSISTS past proceed.
PR_URL = "__PR_URL__"
CHECK_URL = "__GH_BASE__/repos/__REPO__/commits/%s/check-runs"
REPO = "__REPO__"
PR = "__PR__"
MARKER = "__MARKER__"
SEED = "__SEED__"
OK = ("success", "skipped", "neutral")

` + waitGateMarkerHelpers + waitGateTemplateGitHubAuth + `
def handler(input):
    try:
        with _gh(PR_URL) as r:
            pr = json.load(r)
        sha = (pr.get("head") or {}).get("sha") or ""
        if not sha:
            return {"proceed": False, "vars": {"skipped": "no head sha yet"}}
        with _gh(CHECK_URL % sha) as r:
            cr = json.load(r)
        total = cr.get("total") or 0
        concls = sorted((c.get("conclusion") or "") for c in (cr.get("check_runs") or []))
        sig = SEED + "|" + sha + "|" + str(total) + "|" + ",".join(concls)
        if _marker() == sig:
            return {"proceed": False, "vars": {}}  # already observed this exact state
        _mark(sig)
        if total > 0 and all(c in OK for c in concls):
            return {"proceed": True, "vars": {"pr": PR, "repo": REPO, "head_sha": sha, "checks_total": str(total)}}
        return {"proceed": False, "vars": {}}
    except Exception as e:
        # quiet poller: transient fetch errors skip this fire; the schedule is the retry
        return {"proceed": False, "vars": {"skipped_error": str(e)}}
`

// waitGateTemplateStoryDone — story_done(short_id): the platform's own
// board read over the pod identity (the same internal route + SA token
// the pod's agentd MCP tools ride); proceed iff stage == done. The
// read is workspace-bound (GetTaskInWorkspace): a cross-workspace
// short_id reads as not-found and skips quietly — the ladder governs.
const waitGateTemplateStoryDone = `import json, os, urllib.request

# wait-broker sensor story_done(__SHORT_ID__) — compiled by the platform's wait-broker (template ` + waitGateTemplateVersion + `) from a declarative condition; never worker-authored.
# Reads the platform's own board over the pod identity (LLMSAFESPACE_API_URL — controller-set per pod; the rendered default is the broker's in-cluster URL belt).
# Pattern 3 state dedupe: signature = seed|stage|updated_at — write-on-observation, skip-on-match, PERSISTS past proceed.
BOARD_URL = "__BOARD_URL__"
SHORT_ID = "__SHORT_ID__"
MARKER = "__MARKER__"
SEED = "__SEED__"

` + waitGateMarkerHelpers + `
def handler(input):
    try:
        base = os.environ.get("LLMSAFESPACE_API_URL") or "__BOARD_DEFAULT__"
        tok = ""
        try:
            with open(os.environ.get("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE") or "/var/run/bootstrap/token") as f:
                tok = f.read().strip()
        except OSError:
            tok = ""
        req = urllib.request.Request(BOARD_URL, headers={
            "Accept": "application/json",
            "User-Agent": "llmsafespaces-wait-broker",
        })
        if tok:
            req.add_header("Authorization", "Bearer " + tok)
        with urllib.request.urlopen(req, timeout=10) as r:
            task = json.load(r)
        stage = task.get("stage") or ""
        sig = SEED + "|" + stage + "|" + (task.get("updated_at") or "")
        if _marker() == sig:
            return {"proceed": False, "vars": {}}  # already observed this exact state
        _mark(sig)
        if stage == "done":
            return {"proceed": True, "vars": {"story": SHORT_ID, "stage": stage}}
        return {"proceed": False, "vars": {}}
    except Exception as e:
        # quiet poller: transient fetch errors skip this fire; the schedule is the retry
        return {"proceed": False, "vars": {"skipped_error": str(e)}}
`

// waitBrokerGitHubAPI is the GitHub REST base the pr_merged/ci_green
// sensors poll (a package var so the executable pin suite can point
// the rendered scripts at a stub — the imagefactory dispatcher's
// dispatchURL precedent).
var waitBrokerGitHubAPI = "https://api.github.com"

// renderWaitGateScript compiles one declarative condition into its
// gate-script handler source. The condition is DATA validated at the
// verb edge and re-parsed here (board.ParseWaitCondition — nothing
// unparseable reaches a trigger even via a hand-edited event row);
// repo is the declaration-time identity the GitHub templates poll —
// the broker never guesses it.
func renderWaitGateScript(cond board.WaitCondition, repo, declarationEventID, markerPath, boardBaseURL, shortID string) (string, error) {
	seed := waitGateSeed(declarationEventID)
	fill := func(tpl string, subs ...string) string {
		out := tpl
		for i := 0; i+1 < len(subs); i += 2 {
			out = strings.ReplaceAll(out, subs[i], subs[i+1])
		}
		return out
	}
	switch cond.Primitive {
	case "pr_merged":
		return fill(waitGateTemplatePRMerged,
			"__PR_URL__", fmt.Sprintf("%s/repos/%s/pulls/%s", waitBrokerGitHubAPI, repo, cond.Arg),
			"__REPO__", repo,
			"__PR__", cond.Arg,
			"__MARKER__", markerPath,
			"__SEED__", seed,
		), nil
	case "ci_green":
		return fill(waitGateTemplateCIGreen,
			"__PR_URL__", fmt.Sprintf("%s/repos/%s/pulls/%s", waitBrokerGitHubAPI, repo, cond.Arg),
			"__REPO__", repo,
			"__PR__", cond.Arg,
			"__MARKER__", markerPath,
			"__SEED__", seed,
			"__GH_BASE__", waitBrokerGitHubAPI,
		), nil
	case "story_done":
		taskURL := fmt.Sprintf("%s/internal/v1/automation/board/tasks/%s", strings.TrimSuffix(boardBaseURL, "/"), cond.Arg)
		return fill(waitGateTemplateStoryDone,
			"__BOARD_URL__", taskURL,
			"__BOARD_DEFAULT__", strings.TrimSuffix(boardBaseURL, "/"),
			"__SHORT_ID__", cond.Arg,
			"__MARKER__", markerPath,
			"__SEED__", seed,
		), nil
	}
	return "", fmt.Errorf("wait broker: unknown wait-condition primitive %q (the closed vocabulary is pr_merged|ci_green|story_done)", cond.Primitive)
}
