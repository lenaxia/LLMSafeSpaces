// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The matcher model ported VERBATIM from the decompiled opencode 1.18.15
// permission engine (the precedence probe the ruling mandated before
// shipping):
//
//   - fromConfig: rules in Object.entries order (JSON key order; Go's
//     encoding/json emits map keys byte-sorted — so ALPHABETICAL order
//     is our rendered precedence).
//   - evaluate: findLast — the LAST matching rule wins; no match → ask.
//   - match (the Glob helper): pattern → regex: backslash-normalize,
//     escape metachars, '*' → '.*', '?' → '.'.
//
// This port is the load-bearing pin: if a harness bump changes any leg
// (ordering, findLast, glob translation), these tests fail before any
// production behavior drifts.

type tierRule struct{ pattern, action string }

func tierMatch(resource, pattern string) bool {
	p := strings.ReplaceAll(pattern, "\\", "/")
	p = regexp.QuoteMeta(p)
	p = strings.ReplaceAll(p, `\*`, `.*`)
	p = strings.ReplaceAll(p, `\?`, `.`)
	return regexp.MustCompile(`^` + p + `$`).MatchString(strings.ReplaceAll(resource, "\\", "/"))
}

// tierEvaluate renders the tier map the way the writer does — the
// operator's allowed-dirs merged FIRST, the tier floor applied LAST
// (collisions resolve to the tier) — with Go map marshal = byte-sorted
// keys, then evaluates findLast: the model the live harness applies to
// the rendered config.
func tierEvaluate(resource string, allowedDirs map[string]string) string {
	merged := make(map[string]string, len(platformPermissionTiers)+len(allowedDirs))
	for k, v := range allowedDirs {
		merged[k] = v
	}
	for k, v := range platformPermissionTiers {
		merged[k] = v // floor applied last: collisions resolve to the tier
	}
	rules := make([]tierRule, 0, len(merged))
	for _, k := range sortedKeys(merged) {
		rules = append(rules, tierRule{pattern: k, action: merged[k]})
	}
	var last *tierRule
	for i := range rules {
		if tierMatch(resource, rules[i].pattern) {
			last = &rules[i]
		}
	}
	if last == nil {
		return "ask"
	}
	return last.action
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestPermissionTier_PrecedenceModel pins the full verdict matrix under
// the decompiled matcher semantics. Every carve-out must sort AFTER the
// deny it carves; every deny must fire; the deliberate name-ask
// residuals must stay ask.
func TestPermissionTier_PrecedenceModel(t *testing.T) {
	matrix := []struct {
		resource, want, why string
	}{
		{"/tmp/build-dir/x", "allow", "pre-allow /tmp"},
		{"/home/sandbox/.cache/mise/x", "allow", "pre-allow .cache"},
		{"/home/sandbox/.config/opencode/x", "allow", "pre-allow .config"},
		{"/sys/fs/cgroup/memory.current", "allow", "cgroup carve-out beats the /sys deny (alphabetical: '*' < 'f')"},
		{"/sys/kernel/xxx", "deny", "rest of /sys stays denied"},
		{"/opencode/plugins/llmsafespaces-origin.js", "allow", "pre-allow the ro artifact mount"},
		{"/home/sandbox/.ssh", "deny", "credential name (bash-typed tripwire)"},
		{"/home/sandbox/.ssh/id_ed25519", "deny", "child typed through the name"},
		{"/home/sandbox/.git-credentials", "deny", "credential name"},
		{"/home/sandbox/.secrets", "deny", "credential name"},
		{"/home/sandbox/.local/opencode/auth.json", "deny", "credential name under the .local pre-allow (sorts after)"},
		{"/home/sandbox/projects/foo", "ask", "the home carve restores ambient ask"},
		{"/home/sandbox", "ask", "BARE home dir: the wildcardless ask key (without it the /home/* deny matches the bare path — pinned r0)"},
		{"/home", "ask", "BARE /home parent matches NO rule (^/home/.*$ needs the slash) — ambient; children under other users still deny"},
		{"/home/otheruser/x", "deny", "/home parent deny"},
		{"/etc/passwd", "deny", "deny /etc"},
		{"/proc/self/status", "deny", "deny /proc"},
		{"/dev/shm/x", "deny", "deny /dev"},
		{"/sandbox-runtime/rt/secrets", "deny", "resolved target (canonical read-tool matching)"},
		{"/sandbox-runtime/rt/secrets/github", "deny", "resolved target children"},
		{"/sandbox-runtime/rt/auth.json", "deny", "resolved auth store"},
		{"/sandbox-runtime/rt/ssh/id_ed25519", "ask", "DELIBERATE: name-ask only — git/ssh tooling reads out-of-band, ungated"},
		{"/sandbox-runtime/rt/git-credentials", "ask", "DELIBERATE: same class as rt/ssh"},
		{"/sandbox-cfg/secrets.json", "ask", "ambient ask tier"},
		{"/agentd-config/allowed-dirs.json", "ask", "ambient ask tier"},
		{"/var/lib/x", "deny", "deny /var"},
		{"/root/.ssh/x", "deny", "deny /root"},
		{"/run/secrets/x", "deny", "deny /run"},
	}
	for _, tc := range matrix {
		t.Run(tc.resource, func(t *testing.T) {
			assert.Equal(t, tc.want, tierEvaluate(tc.resource, nil), tc.why)
		})
	}
}

// TestPermissionTier_AlphabeticalCarveOrdering pins the mechanism itself:
// every carve-out key must sort AFTER the deny key it carves — the
// findLast matcher makes this ordering the entire enforcement story.
func TestPermissionTier_AlphabeticalCarveOrdering(t *testing.T) {
	carves := []struct{ carve, deny string }{
		{"/home/sandbox/*", "/home/*"},
		{"/home/sandbox", "/home/*"},
		{"/sys/fs/cgroup/*", "/sys/*"},
		{"/home/sandbox/.cache/*", "/home/sandbox/*"},
		{"/home/sandbox/.ssh", "/home/sandbox/*"},
		{"/home/sandbox/.local/opencode/auth.json", "/home/sandbox/.local/*"},
	}
	for _, c := range carves {
		require.Contains(t, platformPermissionTiers, c.carve)
		require.Contains(t, platformPermissionTiers, c.deny)
		assert.Less(t, c.deny, c.carve, "carve %q must sort AFTER deny %q (findLast: last match wins)", c.carve, c.deny)
	}
}

// TestPermissionTier_AllowedDirsCannotReopenDeny: the writer applies the
// tier floor AFTER the operator's allowed-dirs merge, so an operator
// allow colliding with a tier deny key cannot reopen the boundary
// (fat-finger defense; the floor is the platform's, allowedDirs the
// operator's).
func TestPermissionTier_AllowedDirsCannotReopenDeny(t *testing.T) {
	assert.Equal(t, "deny", tierEvaluate("/etc/passwd", map[string]string{"/etc/*": "allow"}),
		"a colliding operator allow must not reopen a tier deny (floor applied last)")
	assert.Equal(t, "deny", tierEvaluate("/sys/kernel/x", map[string]string{"/sys/*": "allow"}))
}

// TestPermissionTier_CgroupMountReadOnly — the amended tier ruling's
// readiness assertion: the "/sys/fs/cgroup/*" allow is safe ONLY
// because the cgroup2 mount is kernel-read-only (a runtime default,
// NOT our pod spec — nothing in the chart enforces it). This pin
// asserts the environmental invariant the tier map's safety argument
// rests on — but ONLY where the allow is actually trusted: inside the
// LLMSafeSpaces sandbox (marked by /sandbox-runtime). Other
// environments skip by design — their cgroup mount state is irrelevant
// (a GitHub runner mounts /sys/fs/cgroup rw, and that is fine: the
// floor it would "protect" there is never rendered into a live pod).
func TestPermissionTier_CgroupMountReadOnly(t *testing.T) {
	if _, err := os.Stat("/sandbox-runtime"); err != nil {
		t.Skip("not the LLMSafeSpaces sandbox (no /sandbox-runtime) — the cgroup allow is only trusted inside the sandbox, so the ro invariant is not asserted here")
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	require.NoError(t, err, "the sandbox always exposes /proc/self/mountinfo")
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		// mountinfo: id parent major:minor root MOUNTPOINT OPTIONS...
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if fields[4] != "/sys/fs/cgroup" {
			continue
		}
		found = true
		opts := strings.Split(fields[5], ",")
		ro := false
		for _, o := range opts {
			if o == "ro" {
				ro = true
			}
		}
		if !ro {
			t.Fatalf("/sys/fs/cgroup is mounted rw (%s) inside the sandbox — the tier map's cgroup allow is UNSAFE here; the kernel-ro invariant is violated", fields[5])
		}
	}
	require.True(t, found, "the sandbox must mount /sys/fs/cgroup (the tier pre-allow targets it)")
}
