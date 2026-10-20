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
//	"/home/*" (deny)      < "/home/sandbox/*" (allow #1759) — '*' (0x2A) < 's'
//	"/sys/*"  (deny)      < "/sys/fs/cgroup/*" (allow)
//	"/home/sandbox/*" allow < "/home/sandbox/.ssh…" denies — '*' (0x2A) < '.' (0x2E)
//
// TIER MAP (per the 2026-09-19 ruling, amended 2026-10-19 by #1759):
//
//   - DEFAULT-ALLOW (#1759, the PVC mount points): /workspace, /tmp,
//     /home/sandbox — the three subPath mounts of the one workspace
//     PVC are the agent's own filesystem by design; read-write, no
//     prompts. Every credential deny under the home root still wins
//     (each sorts after "/home/sandbox/*": '*' 0x2A < '.' 0x2E). The
//     pre-#1759 subpath pre-allows (.cache/.config/.local) are
//     superseded by the root allow and removed — dead rules in a
//     security floor are noise.
//   - PRE-ALLOW (kernel-ro verified live for the artifact/cgroup paths —
//     the mount is the boundary; the rule only stops prompts):
//     /sys/fs/cgroup (carved out of the /sys deny), /opencode.
//   - ASK (ambient, no rule): everything not matched — /sandbox-cfg,
//     /agentd-config, /sandbox-runtime, and the credential SYMLINK
//     NAMES rt/ssh + rt/git-credentials (git/ssh tooling consumes them
//     out-of-band, ungated by permissions; the ambient ask catches a
//     curious agent typing the name).
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
// "caches/config/workspace". #1759 makes the workspace/home/tmp roots
// first-class allow keys: the harness still exempts the PROJECT ROOT
// from external_directory governance (residual 1) — the /workspace
// entries govern bash-typed and tool-resolved paths in the merged
// permission model and keep the config self-describing about which
// filesystem is the agent's.
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
//  3. ENGINE DENY GRAMMAR (observed live on the pinned 1.18.15 during
//     the #1759 investigation — the 18-probe grammar set recorded with
//     the closed #1764, plus the two salvage experiments: the env
//     mutation boot and the children-deny probe in (d); the ported
//     model above is faithful for "/*" rules
//     and optimistic elsewhere): only "X/*"-suffixed patterns ever
//     fire a deny. Exact keys ("X") and terminal-star keys ("X*") load
//     into the engine's rule list but never match for a DENY, while
//     both shapes DO fire for an ALLOW. Consequences, recorded so
//     nobody re-derives them:
//     (a) The bare credential names below are intent/model rules:
//     live, a typed `ls /home/sandbox/.ssh` executes under the root
//     allow (pre-#1759 the home carve's ask key blocked it headless);
//     the CONTENTS stay hard-denied by the "/*" variants (proven live
//     — the deny-preservation leg in the integration corpus).
//     (b) A single FILE under an allowed glob has no live-effective
//     deny shape: e.g. /workspace/.local/opencode/auth.json under
//     "/workspace/*" is ALLOWED wherever external_directory governs it
//     (proven with a foreign project root — the integration legs boot
//     opencode outside /workspace); on the default pod residual 1's
//     project-root exemption is the governing claim. This exposure is
//     the config allow's own trade (#1759's owner spec). The closed
//     #1764 asserted an engine auth-store self-allow that was never
//     observed (no rule dump ever showed one) and concluded from it
//     that a children deny could not protect the auth path — the
//     conclusion is falsified by (d) below; its shipped body still
//     carries the claim uncorrected, and THIS note is the in-repo
//     correction pointer.
//     (c) The engine APPENDS self-allows after the config rules
//     (observed shapes: the boot's tool-output directory,
//     "/tmp/opencode/*"); appended rules beat config under findLast.
//     (d) A live-effective protective shape DOES exist for the
//     directory case: a children deny "/workspace/.local/opencode/*"
//     byte-sorts after the root allow and BITES live (probed
//     2026-10-20: a bash cat of the auth path denied with our rule
//     quoted in the engine's dump). Adding it to the floor would also
//     deny opencode.db-class session state to agent tools — an owner
//     decision, deliberately NOT taken in this documentation change.
//     UPSTREAM ASKS: (i) honor exact/terminal-star denies; (ii) an
//     exclude shape that beats allow-globs; (iii) document which
//     paths the engine self-allows (supersedes #1764's ask iii,
//     "stop self-allowing the auth store", per the corrected
//     attribution above).

// platformPermissionTiers is the external_directory floor the writer
// bakes into the TOP-LEVEL permission key (the LIVE shape on the
// pinned 1.18.15 — mode.permissions is inert, the corpse the ruling surfaced).
// Map keys are exactly the rendered JSON keys (marshal-sorted =
// precedence, see the file comment). Values are the opencode actions:
// "allow", "ask", "deny".
//
// The bare-directory entries (no trailing /*) document the symlink
// NAME itself in the MODEL (e.g. an `ls /home/sandbox/.ssh`); the /*
// variants govern children typed through the name. On the live engine
// the bare keys do not fire for denies — residual 3(a): the children
// globs are what bite.
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

	// --- /home carve (#1759): the home PVC root is DEFAULT-ALLOW. BOTH
	// keys are needed: the /* variant governs children; the bare key
	// governs the directory itself (the ported matcher treats a
	// wildcardless pattern as an exact match, so ^/home/.*$ would
	// otherwise deny the bare path with the /home/* deny — r0 finding
	// 3). The bare key sorts after "/home/*" ('*' 0x2A < 's') — the
	// allow wins over the parent deny.
	"/home/sandbox":   "allow",
	"/home/sandbox/*": "allow",

	// --- credential-name denies (bash-typed tripwire; each sorts after
	//     the "/home/sandbox/*" allow: '*' 0x2A < '.' 0x2E — the deny
	//     is the LAST matching rule for every credential path) ---
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

	// --- #1759: the tmp PVC mount root, read-write by default. BOTH
	//     keys (bare + children), same rule as the other roots. ---
	"/tmp":   "allow",
	"/tmp/*": "allow",

	// --- #1759: the workspace PVC mount root, read-write by default.
	//     BOTH keys. The harness additionally exempts the project root
	//     from external_directory governance (residual 1) — these
	//     entries govern bash-typed/tool-resolved paths in the merged
	//     permission model. ---
	"/workspace":   "allow",
	"/workspace/*": "allow",

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
