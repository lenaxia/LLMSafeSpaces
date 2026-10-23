// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"errors"
	"reflect"
	"testing"
)

// Purity pins for the claims parser + orchestration pins for the
// gh-backed discovery (via the exec seams). The degradation contract:
// discovery NEVER fails the caller — it returns nil (or a partial set)
// and assignment proceeds with whatever knowledge it has.

func TestClaimsFromTreePaths(t *testing.T) {
	claims := &OpenClaims{Worklogs: map[int][]int{}, Migrations: map[int][]int{}}
	claimsFromTreePaths(1839, []string{
		"worklogs/1181_2026-10-22_lane-alpha.md",  // claims worklog 1181
		"worklogs/NNNN_2026-10-22_unnumbered.md",  // sentinel: claims nothing
		"worklogs/README.md",                      // non-numbered: ignored
		"api/migrations/000044_board_ui.up.sql",   // claims migration 44
		"api/migrations/000044_board_ui.down.sql", // same claim, down side
		"api/migrations/README.md",                // ignored
		"internal/foo/bar.go",                     // not a numbered artifact
		"helm/migrations/000044_board_ui.up.sql",  // mirror copy: ignored (canonical dir only)
	}, claims)

	if got := claims.Worklogs[1181]; !reflect.DeepEqual(got, []int{1839}) {
		t.Errorf("worklog 1181 claims = %v, want [1839]", got)
	}
	if len(claims.Worklogs) != 1 {
		t.Errorf("worklog claims = %v, want exactly one entry", claims.Worklogs)
	}
	if got := claims.Migrations[44]; !reflect.DeepEqual(got, []int{1839}) {
		t.Errorf("migration 44 claims = %v, want [1839]", got)
	}
	if len(claims.Migrations) != 1 {
		t.Errorf("migration claims = %v, want exactly one entry", claims.Migrations)
	}
}

func TestScanOpenClaimsViaGitHub_OwnPRExcluded(t *testing.T) {
	restore := overrideClaimsExecSeams(
		func(string) ([]openPRHead, error) {
			return []openPRHead{
				{Number: 1839, HeadSHA: "aaa"}, // own PR (HEAD matches)
				{Number: 1841, HeadSHA: "bbb"}, // sibling
			}, nil
		},
		func(_ string, sha string) ([]string, error) {
			switch sha {
			case "aaa":
				return []string{"worklogs/1181_2026-10-22_own.md"}, nil
			case "bbb":
				return []string{"worklogs/1182_2026-10-22_sibling.md"}, nil
			}
			return nil, errors.New("unexpected sha")
		},
		func(string) string { return "aaa" },
	)
	defer restore()

	claims := scanOpenClaimsViaGitHub(".")
	if claims == nil {
		t.Fatal("expected non-nil claims")
	}
	if claims.WorklogClaimed(1181) {
		t.Error("own PR's claim (HEAD matches) must be excluded — it is local knowledge")
	}
	if !claims.WorklogClaimed(1182) {
		t.Error("sibling PR's claim 1182 must be visible")
	}
}

func TestScanOpenClaimsViaGitHub_SiblingClaimedBothClasses(t *testing.T) {
	restore := overrideClaimsExecSeams(
		func(string) ([]openPRHead, error) {
			return []openPRHead{
				{Number: 1839, HeadSHA: "aaa"},
				{Number: 1841, HeadSHA: "bbb"},
			}, nil
		},
		func(_ string, sha string) ([]string, error) {
			if sha == "bbb" {
				return []string{
					"worklogs/1182_2026-10-22_lane-beta.md",
					"api/migrations/000044_board_ui.up.sql",
					"api/migrations/000044_board_ui.down.sql",
				}, nil
			}
			return nil, errors.New("boom")
		},
		func(string) string { return "aaa" },
	)
	defer restore()

	// PR 1839's tree fetch fails — it must be skipped, not fatal.
	claims := scanOpenClaimsViaGitHub(".")
	if claims == nil {
		t.Fatal("expected non-nil claims")
	}
	if !claims.WorklogClaimed(1182) {
		t.Error("sibling worklog claim 1182 must be visible")
	}
	if !claims.MigrationClaimed(44) {
		t.Error("sibling migration claim 44 must be visible")
	}
	if got := claims.Worklogs[1182]; !reflect.DeepEqual(got, []int{1841}) {
		t.Errorf("worklog 1182 claims = %v, want [1841]", got)
	}
}

func TestScanOpenClaimsViaGitHub_ListFails_DegradesToNil(t *testing.T) {
	restore := overrideClaimsExecSeams(
		func(string) ([]openPRHead, error) { return nil, errors.New("no gh") },
		nil, nil,
	)
	defer restore()

	if claims := scanOpenClaimsViaGitHub("."); claims != nil {
		t.Fatalf("expected nil claims when gh is unavailable, got %+v", claims)
	}
}

// TestOpenClaimsNilReceiver documents the degraded-mode contract used
// by fixWorklogs: every predicate on a nil *OpenClaims is false.
func TestOpenClaimsNilReceiver(t *testing.T) {
	var claims *OpenClaims
	if claims.WorklogClaimed(1181) || claims.MigrationClaimed(44) {
		t.Error("nil claims must claim nothing")
	}
	if set := claims.WorklogClaimSet(); set != nil {
		t.Errorf("nil claims yield nil set, got %v", set)
	}
}

func overrideClaimsExecSeams(
	list func(string) ([]openPRHead, error),
	tree func(string, string) ([]string, error),
	head func(string) string,
) (restore func()) {
	origList, origTree, origHead := ghListOpenPRs, ghPRTreePaths, gitHEADSHA
	if list != nil {
		ghListOpenPRs = list
	}
	if tree != nil {
		ghPRTreePaths = tree
	}
	if head != nil {
		gitHEADSHA = head
	}
	return func() {
		ghListOpenPRs, ghPRTreePaths, gitHEADSHA = origList, origTree, origHead
	}
}
