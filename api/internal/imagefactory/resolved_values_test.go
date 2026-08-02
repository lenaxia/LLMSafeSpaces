// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"strings"
	"testing"
)

func extMap(exts ...Extension) map[string]Extension {
	m := make(map[string]Extension, len(exts))
	for _, e := range exts {
		m[e.ID] = e
	}
	return m
}

func TestResolveSelection_HappyPath(t *testing.T) {
	t.Parallel()
	exts := extMap(
		Extension{ID: "ffmpeg", Type: ExtensionTypeApt, Value: "ffmpeg", SupportedBases: []string{"bookworm", "trixie"}},
		Extension{ID: "python313", Type: ExtensionTypeMise, Value: "python@3.13", SupportedBases: []string{"bookworm"}},
	)
	rv, err := ResolveSelection(Selection{"ffmpeg", "python313"}, exts, "bookworm")
	if err != nil {
		t.Fatalf("ResolveSelection: %v", err)
	}
	if got := rv["ffmpeg"]; got.Type != ExtensionTypeApt || got.Value != "ffmpeg" {
		t.Errorf("ffmpeg resolved wrong: %+v", got)
	}
	if got := rv["python313"]; got.Type != ExtensionTypeMise || got.Value != "python@3.13" {
		t.Errorf("python313 resolved wrong: %+v", got)
	}
}

func TestResolveSelection_FileSpecCarried(t *testing.T) {
	t.Parallel()
	exts := extMap(Extension{
		ID:             "motd",
		Type:           ExtensionTypeFile,
		Value:          "welcome\n",
		FileSpec:       &FileSpec{Path: "/etc/motd", Mode: "0644"},
		SupportedBases: []string{"bookworm"},
	})
	rv, err := ResolveSelection(Selection{"motd"}, exts, "bookworm")
	if err != nil {
		t.Fatalf("ResolveSelection: %v", err)
	}
	got := rv["motd"]
	if got.FileSpec == nil || got.FileSpec.Path != "/etc/motd" {
		t.Fatalf("file_spec not carried: %+v", got)
	}
	if got.Value != "welcome\n" {
		t.Fatalf("file value (body) not carried: %q", got.Value)
	}
}

func TestResolveSelection_DedupCollapses(t *testing.T) {
	t.Parallel()
	exts := extMap(Extension{ID: "ffmpeg", Type: ExtensionTypeApt, Value: "ffmpeg", SupportedBases: []string{"bookworm"}})
	rv, err := ResolveSelection(Selection{"ffmpeg", "ffmpeg"}, exts, "bookworm")
	if err != nil {
		t.Fatalf("ResolveSelection: %v", err)
	}
	if len(rv) != 1 {
		t.Fatalf("expected dedup to 1 entry, got %d", len(rv))
	}
}

func TestResolveSelection_UnhappyPaths(t *testing.T) {
	t.Parallel()
	exts := extMap(
		Extension{ID: "ffmpeg", Type: ExtensionTypeApt, Value: "ffmpeg", SupportedBases: []string{"bookworm"}},
		Extension{ID: "retired", Type: ExtensionTypeApt, Value: "x", SupportedBases: []string{"bookworm"}, Retired: true},
		Extension{ID: "trixieonly", Type: ExtensionTypeApt, Value: "x", SupportedBases: []string{"trixie"}},
	)
	cases := []struct {
		name    string
		sel     Selection
		base    string
		wantSub string
	}{
		{"missing id", Selection{"ghost"}, "bookworm", "not found"},
		{"retired extension", Selection{"retired"}, "bookworm", "retired"},
		{"unsupported base", Selection{"trixieonly"}, "bookworm", "not supported"},
		{"empty selection", Selection{}, "bookworm", "empty"},
		{"invalid id charset", Selection{"bad;rm"}, "bookworm", "charset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveSelection(tc.sel, exts, tc.base)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want err containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}

func TestValidateResolved_HappyPath(t *testing.T) {
	t.Parallel()
	rv := ResolvedValues{
		"ffmpeg":    {Type: ExtensionTypeApt, Value: "ffmpeg"},
		"python313": {Type: ExtensionTypeMise, Value: "python@3.13"},
		"motd":      {Type: ExtensionTypeFile, Value: "hi\n", FileSpec: &FileSpec{Path: "/etc/motd", Mode: "0644"}},
		"nomode":    {Type: ExtensionTypeFile, Value: "x", FileSpec: &FileSpec{Path: "/x"}}, // mode defaults
	}
	if err := ValidateResolved(rv); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidateResolved_EmptyMapIsValid(t *testing.T) {
	t.Parallel()
	// Empty resolved_values is a named alias for the base image (no extensions).
	if err := ValidateResolved(ResolvedValues{}); err != nil {
		t.Fatalf("empty resolved_values must be valid (extension-less config), got %v", err)
	}
}

func TestValidateResolved_UnhappyPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rv   ResolvedValues
		want string
	}{
		{"unknown type", ResolvedValues{"x": {Type: "bogus", Value: "v"}}, "type"},
		{"empty value", ResolvedValues{"x": {Type: ExtensionTypeApt, Value: ""}}, "value"},
		{"file without filespec", ResolvedValues{"x": {Type: ExtensionTypeFile, Value: "v"}}, "file_spec"},
		{"file relative path", ResolvedValues{"x": {Type: ExtensionTypeFile, Value: "v", FileSpec: &FileSpec{Path: "etc/x"}}}, "absolute"},
		{"file traversal", ResolvedValues{"x": {Type: ExtensionTypeFile, Value: "v", FileSpec: &FileSpec{Path: "/etc/../passwd"}}}, "traversal"},
		{"file bad mode", ResolvedValues{"x": {Type: ExtensionTypeFile, Value: "v", FileSpec: &FileSpec{Path: "/x", Mode: "999"}}}, "mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateResolved(tc.rv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want err containing %q, got %v", tc.want, err)
			}
		})
	}
}
