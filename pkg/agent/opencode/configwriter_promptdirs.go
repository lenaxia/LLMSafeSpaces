// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
)

// stripBuildPrompt removes the writer-owned build.prompt key from the
// captured "agent" section. Called when an Apply supplies the
// AdminPrompt source (replace or clear): the source is authoritative,
// so a stale prompt captured from a previous render must not survive a
// clear or bleed through a replace. Sibling agent/build fields (tools,
// model, etc.) are preserved; empty objects are pruned so a clear does
// not leave `"agent": {"build": {}}` noise. Returns nil when nothing
// but the prompt existed.
func stripBuildPrompt(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var agent map[string]json.RawMessage
	if json.Unmarshal(raw, &agent) != nil || agent == nil {
		return raw
	}
	buildRaw, ok := agent["build"]
	if !ok {
		return raw
	}
	var build map[string]json.RawMessage
	if json.Unmarshal(buildRaw, &build) != nil || build == nil {
		return raw
	}
	if _, ok := build["prompt"]; !ok {
		return raw
	}
	delete(build, "prompt")
	if len(build) == 0 {
		delete(agent, "build")
	} else {
		out, err := json.Marshal(build)
		if err != nil {
			return raw
		}
		agent["build"] = out
	}
	if len(agent) == 0 {
		return nil
	}
	out, err := json.Marshal(agent)
	if err != nil {
		return raw
	}
	return out
}

// stripInjectedExternalDirs removes the writer-injected keys from the
// captured "mode" section's permissions.external_directory map. Called
// when an Apply supplies the AllowedDirs source (replace or clear):
// like stripBuildPrompt, the source is authoritative over prior
// renders. Only the tracked injected keys are removed — user-authored
// entries (deny rules, their own allows, a bare-string policy) survive.
// Objects that become empty are pruned; a nil/absent or bare-string
// external_directory is returned untouched. Returns nil when the mode
// section becomes empty.
func stripInjectedExternalDirs(raw json.RawMessage, keys []string) json.RawMessage {
	if len(raw) == 0 || len(keys) == 0 {
		return raw
	}
	var mode map[string]json.RawMessage
	if json.Unmarshal(raw, &mode) != nil || mode == nil {
		return raw
	}
	permsRaw, ok := mode["permissions"]
	if !ok {
		return raw
	}
	var perms map[string]json.RawMessage
	if json.Unmarshal(permsRaw, &perms) != nil || perms == nil {
		return raw
	}
	extRaw, ok := perms["external_directory"]
	if !ok {
		return raw
	}
	var ext map[string]string
	if json.Unmarshal(extRaw, &ext) != nil || ext == nil {
		// Bare-string or other non-map form: user-owned policy, preserved.
		return raw
	}
	changed := false
	for _, k := range keys {
		if _, ok := ext[k]; ok {
			delete(ext, k)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	if len(ext) == 0 {
		delete(perms, "external_directory")
	} else {
		out, err := json.Marshal(ext)
		if err != nil {
			return raw
		}
		perms["external_directory"] = out
	}
	if len(perms) == 0 {
		delete(mode, "permissions")
	} else {
		out, err := json.Marshal(perms)
		if err != nil {
			return raw
		}
		mode["permissions"] = out
	}
	if len(mode) == 0 {
		return nil
	}
	out, err := json.Marshal(mode)
	if err != nil {
		return raw
	}
	return out
}

// sanitizeAllowedDirs canonicalizes an AllowedDirs input: empty
// patterns dropped, duplicates collapsed (first occurrence wins),
// returning a fresh slice (the caller's slice is never aliased — same
// defensive-copy policy as every other slice source). Mirrors the
// sanitation the side-car load path applies so both entry points
// render identically.
func sanitizeAllowedDirs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		if d == "" {
			continue
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
