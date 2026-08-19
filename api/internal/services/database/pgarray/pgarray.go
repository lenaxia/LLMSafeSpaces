// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pgarray

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
)

// Array adapts a string-kind slice (plain or named, e.g. `type Selection
// []string`) to PostgreSQL text[] over database/sql, implementing both
// driver.Valuer (bind) and sql.Scanner (scan).
//
// It replaces lib/pq's pq.Array, which the platform no longer depends on:
// GO-2026-6166..6173 flag lib/pq with no fixed release available, and the
// driver was vestigial (pgx/v5 stdlib has been the actual driver all
// along — lib/pq was linked only for these helpers). The bind format is
// byte-identical to pq's ({"a","b"} — every element quoted); the parser
// additionally accepts PostgreSQL's own output form ({a,b}), so row
// fixtures and driver output both scan.
//
// Scope is deliberately strings-only — every array column in the schema
// (architectures, selection, domains, CIDRs, bases) is text[]. Numeric
// arrays would need this generalized; don't until one exists.
type Array struct {
	scanDst any // *[]string or *NamedStringSlice (set by Scan)
	val     any // slice or *slice (set by Value)
}

// New wraps a string-kind slice for BINDING as a text[] argument.
// Accepts the slice directly or a pointer to it.
func New(v any) *Array {
	return &Array{val: v, scanDst: v}
}

// Scan implements sql.Scanner: parses PostgreSQL text[] literal form
// into the wrapped destination slice.
func (a *Array) Scan(src any) error {
	if a.scanDst == nil {
		return fmt.Errorf("pgarray: Scan into nil destination")
	}
	elems, err := parseTextArray(src)
	if err != nil {
		return err
	}
	return assign(a.scanDst, elems)
}

// Value implements driver.Valuer: renders the wrapped slice as a
// PostgreSQL text[] literal. A nil slice binds as NULL.
func (a *Array) Value() (driver.Value, error) {
	rv := reflect.ValueOf(a.val)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil, nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil, nil
	}
	if rv.Kind() != reflect.Slice {
		return nil, fmt.Errorf("pgarray: unsupported bind type %T", a.val)
	}
	if rv.IsNil() {
		return nil, nil
	}
	if rv.Type().Elem().Kind() != reflect.String {
		return nil, fmt.Errorf("pgarray: unsupported element type %s (strings only)", rv.Type().Elem())
	}
	return formatTextArray(rv), nil
}

func assign(dst any, elems []string) error {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Ptr {
		return fmt.Errorf("pgarray: scan destination must be a pointer, got %T", dst)
	}
	sl := rv.Elem()
	if sl.Kind() != reflect.Slice || sl.Type().Elem().Kind() != reflect.String {
		return fmt.Errorf("pgarray: scan destination must point to a string-kind slice, got %T", dst)
	}
	if elems == nil {
		sl.Set(reflect.Zero(sl.Type())) // SQL NULL round-trips to nil slice, not empty
		return nil
	}
	out := reflect.MakeSlice(sl.Type(), len(elems), len(elems))
	for i, e := range elems {
		out.Index(i).SetString(e)
	}
	sl.Set(out)
	return nil
}

// formatTextArray renders a string-kind slice as {"a","b"} — byte-identical
// to lib/pq's StringArray format (every element quoted), so bound values
// are indistinguishable from the previous driver's output.
func formatTextArray(rv reflect.Value) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < rv.Len(); i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(rv.Index(i).String()))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// parseTextArray parses the PostgreSQL text[] literal wire form. src may
// be nil (SQL NULL → nil slice), string, or []byte — the three shapes
// database/sql hands a Scanner.
func parseTextArray(src any) ([]string, error) {
	switch v := src.(type) {
	case nil:
		return nil, nil
	case []byte:
		return splitTextArray(string(v))
	case string:
		return splitTextArray(v)
	default:
		return nil, fmt.Errorf("pgarray: unsupported scan source %T", src)
	}
}

func splitTextArray(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "NULL" {
		return nil, nil
	}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil, fmt.Errorf("pgarray: malformed text[] literal %q", s)
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return []string{}, nil
	}
	var (
		elems   []string
		cur     strings.Builder
		inQuote bool
		escaped bool
	)
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case inQuote && c == '\\':
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			elems = append(elems, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	elems = append(elems, cur.String())
	return elems, nil
}
