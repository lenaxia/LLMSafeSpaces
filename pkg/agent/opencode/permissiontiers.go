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
//	"/home/*" (deny)       < "/home/sandbox/*" (allow)  — '*' (0x2A) < 's'
//	"/sys/*"  (deny)       < "/sys/fs/cgroup/*" (allow)
//	"/home/sandbox/*" allow < "/home/sandbox/.ssh…" denies — '*' < '.'
//
// TIER MAP (per the 2026-09-19 ruling, amended by #1759 — the three
// PVC mount points are allow):
//
//   - ALLOW (the #1759 PVC roots — bare key AND children glob for each):
//     /workspace, /tmp, /home/sandbox. /tmp children and the home
//     .cache/.config/.config pre-allows predate #1759; the ruling made
//     all three ROOTS allow (bare keys included — the bare path is a
//     child of its parent's deny or matches no rule otherwise).
//     /workspace is belt-and-braces under the project-root exemption
//     (see residual 1); the auth twin deny keeps the tripwire coherent
//     wherever the rules DO govern it (a foreign project root).
//   - PRE-ALLOW (kernel-ro verified live for the artifact/cgroup paths —
//     the mount is the boundary; the rule only stops prompts):
//     /home/sandbox/.cache, /home/sandbox/.local,
//     /home/sandbox/.config, /sys/fs/cgroup (carved out of the /sys
//     deny), /opencode.
//   - ASK (ambient, no rule): everything not matched — /sandbox-cfg,
//     /agentd-config, /sandbox-runtime, and the credential SYMLINK
//     NAMES rt/ssh + rt/git-credentials (git/ssh tooling consumes them
//     out-of-band, ungated by permissions; the ambient ask catches a
//     curious agent typing the name).
//   - HARD-DENY: /etc /proc /sys /dev /run /root /var /home (parent);
//     the home credential names /home/sandbox/.ssh, .secrets,
//     .git-credentials, .local/opencode/auth.json and the /workspace
//     twin .local/opencode/auth.json (bash-typed tripwire — matching
//     for parsed bash commands uses the TYPED path); and the RESOLVED
//     secret targets /sandbox-runtime/rt/secrets and rt/auth.json (file
//     tools resolve symlinks to canonical paths before asking, so only
//     the resolved target denies read-tool access).
//
// Design 0060 Part D transcribes the pre-allow set as
// "caches/config/workspace". #1759 (owner, 2026-10-19) superseded the
// no-/workspace-entry reading: the root now carries an explicit allow.
// In the live pod /workspace IS the opencode project root (WORKDIR,
// runtimes/base/Dockerfile) and external_directory does not govern it
// there — the allow is inert belt-and-braces; it governs (and the twin
// deny bites) whenever the project root differs.
//
// RESIDUALS (documented, not solved — PR + ruling):
//  1. bash-typed reads of PROJECT-RELATIVE credential paths
//     (/workspace/.local/opencode/auth.json) are inside the workspace
//     root: external_directory does not govern them when /workspace is
//     the project root. The twin deny covers every configuration where
//     the path IS governed. Same trust class as the tokens already in
//     the agent's env.
//  2. No read-only granularity exists in the permission model: one
//     action per directory. The ro mounts carry the real boundary for
//     the two read-only pre-allows (assert cgroup2 ro before trusting —
//     runtime default, not our pod spec; /opencode ro is ours).
//  3. (#1759, observed live on 1.18.15 — 18-probe evidence set) THE
//     LIVE DENY GRAMMAR: only "/*"-suffixed (slash-star) patterns ever
//     fire a deny. Per-shape matrix: "X/*" — allow fires, deny fires
//     (the children globs are what has always protected .ssh/*,
//     .secrets/*, rt/secrets/*, /etc/*); "X" exact — allow fires
//     (bare "/tmp" leg), deny NEVER fires (loaded into the engine's
//     rule list, never matched: ls of the bare .ssh name executed
//     under the #1759 root allow, both auth.json twins' cats
//     executed, cat of rt/auth.json fell to ambient ask); "X*"
//     terminal-star — allow fires, deny never fires (synthetic-boot
//     probes both directions). CONSEQUENCE: a bare NAME — file or
//     directory — cannot be denied on this engine under any pattern
//     shape; the exact deny keys above (incl. both auth.json twins and
//     the resolved-target rt/auth.json) are intent documentation held
//     by the MODEL (tierMatch + the matrix), transparent at
//     evaluation, correct the moment an engine honors them. The bare
//     credential NAMES moved ask→allow as a side effect of the root
//     allow: pre-#1759 "/home/sandbox/.ssh" matched the home-carve
//     ASK key (blocked in headless turns); post-#1759 it matches
//     "/home/sandbox/*" allow, so a bare-name listing executes —
//     name/metadata exposure only, since the CONTENTS stay
//     hard-denied by the children globs (pinned live). The
//     engine also APPENDS its own self-allows (tool-output dir,
//     /tmp/opencode/*, its XDG-resolved auth store) AFTER the config
//     rules — appended rules beat config under findLast — so with the
//     controller-injected XDG_DATA_HOME=/workspace/.local a bash cat
//     of /workspace/.local/opencode/auth.json is engine-self-allowed
//     regardless of any rule (PRE-EXISTING; and with the #1759
//     "/workspace/*" allow the typed path is allowed by config too —
//     the ask-wall on that one path is traded away by the issue's
//     own spec; structural secrecy stays US-35.7's tmpfs+wipe and
//     #821's egress bounds — the tiers are the alarm, not the
//     closure, design 0060 Part D; the pre-existing
//     "/home/sandbox/.config/*" allow exposes gh tool-auth the same
//     way). UPSTREAM ASKS: (i) honor exact/terminal-star denies;
//     (ii) an exclude shape that beats allow-globs; (iii) stop
//     self-allowing the auth store. A grammar-conformant PARTIAL
//     mitigation exists for a future engine: a children deny
//     ("/workspace/.local/opencode/*") would bite for
//     opencode.db-class paths under the root allow, but cannot
//     protect auth.json itself while the appended self-allow stands
//     (pairs with ask iii) — deliberately NOT added in this change.

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

	// --- /home carve: the workspace home PVC root is ALLOW (#1759 —
	// supersedes the ruling's home-carve ask). BOTH keys are needed:
	// the /* variant governs children; the bare key governs the
	// directory itself (the ported matcher treats a wildcardless
	// pattern as an exact match, and the /home/* deny matches the bare
	// path — r0 finding 3). The bare key sorts after "/home/*"
	// ('*' 0x2A < 's') — the allow wins; every credential deny below
	// sorts after "/home/sandbox/*" ('*' 0x2A < '.') and keeps winning.
	"/home/sandbox":   "allow",
	"/home/sandbox/*": "allow",

	// --- pre-allows under the home carve (each sorts after the ask) ---
	"/home/sandbox/.cache/*":  "allow",
	"/home/sandbox/.config/*": "allow",
	"/home/sandbox/.local/*":  "allow",

	// --- credential-name denies (bash-typed tripwire; sort after the
	//     .local/.cache/.config allows they live alongside). These exact
	//     keys document intent and hold in the MODEL — on the live
	//     1.18.15 engine wildcardless denies never fire (residual 3);
	//     the children "/*" variants are what actually bites. ---
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

	// --- the #1759 PVC roots. /tmp children predate the ruling; the
	//     bare keys close the mount-point-itself gap (the bare path
	//     matched no rule before — ambient ask on the root dirs). ---
	"/tmp":       "allow",
	"/tmp/*":     "allow",
	"/workspace": "allow",

	// --- workspace PVC children + the XDG auth.json tripwire twin
	//     (authJSONPath per README). Intent/model key, same inert-on-live
	//     class as the home twin — residual 3. ---
	"/workspace/*":                         "allow",
	"/workspace/.local/opencode/auth.json": "deny",

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
// prefix glob — AND carries NO "?" bytes (the deny-shape premise pin
// enforces both test-side; a "?-bearing deny would under-approximate
// here: the exact branch feeds its bytes to tierMatch as a literal
// resource while the live matcher compiles them as any-char). An exact
// deny D intersects pattern p iff p matches D
// (single concrete string — complete check). A deny "X/\*" intersects
// p iff p can match some string beginning "X/": with L the literal
// prefix of p before its FIRST WILDCARD of either kind ("\*" or "?",
// both of which the ported matcher compiles to match-any runes — r5
// closed the ? gap: a leading-? pattern's bytes are NOT literal), that
// holds iff L is empty, L is itself under "X/", or "X/" extends L
// (p's wildcard can absorb the rest of X plus the slash). That branch
// is a live overlap or a CONSERVATIVE OVER-DROP (e.g. "/et?" matches
// only bare 4-char "/etX" paths — "/etc", "/etx", "/et/" — none of
// which any deny governs, but dropping the pattern is the safe
// direction); it is never an under-drop. Patterns outside every deny keep their allow.
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
