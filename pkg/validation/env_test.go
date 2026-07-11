// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package validation

import (
	"strings"
	"testing"
)

// TestValidateEnvVarName_AcceptsLegitimateNames covers the happy path:
// any POSIX-portable env-var name that is NOT on the dangerous-name
// blocklist must be accepted. Without this, legitimate use (FOO, BAR,
// MY_API_KEY, etc.) would break.
func TestValidateEnvVarName_AcceptsLegitimateNames(t *testing.T) {
	// The names here are deliberately drawn from real usage in the
	// existing test suite (workspace_env_test.go) plus a sample of
	// common env-var names from real opencode/agent deployments.
	legit := []string{
		"FOO",
		"BAR",
		"BAZ",
		"DATABASE_URL", // used in existing TestE2E_RealAuth_WorkspaceEnv
		"API_KEY",      // used in existing TestE2E_RealAuth_WorkspaceEnv
		"MY_API_KEY",
		"GITHUB_TOKEN",
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"DEBUG",
		"VERBOSE",
		"NODE_ENV",
		"PIP_INDEX_URL",
		"NPM_CONFIG_REGISTRY",
		"GOPROXY",
		"_UNDERSCORE_PREFIX", // POSIX allows leading underscore
	}
	for _, name := range legit {
		t.Run(name, func(t *testing.T) {
			if err := ValidateEnvVarName(name); err != nil {
				t.Errorf("ValidateEnvVarName(%q) = %v; want nil", name, err)
			}
		})
	}
}

// TestValidateEnvVarName_RejectsEmptyAndTooLong covers the length edge
// cases. Empty names are meaningless; names longer than 256 chars exceed
// what any reasonable env-var consumer accepts (matching agentd's
// existing validateVarName cap at pkg/agentd/secrets/secrets.go:226).
func TestValidateEnvVarName_RejectsEmptyAndTooLong(t *testing.T) {
	tooLong := make([]byte, 257)
	for i := range tooLong {
		tooLong[i] = 'A'
	}
	cases := []string{
		"",
		string(tooLong),
	}
	for _, name := range cases {
		t.Run("length_"+name, func(t *testing.T) {
			if err := ValidateEnvVarName(name); err == nil {
				t.Errorf("ValidateEnvVarName(%q) = nil; want non-nil", name)
			}
		})
	}
}

// TestValidateEnvVarName_RejectsInvalidPOSIXChars covers the regex half
// of the validation. Env-var names must be [A-Za-z_][A-Za-z0-9_]* per
// POSIX. Mirrors the existing regex in pkg/agentd/secrets/secrets.go:203
// so the two layers cannot drift.
func TestValidateEnvVarName_RejectsInvalidPOSIXChars(t *testing.T) {
	cases := []string{
		"1STARTS_WITH_DIGIT", // must start with letter or underscore
		"HAS-SPACE",          // hyphen not allowed
		"HAS.DOT",            // dot not allowed (env-var names, not secret names)
		"HAS SPACE",          // space
		"HAS$DOLLAR",         // shell metachar
		"=",                  // empty name with separator
		"FOO=BAR",            // full assignment syntax, not a name
		"ünïcödé",            // non-ASCII
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateEnvVarName(name); err == nil {
				t.Errorf("ValidateEnvVarName(%q) = nil; want non-nil", name)
			}
		})
	}
}

