// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"database/sql/driver"
	"fmt"
	"strings"
)

// stringArray is a database/sql Valuer + sql.Scanner for Postgres
// text[] columns (#936 / GO-2026-6173 follow-through). It replaces
// lib/pq's pq.Array — the last live use of that module — so the module
// can be dropped entirely. Encoding is the canonical Postgres array
// literal {"a","b"}; empty arrays encode as {} (NOT NULL — callers
// historically passed pq.Array(nil) producing NULL for empty, so
// callers preserve NULL semantics by passing nil themselves).
type stringArray []string

// Value implements driver.Valuer.
func (a stringArray) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, r := range s {
			switch r {
			case '"':
				b.WriteString("\\\"")
			case '\\':
				b.WriteString("\\\\")
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan implements sql.Scanner for text[] input ({...} literal or nil).
func (a *stringArray) Scan(src interface{}) error {
	if src == nil {
		*a = nil
		return nil
	}
	var literal string
	switch v := src.(type) {
	case string:
		literal = v
	case []byte:
		literal = string(v)
	default:
		return fmt.Errorf("stringArray.Scan: unsupported src %T", src)
	}
	literal = strings.TrimSpace(literal)
	if literal == "NULL" {
		*a = nil
		return nil
	}
	literal = strings.TrimPrefix(literal, "{")
	literal = strings.TrimSuffix(literal, "}")
	if literal == "" {
		*a = stringArray{}
		return nil
	}
	// Split on commas outside quotes.
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(literal); i++ {
		c := literal[i]
		switch {
		case c == '\\' && inQuote && i+1 < len(literal):
			i++
			cur.WriteByte(literal[i])
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	out = append(out, cur.String())
	*a = out
	return nil
}

// pgStringArray adapts []string for binding (driver.Valuer).
func pgStringArray(s []string) driver.Valuer {
	if s == nil {
		return nil
	}
	return stringArray(s)
}

// stringArrayScan adapts a *[]string (or named-type pointer) for Scan
// positions. The double pointer lets callers pass &slice directly.
func stringArrayScan(p *[]string) *stringArray {
	return (*stringArray)(p)
}
