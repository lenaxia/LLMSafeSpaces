// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoAdapterNilChecks (the #828 final-batch repolint gate): the
// adapter is a required constructor parameter — a nil-adapter nil-check
// anywhere in this package is a reintroduction of the deleted
// dual-path/fallback pattern and must fail here. The count of
// `h.adapter == nil` / `h.adapter != nil` in non-test .go files must be
// ZERO.
func TestNoAdapterNilChecks(t *testing.T) {
	pattern := regexp.MustCompile(`h\.adapter\s*(!=|==)\s*nil`)

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var offenders []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", e.Name()))
		require.NoError(t, err)
		for i, line := range strings.Split(string(data), "\n") {
			if pattern.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				offenders = append(offenders, e.Name()+":"+strconv.Itoa(i+1)+" "+strings.TrimSpace(line))
			}
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("adapter nil-checks are forbidden (#828 final batch — the adapter is ctor-required); found %d:\n%s",
			len(offenders), strings.Join(offenders, "\n"))
	}
}
