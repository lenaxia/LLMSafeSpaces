// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package validation

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// EnvVarNamePattern is the regex pattern (as a string) that POSIX-
// portable env-var names must match: a letter or underscore, followed
// by letters, digits, or underscores. Bash, sh, and every common
// interpreter accept this shape. Matches the existing regex in
// pkg/agentd/secrets/secrets.go:203 — single source of truth lives here.
const EnvVarNamePattern = `^[A-Za-z_][A-Za-z0-9_]*$`

// EnvVarNameRE is the compiled regex for EnvVarNamePattern.
var EnvVarNameRE = regexp.MustCompile(EnvVarNamePattern)

// EnvVarNameMaxLength is the maximum length of a workspace env-var name.
// Matches the existing cap in pkg/agentd/secrets/secrets.go:226 (256).
const EnvVarNameMaxLength = 256

// blockedEnvVarNames is the G37 blocklist of env-var names that
// influence dynamic linking, library search paths, shell behavior,
// interpreter startup, or process execution. Setting one of these via
// the workspace env-secret mechanism would let a user compromise every
// process spawned in the workspace pod (agentd, opencode, mise-installed
// interpreters) — a container-escape-equivalent in practice because the
// pod's single UID shares the same trust boundary.
//
// Membership is curated, not exhaustive. The bar for inclusion: the
// variable's effect is (a) documented in its runtime's manual as
// influencing code loading, code execution, command lookup, or trust
// anchors AND (b) not a variable that users legitimately need to set
// (e.g. LANG, LC_ALL, TZ are NOT here — they are locale/timezone prefs
// and don't execute code).
//
// Sources:
//   - ld.so(8) — LD_*: dynamic linker
//   - dyld(1) (macOS) — DYLD_*: same threat on arm64 macOS runners
//   - bash(1) — BASH_ENV, ENV, SHELLOPTS, IFS, etc.
//   - python(1) — PYTHONPATH, PYTHONSTARTUP, PYTHONHOME, etc.
//   - node(1) — NODE_OPTIONS, NODE_PATH, NODE_EXTRA_CA_CERTS
//   - ruby(1) — RUBYOPT, RUBYLIB
//   - perl(1) — PERL5OPT, PERL5LIB, PERLLIB
//   - java — JAVA_TOOL_OPTIONS, _JAVA_OPTIONS
//   - glibc — LOCPATH
//   - POSIX env — PATH, HOME, TMPDIR, PS4
var blockedEnvVarNames = map[string]struct{}{
	// Dynamic linker (glibc ld.so)
	"LD_PRELOAD":      {},
	"LD_LIBRARY_PATH": {},
	"LD_BIND_NOW":     {},
	"LD_AUDIT":        {},
	"LD_DEBUG":        {},
	// Dynamic linker (macOS dyld)
	"DYLD_INSERT_LIBRARIES": {},
	"DYLD_LIBRARY_PATH":     {},
	// Shell execution environment
	"BASH_ENV":  {}, // bash sources this file on every non-interactive invocation
	"ENV":       {}, // POSIX sh equivalent of BASH_ENV
	"SHELLOPTS": {}, // sets shell options without -o
	"PS4":       {}, // xtrace output — can run subshells via $(...)
	"IFS":       {}, // word-split separator — classic injection vector
	"TMPDIR":    {}, // redirect temp file writes (mktemp, sort, etc.)
	"PATH":      {}, // redirect every command lookup (opencode, git, ssh, etc.)
	"HOME":      {}, // ~/.ssh, ~/.gitconfig, ~/.config resolution
	// Python startup / module search
	"PYTHONPATH":     {}, // sys.path prepend — import attacker module
	"PYTHONSTARTUP":  {}, // exec'd on interactive boot
	"PYTHONHOME":     {}, // redirect stdlib — load attacker's os.py
	"PYTHONUSERBASE": {}, // user site-packages root
	// Node startup
	"NODE_OPTIONS":        {}, // CLI flags via env (e.g. --require)
	"NODE_PATH":           {}, // module search path prepend
	"NODE_EXTRA_CA_CERTS": {}, // trust-anchor injection (mitm)
	// Ruby
	"RUBYOPT": {}, // CLI flags via env
	"RUBYLIB": {}, // load path prepend
	// Perl
	"PERL5OPT": {}, // CLI flags via env
	"PERL5LIB": {}, // load path
	"PERLLIB":  {}, // load path
	// Java
	"JAVA_TOOL_OPTIONS": {}, // JVM CLI flags via env
	"_JAVA_OPTIONS":     {}, // JVM CLI flags via env (Oracle)
	// Locale data (glibc locale-loading vulns historically)
	"LOCPATH": {}, // locale data path — corrupt glibc locale parsing
}

// ErrEnvVarNameBlocked is returned by ValidateEnvVarName when the name
// is on the dangerous-names blocklist. The error message names the
// offending variable so the user knows it was an intentional rejection
// rather than a regex miss.
var ErrEnvVarNameBlocked = errors.New("env var name is on the dangerous-names blocklist")

// ValidateEnvVarName validates a workspace env-var name against three
// rules:
//
//  1. POSIX-portable shape: [A-Za-z_][A-Za-z0-9_]* (case-sensitive on
//     the regex; the dangerous-name check below is case-insensitive).
//  2. Length ≤ 256.
//  3. Not on the dangerous-names blocklist (G37) — compared case-
//     insensitively because ld.so and several interpreters accept the
//     lowercase form on some platforms.
//
// Shared between the API layer (api/internal/handlers/workspace_env.go)
// and the in-pod materializer (pkg/agentd/secrets/secrets.go) so the two
// layers cannot drift.
func ValidateEnvVarName(s string) error {
	if s == "" {
		return errors.New("env var name is empty")
	}
	if len(s) > EnvVarNameMaxLength {
		return fmt.Errorf("env var name length %d exceeds maximum of %d", len(s), EnvVarNameMaxLength)
	}
	if !EnvVarNameRE.MatchString(s) {
		return fmt.Errorf("env var name %q does not match POSIX rules (%s)", s, EnvVarNamePattern)
	}
	if _, blocked := blockedEnvVarNames[strings.ToUpper(s)]; blocked {
		return fmt.Errorf("env var name %q is blocked: %w", s, ErrEnvVarNameBlocked)
	}
	return nil
}

// IsBlockedEnvVarName reports whether name would be rejected by
// ValidateEnvVarName solely on the dangerous-names blocklist. Useful
// for callers that want to surface the blocklist reason separately
// from the regex/length reasons.
func IsBlockedEnvVarName(name string) bool {
	_, blocked := blockedEnvVarNames[strings.ToUpper(name)]
	return blocked
}
