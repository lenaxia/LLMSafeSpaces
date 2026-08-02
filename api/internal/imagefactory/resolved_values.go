// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"fmt"
	"regexp"
	"strings"
)

// octModeRe matches 3- or 4-digit octal file modes ("0644", "0755", "644").
// Leading "0" optional; 4-digit form is conventional in Dockerfiles.
var octModeRe = regexp.MustCompile(`^0?[0-7]{3}$`)

// ResolveSelection joins a Selection against a set of catalog Extensions
// and returns the frozen ResolvedValues projection stored on the config
// (design/0046 #10). Errors if any ID is missing, retired, or unsupported
// on baseName. Pure: no I/O.
func ResolveSelection(sel Selection, exts map[string]Extension, baseName string) (ResolvedValues, error) {
	if err := ValidateSelection(sel); err != nil {
		return nil, err
	}
	rv := make(ResolvedValues, len(sel))
	for _, id := range sel {
		ext, ok := exts[id]
		if !ok {
			return nil, fmt.Errorf("extension %q not found in catalog", id)
		}
		if ext.Retired {
			return nil, fmt.Errorf("extension %q is retired", id)
		}
		if !contains(ext.SupportedBases, baseName) {
			return nil, fmt.Errorf("extension %q is not supported on base %q (supported: %s)",
				id, baseName, strings.Join(ext.SupportedBases, ", "))
		}
		rv[id] = ResolvedValue{
			Type:     ext.Type,
			Value:    ext.Value,
			FileSpec: ext.FileSpec,
		}
	}
	if len(rv) == 0 {
		return nil, fmt.Errorf("resolved to empty values")
	}
	return rv, nil
}

// ValidateResolved checks the resolved shape: if non-empty, every entry has
// a known type, a non-empty value, and (for file) an absolute non-traversal
// path + valid octal mode. Empty is valid — an extension-less config is a
// named alias for the base image (design permits it; the picker shows it).
// Used as a defensive check on data crossing the DB boundary.
func ValidateResolved(rv ResolvedValues) error {
	for id, v := range rv {
		switch v.Type {
		case ExtensionTypeApt, ExtensionTypeMise, ExtensionTypeFile:
			// ok
		default:
			return fmt.Errorf("resolved_values[%q]: unknown type %q", id, v.Type)
		}
		if strings.TrimSpace(v.Value) == "" {
			return fmt.Errorf("resolved_values[%q]: empty value", id)
		}
		if v.Type == ExtensionTypeFile {
			if v.FileSpec == nil {
				return fmt.Errorf("resolved_values[%q]: file requires file_spec", id)
			}
			if err := validateFileSpec(v.FileSpec); err != nil {
				return fmt.Errorf("resolved_values[%q]: %w", id, err)
			}
		}
	}
	return nil
}

func validateFileSpec(fs *FileSpec) error {
	if fs.Path == "" {
		return fmt.Errorf("file_spec: path is required")
	}
	if !strings.HasPrefix(fs.Path, "/") {
		return fmt.Errorf("file_spec: path %q must be absolute", fs.Path)
	}
	if containsTraversal(fs.Path) {
		return fmt.Errorf("file_spec: path %q contains traversal (..)", fs.Path)
	}
	if fs.Mode != "" && !octModeRe.MatchString(fs.Mode) {
		return fmt.Errorf("file_spec: mode %q must be octal like 0644 or 0755", fs.Mode)
	}
	return nil
}

func containsTraversal(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
