// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pgarray provides Postgres text[] encoding/decoding for
// database/sql without github.com/lib/pq.
//
// lib/pq was used exclusively for pq.Array/pq.StringArray (+ one error
// type assertion that could never match under the pgx driver — see
// IsUniqueViolation). Its driver's conservative call-graph edge kept
// tripping govulncheck on advisories with no upstream fix (first:
// GO-2026-6173, 2026-08-18); dropping the dependency ends that class.
//
// Wire semantics are byte-compatible with lib/pq v1.12.3 (pinned by
// probe against the real implementation — see pgarray_test.go):
//
//	Value: nil slice → SQL NULL; empty slice → '{}'; every element is
//	       double-quoted with `"` → `\"` and `\` → `\\` escaping.
//	Scan: NULL → nil slice; '{}' → empty non-nil slice; string or
//	      []byte input; unquoted elements tolerated; NULL element → error
//	      (a string column cannot hold SQL NULL).
package pgarray

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
)

// StringArray is a self-contained []string that implements Valuer and
// Scanner. Drop-in for pq.StringArray.
type StringArray []string

// Value implements driver.Valuer.
func (s StringArray) Value() (driver.Value, error) { return encode(s), nil }

// Scan implements sql.Scanner.
func (s *StringArray) Scan(src any) error {
	out, err := decode(src)
	if err != nil {
		return err
	}
	*s = out
	return nil
}

// arrayValuer lets Array() return one type satisfying both interfaces.
type arrayValuer struct {
	get  func() ([]string, bool) // false = nil slice (→ NULL)
	set  func([]string)
	addr any
}

// Array adapts a string-slice or string-slice-pointer (including named
// types like imagefactory.Selection) to driver.Valuer + sql.Scanner.
// Drop-in call-shape for pq.Array, minus pq's named-type scan limitation
// (which forced (*[]string) casts at call sites — no longer needed).
func Array(a any) interface {
	driver.Valuer
	sql.Scanner
} {
	rv := reflect.ValueOf(a)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			panic("pgarray.Array: nil pointer") // matches pq: programming error
		}
		if rv.Elem().Kind() == reflect.Slice {
			target := rv.Elem()
			return &arrayValuer{
				get: func() ([]string, bool) {
					if target.IsNil() {
						return nil, false
					}
					return readStrings(target), true
				},
				set: func(s []string) {
					if s == nil {
						// NULL → nil slice, matching pq (probe-verified).
						target.Set(reflect.Zero(target.Type()))
					} else {
						target.Set(reflect.ValueOf(s).Convert(target.Type()))
					}
				},
			}
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Slice {
		src := rv
		return &arrayValuer{
			get: func() ([]string, bool) {
				if src.IsNil() {
					return nil, false
				}
				return readStrings(src), true
			},
			set: func([]string) { panic("pgarray.Array: Scan into non-pointer slice") },
		}
	}
	panic(fmt.Sprintf("pgarray.Array: unsupported type %T (want string slice or pointer to one)", a))
}

func readStrings(v reflect.Value) []string {
	out := make([]string, v.Len())
	for i := range out {
		out[i] = v.Index(i).String()
	}
	return out
}

func (a *arrayValuer) Value() (driver.Value, error) {
	s, ok := a.get()
	if !ok {
		return nil, nil
	}
	return encode(s), nil
}

func (a *arrayValuer) Scan(src any) error {
	out, err := decode(src)
	if err != nil {
		return err
	}
	a.set(out)
	return nil
}

func encode(s []string) driver.Value {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range s {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, r := range e {
			switch r {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func decode(src any) ([]string, error) {
	if src == nil {
		return nil, nil
	}
	var text string
	switch v := src.(type) {
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return nil, fmt.Errorf("pgarray: unsupported source type %T", src)
	}
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "{") || !strings.HasSuffix(text, "}") {
		return nil, fmt.Errorf("pgarray: malformed array literal %q", text)
	}
	inner := text[1 : len(text)-1]
	if inner == "" {
		return []string{}, nil
	}
	// Elements are quoted per our encoder; unquoted (hand-written SQL
	// literals) are tolerated per Postgres's own leniency.
	var (
		out     []string
		cur     strings.Builder
		inQuote bool
		escaped bool
		flush   = func() { out = append(out, cur.String()); cur.Reset() }
	)
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		switch {
		case c == '\\' && inQuote:
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	if inQuote || escaped {
		return nil, fmt.Errorf("pgarray: unterminated quote in %q", text)
	}
	flush()
	// Postgres NULL element: a string column cannot hold it — error like pq.
	for _, e := range out {
		if e == "NULL" && !strings.Contains(inner, `"NULL"`) {
			return nil, fmt.Errorf("pgarray: cannot scan NULL element into string (index 0)")
		}
	}
	return out, nil
}
