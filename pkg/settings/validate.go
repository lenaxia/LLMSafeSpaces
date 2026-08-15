// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"fmt"
	"regexp"
)

// Validate checks that value is valid for the given setting definition.
// Returns nil if valid, or a descriptive error.
func Validate(def SettingDef, value any) error {
	switch def.Type {
	case TypeBool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("key %q: expected bool, got %T", def.Key, value)
		}
	case TypeInt:
		n, ok := toInt(value)
		if !ok {
			return fmt.Errorf("key %q: expected int, got %T", def.Key, value)
		}
		if def.Min != nil && n < *def.Min {
			return fmt.Errorf("key %q: value %d below minimum %d", def.Key, n, *def.Min)
		}
		if def.Max != nil && n > *def.Max {
			return fmt.Errorf("key %q: value %d above maximum %d", def.Key, n, *def.Max)
		}
	case TypeString:
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("key %q: expected string, got %T", def.Key, value)
		}
		if def.Pattern != "" {
			re, err := regexp.Compile(def.Pattern)
			if err != nil {
				return fmt.Errorf("key %q: invalid pattern %q: %w", def.Key, def.Pattern, err)
			}
			if !re.MatchString(s) {
				return fmt.Errorf("key %q: value %q does not match pattern %q", def.Key, s, def.Pattern)
			}
		}
		if def.RejectMutableTags {
			if err := validateImageRefPinned(s); err != nil {
				return fmt.Errorf("key %q: %w", def.Key, err)
			}
		}
	case TypeEnum:
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("key %q: expected string for enum, got %T", def.Key, value)
		}
		found := false
		for _, e := range def.Enum {
			if s == e {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("key %q: value %q not in allowed values %v", def.Key, s, def.Enum)
		}
	case TypeStrings:
		switch v := value.(type) {
		case []string:
			// valid
			_ = v
		case []any:
			for i, item := range v {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("key %q: element %d is %T, expected string", def.Key, i, item)
				}
			}
		default:
			return fmt.Errorf("key %q: expected []string, got %T", def.Key, value)
		}
	default:
		return fmt.Errorf("key %q: unknown type %q", def.Key, def.Type)
	}
	return nil
}

// toInt converts a value to int, handling float64 (from JSON) and int.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
		return 0, false
	default:
		return 0, false
	}
}
