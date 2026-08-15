// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"testing"
)

func TestValidateImageRefPinned(t *testing.T) {
	cases := []struct {
		name  string
		value string
		valid bool
	}{
		{"empty means RTE fallback", "", true},
		{"runtime-environment name", "base", true},
		{"rte name with dashes and digits", "python-3.11", true},
		{"semver tag", "ghcr.io/lenaxia/llmsafespaces/base:0.15.5", true},
		{"immutable ci tag sha", "ghcr.io/lenaxia/llmsafespaces/base:sha-9cf7947", true},
		{"immutable ci tag ts", "ghcr.io/lenaxia/llmsafespaces/base:ts-1781332002", true},
		{"registry with port and tag", "registry.local:5000/team/img:v1.2.3", true},
		{"digest pin", "ghcr.io/lenaxia/llmsafespaces/base@sha256:808af6bbafc2df306e6a2fcf8945536668af23b6233de0a05a895f05b8671aa0", true},
		{"digest pin with port registry", "registry.local:5000/team/img@sha256:808af6bbafc2df306e6a2fcf8945536668af23b6233de0a05a895f05b8671aa0", true},
		{"explicit latest tag", "ghcr.io/lenaxia/llmsafespaces/base:latest", false},
		{"latest tag case-insensitive", "ghcr.io/lenaxia/llmsafespaces/base:Latest", false},
		{"latest tag with port registry", "registry.local:5000/team/img:latest", false},
		{"main tag", "ghcr.io/lenaxia/llmsafespaces/base:main", false},
		{"master tag", "ghcr.io/lenaxia/llmsafespaces/base:master", false},
		{"dev tag", "ghcr.io/lenaxia/llmsafespaces/base:dev", false},
		{"edge tag", "ghcr.io/lenaxia/llmsafespaces/base:edge", false},
		{"nightly tag", "ghcr.io/lenaxia/llmsafespaces/base:nightly", false},
		{"untagged image ref is implicit latest", "ghcr.io/lenaxia/llmsafespaces/base", false},
		{"untagged with port registry", "registry.local:5000/team/img", false},
		{"image ref with spaces", "ghcr.io/lenaxia/llmsafespaces/base:0.15.5 extra", false},
		{"tag with invalid chars", "ghcr.io/lenaxia/llmsafespaces/base:0.15.5^", false},
		{"bare latest is not an rte name", "latest", false},
		{"bare main is not an rte name", "main", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageRefPinned(tc.value)
			if tc.valid && err != nil {
				t.Errorf("expected %q valid, got error: %v", tc.value, err)
			}
			if !tc.valid && err == nil {
				t.Errorf("expected %q rejected, got nil", tc.value)
			}
		})
	}
}

func TestValidate_RejectMutableTags(t *testing.T) {
	def := SettingDef{
		Key:               "test.image",
		Tier:              2,
		Type:              TypeString,
		RejectMutableTags: true,
	}

	if err := Validate(def, "registry.example.com/team/app:1.0.0"); err != nil {
		t.Errorf("pinned tag rejected: %v", err)
	}
	if err := Validate(def, "registry.example.com/team/app:latest"); err == nil {
		t.Error("floating tag accepted")
	}
	if err := Validate(def, 42); err == nil {
		t.Error("non-string accepted for string setting")
	}
}

func TestValidate_NoMutableTagCheckWithoutFlag(t *testing.T) {
	def := SettingDef{Key: "test.plain", Tier: 2, Type: TypeString}
	if err := Validate(def, "ghcr.io/whatever/base:latest"); err != nil {
		t.Errorf("plain string settings must not be image-checked: %v", err)
	}
}

func TestWorkspaceDefaultImage_DefaultIsNotFloating(t *testing.T) {
	for _, def := range InstanceSettings() {
		if def.Key != "workspace.defaultImage" {
			continue
		}
		if def.Default != "" {
			t.Errorf("workspace.defaultImage default must be empty (RTE fallback), got %q", def.Default)
		}
		if !def.RejectMutableTags {
			t.Error("workspace.defaultImage must set RejectMutableTags")
		}
		if err := Validate(def, def.Default); err != nil {
			t.Errorf("default must pass validation: %v", err)
		}
		return
	}
	t.Fatal("workspace.defaultImage not found in InstanceSettings")
}

func TestInstanceSettings_ImageLikeDefaultsArePinned(t *testing.T) {
	for _, def := range InstanceSettings() {
		if !def.RejectMutableTags {
			continue
		}
		s, ok := def.Default.(string)
		if !ok {
			t.Errorf("%s: RejectMutableTags requires a string default", def.Key)
			continue
		}
		if err := validateImageRefPinned(s); err != nil {
			t.Errorf("%s: default %q is not pinned: %v", def.Key, s, err)
		}
	}
}

func TestNormalize_TrimsDefaultImageWhitespace(t *testing.T) {
	def := SettingDef{Key: "workspace.defaultImage", Tier: 2, Type: TypeString}
	got := Normalize(def, "  ghcr.io/x/y:1.0.0  ")
	if got != "ghcr.io/x/y:1.0.0" {
		t.Errorf("expected trimmed ref, got %q", got)
	}
}

func TestIsMutableTag(t *testing.T) {
	if !isMutableTag("latest") || !isMutableTag("Latest") || !isMutableTag("MAIN") {
		t.Error("known mutable tags must match case-insensitively")
	}
	if isMutableTag("0.15.5") || isMutableTag("sha-9cf7947") || isMutableTag("") {
		t.Error("pinned tags must not be flagged mutable")
	}
}
