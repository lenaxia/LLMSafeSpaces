// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"regexp"
	"strings"
)

// permissiontiers.go — the platform-baked external_directory permission
// floor (the 2026-09-19 ruling, transcribed in
// design/0060_2026-09-21_credential-plane-closure.md row A2, the #825
// credential-plane lineage): the pre-allow set stops the recurring
// folder prompts; the deny tier is the security boundary; ask stays
// ambient (opencode's built-in external_directory default is ask, so
// no rule is needed for the ask tier).
//
// PRECEDENCE MODEL (pinned by TestPermissionTier_PrecedenceModel, ported
// from the decompiled 1.18.15 matcher): rules evaluate findLast — the
// LAST matching rule wins — in rule-list order, which for a rendered
// JSON object is KEY ORDER. Go's encoding/json marshals map keys in
// BYTE-SORTED order, so precedence between our patterns is alphabetical
// and deterministic. The tier set is constructed so that every carve-out
// sorts AFTER the deny it carves:
//
//	"/home/*" (deny)      < "/home/sandbox/*" (ask)   — '*' (0x2A) < 's'
//	"/sys/*"  (deny)      < "/sys/fs/cgroup/*" (allow)
//	"/home/sandbox/*" ask < "/home/sandbox/.ssh…" denies — '*' < '.'
//
// TIER MAP (per the 2026-09-19 ruling, amended):
//
//   - PRE-ALLOW (kernel-ro verified live for the artifact/cgroup paths —
//     the mount is the boundary; the rule only stops prompts):
//     /tmp, /home/sandbox/.cache, /home/sandbox/.local,
//     /home/sandbox/.config, /sys/fs/cgroup (carved out of the /sys
//     deny), /opencode.
//   - ASK (ambient, no rule): everything not matched — /sandbox-cfg,
//     /agentd-config, /sandbox-runtime, /home/sandbox itself, and the
//     credential SYMLINK NAMES rt/ssh + rt/git-credentials (git/ssh
//     tooling consumes them out-of-band, ungated by permissions; the
//     ambient ask catches a curious agent typing the name).
//   - HARD-DENY: /etc /proc /sys /dev /run /root /var /home (parent);
//     the home credential names /home/sandbox/.ssh, .secrets,
//     .git-credentials, .local/opencode/auth.json (bash-typed
//     tripwire — matching for parsed bash commands uses the TYPED
//     path); and the RESOLVED secret targets /sandbox-runtime/rt/secrets
//     and rt/auth.json (file tools resolve symlinks to canonical paths
//     before asking, so only the resolved target denies read-tool
//     access).
//
// Design 0060 Part D transcribes the pre-allow set as
// "caches/config/workspace" — there is deliberately NO /workspace
// entry: /workspace is the opencode project root, which
// external_directory does not govern at all (residual 1 below); the
// workspace subsumes into that exemption, not into a tier.
//
// RESIDUALS (documented, not solved — PR + ruling):
//  1. bash-typed reads of PROJECT-RELATIVE credential paths
//     (/workspace/.local/opencode/auth.json) are inside the workspace
//     root: external_directory does not govern them. Same trust class
//     as the tokens already in the agent's env.
//  2. No read-only granularity exists in the permission model: one
//     action per directory. The ro mounts carry the real boundary for
//     the two read-only pre-allows (assert cgroup2 ro before trusting —
//     runtime default, not our pod spec; /opencode ro is ours).