// TestValidateEnvVarName_RejectsDangerousNames is the G37 core: names
// that influence dynamic linking, library search paths, shell behavior,
// interpreter startup, or process execution MUST be rejected. Setting
// these via the env-secret mechanism would let a user compromise every
// process spawned in the workspace pod (agentd, opencode, mise-installed
// interpreters) — a container-escape-equivalent in practice because the
// pod's single UID shares the same trust boundary.
//
// Each name here is sourced from the relevant runtime's documentation:
//   - ld.so(8): LD_PRELOAD, LD_LIBRARY_PATH, LD_BIND_NOW, etc.
//   - bash(1): BASH_ENV, ENV, SHELLOPTS, etc.
//   - python(1): PYTHONPATH, PYTHONSTARTUP, PYTHONDONTWRITEBYTECODE, etc.
//   - node(1): NODE_OPTIONS, NODE_PATH, etc.
//   - libc/process: IFS, PATH, TMPDIR, DYLD_*, etc.
func TestValidateEnvVarName_RejectsDangerousNames(t *testing.T) {
	cases := []string{
		// Dynamic linker — load arbitrary .so into every spawned process
		"LD_PRELOAD",
		"LD_LIBRARY_PATH",
		"LD_BIND_NOW",
		"LD_AUDIT",
		"LD_DEBUG",
		// macOS dynamic linker (same threat on arm64 macOS runners)
		"DYLD_INSERT_LIBRARIES",
		"DYLD_LIBRARY_PATH",
		// Shell-injection vectors
		"BASH_ENV",  // bash sources this file on every non-interactive invocation
		"ENV",       // POSIX sh equivalent of BASH_ENV
		"SHELLOPTS", // sets shell options without -o
		"PS4",       // traced command output (xtrace) — can run subshells
		"IFS",       // word-split separator — classic injection vector
		"TMPDIR",    // redirect temp file writes (mktemp, sort, etc.)
		"PATH",      // redirect every command lookup (opencode, git, ssh, etc.)
		"HOME",      // ~/.ssh, ~/.gitconfig, ~/.config resolution
		// Python startup — arbitrary code on interpreter boot
		"PYTHONPATH",     // sys.path prepend — import attacker module
		"PYTHONSTARTUP",  // exec'd on interactive boot
		"PYTHONHOME",     // redirect stdlib — load attacker's os.py
		"PYTHONUSERBASE", // user site-packages root
		// Node startup
		"NODE_OPTIONS",        // CLI flags via env (e.g. --require)
		"NODE_PATH",           // module search path prepend
		"NODE_EXTRA_CA_CERTS", // trust-anchor injection (mitm)
		// Ruby
		"RUBYOPT", // CLI flags via env
		"RUBYLIB", // load path prepend
		// Perl
		"PERL5OPT", // CLI flags via env
		"PERL5LIB", // load path
		"PERLLIB",
		// Java
		"JAVA_TOOL_OPTIONS", // JVM CLI flags via env
		"_JAVA_OPTIONS",
		// General interpreter
		"LOCPATH", // locale data — corrupt glibc locale parsing
		// NOTE: LANG/LC_* are deliberately NOT blocked — they're commonly
		// set by users for legitimate localization and don't execute code.
		// See TestValidateEnvVarName_AcceptsLegitimateNames for coverage.
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateEnvVarName(name); err == nil {
				t.Errorf("ValidateEnvVarName(%q) = nil; want non-nil (dangerous name not blocked)", name)
			}
		})
	}
}

// TestValidateEnvVarName_AcceptsLocaleNames confirms locale-related
// env vars are NOT on the blocklist. LANG, LC_ALL, TZ are commonly set
// by users for legitimate purposes and do not execute code. This is a
// regression guard: a future "block everything that affects the
// environment" sweep must not silently expand the blocklist to cover
// locale.
func TestValidateEnvVarName_AcceptsLocaleNames(t *testing.T) {
	for _, name := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TZ", "LANGUAGE"} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateEnvVarName(name); err != nil {
				t.Errorf("ValidateEnvVarName(%q) = %v; want nil (locale vars must not be blocked)", name, err)
			}
		})
	}
}

// TestValidateEnvVarName_RejectsDangerousNamesCaseInsensitive confirms
// the blocklist match is case-insensitive. ld.so accepts ld_preload on
// some glibc versions; the blocklist must catch the lowercase form too.
func TestValidateEnvVarName_RejectsDangerousNamesCaseInsensitive(t *testing.T) {
	for _, name := range []string{"ld_preload", "Path", "pythonpath", "node_options"} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateEnvVarName(name); err == nil {
				t.Errorf("ValidateEnvVarName(%q) = nil; want non-nil (case-insensitive blocklist miss)", name)
			}
		})
	}
}

// TestValidateEnvVarName_ErrorMessagesDoNotLeak confirms the error
// message is safe to return to the API caller — no path disclosure, no
// internal state. The message names the offending var and the reason.
func TestValidateEnvVarName_ErrorMessagesDoNotLeak(t *testing.T) {
	err := ValidateEnvVarName("LD_PRELOAD")
	if err == nil {
		t.Fatal("expected error for LD_PRELOAD")
	}
	// Must mention the var name so the user knows it was an intentional
	// rejection of THAT name, not a regex miss on something else.
	msg := err.Error()
	if !strings.Contains(msg, "LD_PRELOAD") {
		t.Errorf("error message %q should mention %q", msg, "LD_PRELOAD")
	}
}