// platformPermissionTiers is the external_directory floor the writer
// bakes into the TOP-LEVEL permission key (the LIVE shape on the
// pinned 1.18.15 — mode.permissions is inert, the corpse the ruling surfaced).
// Map keys are exactly the rendered JSON keys (marshal-sorted =
// precedence, see the file comment). Values are the opencode actions:
// "allow", "ask", "deny".
//
// The bare-directory entries (no trailing /*) govern the symlink NAME
// itself (e.g. an `ls /home/sandbox/.ssh`); the /* variants govern
// children typed through the name.
var platformPermissionTiers = map[string]string{
	// --- deny tier (parents first alphabetically; carves below) ---
	"/dev/*":  "deny",
	"/etc/*":  "deny",
	"/home/*": "deny",
	"/proc/*": "deny",
	"/root/*": "deny",
	"/run/*":  "deny",
	"/sys/*":  "deny",
	"/var/*":  "deny",

	// --- /home carve: the workspace home back to ambient ask. BOTH keys
	// are needed: the /* variant governs children; the bare key governs
	// the directory itself (the ported matcher treats a wildcardless
	// pattern as an exact match, so ^/home/.*$ would otherwise deny the
	// bare path with the /home/* deny — r0 finding 3). The bare key
	// sorts after "/home/*" ('*' 0x2A < 's') — the ask wins.
	"/home/sandbox":   "ask",
	"/home/sandbox/*": "ask",

	// --- pre-allows under the home carve (each sorts after the ask) ---
	"/home/sandbox/.cache/*":  "allow",
	"/home/sandbox/.config/*": "allow",
	"/home/sandbox/.local/*":  "allow",

	// --- credential-name denies (bash-typed tripwire; sort after the
	//     .local/.cache/.config allows they live alongside) ---
	"/home/sandbox/.git-credentials":          "deny",
	"/home/sandbox/.git-credentials/*":        "deny",
	"/home/sandbox/.local/opencode/auth.json": "deny",
	"/home/sandbox/.secrets":                  "deny",
	"/home/sandbox/.secrets/*":                "deny",
	"/home/sandbox/.ssh":                      "deny",
	"/home/sandbox/.ssh/*":                    "deny",

	// --- /sys carve-out: cgroup2 is ro at the kernel level (runtime
	//     default — assert before trusting; see the residuals). BOTH
	//     keys, same bare-path rule as the home carve: without the bare
	//     key the directory itself denies (^/sys/.*$ matches it, the
	//     /* variant does not) — r4 finding 2.
	"/sys/fs/cgroup":   "allow",
	"/sys/fs/cgroup/*": "allow",

	// --- standalone pre-allows ---
	"/opencode/*": "allow",
	"/tmp/*":      "allow",

	// --- resolved secret targets (canonical matching for file tools).
	//     rt/ssh and rt/git-credentials are deliberately ABSENT: ambient
	//     ask on the name only; the git/ssh tooling reads them
	//     out-of-band, ungated by the permission model. ---
	"/sandbox-runtime/rt/auth.json": "deny",
	"/sandbox-runtime/rt/secrets":   "deny",
	"/sandbox-runtime/rt/secrets/*": "deny",
}

// tierRule is one rendered external_directory rule (pattern, action).
type tierRule struct{ pattern, action string }

// tierMatch is the ported 1.18.15 external_directory matcher (moved
// verbatim from the test-side model into production when the render
// gained floor dominance — r4): backslashes normalize to slashes, the
// pattern is regex-quoted with \* → .* and \? → ., anchored both ends.
// The precedence tests exercise THIS function — the model and the
// enforcement share one source.
func tierMatch(resource, pattern string) bool {
	p := strings.ReplaceAll(pattern, "\\", "/")
	p = regexp.QuoteMeta(p)
	p = strings.ReplaceAll(p, `\*`, `.*`)
	p = strings.ReplaceAll(p, `\?`, `.`)
	return regexp.MustCompile(`^` + p + `$`).MatchString(strings.ReplaceAll(resource, "\\", "/"))
}

// allowReopensTierDeny — the floor-dominance filter (r4): reports
// whether an operator allow pattern can match ANY path a tier deny
// governs. Exact-key collision handling alone is insufficient: under
// findLast, a deeper operator pattern ("/etc/latency/*") sorts AFTER
// the deny it carves ("/etc/*") and would WIN — reopening the deny.
// The render DROPS any operator allow that reopens; the tier map is
// never weakened by operator input.
//
// Soundness: every tier deny is an exact path or a trailing-"/\*"
// prefix glob. An exact deny D intersects pattern p iff p matches D
// (single concrete string — complete check). A deny "X/\*" intersects
// p iff p can match some string beginning "X/": with L the literal
// prefix of p before its FIRST WILDCARD of either kind ("\*" or "?",
// both of which the ported matcher compiles to match-any runes — r5
// closed the ? gap: a leading-? pattern's bytes are NOT literal), that
// holds iff L is empty, L is itself under "X/", or "X/" extends L
// (p's wildcard can absorb the rest of X plus the slash). That branch
// is a live overlap or a CONSERVATIVE OVER-DROP (e.g. "/et?" matches
// only the bare "/etc", which no deny governs — ambient ask — but
// dropping the pattern is the safe direction); it is never an
// under-drop. Patterns outside every deny keep their allow.
func allowReopensTierDeny(pattern string) bool {
	// Normalize EXACTLY like the matcher (r6): tierMatch maps \ → /
	// before matching, so a backslashed pattern ("\etc/*") IS the
	// deny-root pattern to the engine while its raw bytes share no
	// prefix with the deny key — and \ (0x5C) sorts after every
	// /-prefixed key, making the reopen live under findLast. The filter
	// and the matcher must agree on the string they analyze.
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	wild := strings.IndexAny(pattern, "*?")
	litPrefix := pattern
	if wild >= 0 {
		litPrefix = pattern[:wild]
	}
	for k, v := range platformPermissionTiers {
		if v != "deny" {
			continue
		}
		if !strings.Contains(k, "*") {
			if tierMatch(k, pattern) {
				return true
			}
			continue
		}
		denyRoot := strings.TrimSuffix(k, "*") // "X/"
		if litPrefix == "" || strings.HasPrefix(litPrefix, denyRoot) || strings.HasPrefix(denyRoot, litPrefix) {
			return true
		}
	}
	return false
}
