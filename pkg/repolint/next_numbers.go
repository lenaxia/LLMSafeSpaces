// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// NextNumberReport is the output of NextNumbers: the next free number
// per sequence, accounting for the local tree, origin/main, AND the
// open-PR taken-set (#1838). This is the soft layer for hand-numbered
// artifacts — migrations are authored by humans, so the lever given to
// authors is knowledge, not automation.
type NextNumberReport struct {
	// WorklogNext is the next free worklog number.
	WorklogNext int
	// WorklogMax is the highest worklog version on local ∪ origin/main.
	WorklogMax int
	// WorklogClaims lists claimed worklog versions (open PRs only).
	WorklogClaims map[int][]int
	// MigrationNext is the next free migration version.
	MigrationNext int
	// MigrationMax is the highest migration version on local ∪ origin/main.
	MigrationMax int
	// MigrationClaims lists claimed migration versions (open PRs only).
	MigrationClaims map[int][]int
}

// String renders the report for `repolint -next-numbers`.
func (r NextNumberReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "next worklog number:   %d\n", r.WorklogNext)
	if len(r.WorklogClaims) > 0 {
		fmt.Fprintf(&b, "  open-PR claims: %s\n", formatClaims(r.WorklogClaims))
	}
	fmt.Fprintf(&b, "next migration number: %d\n", r.MigrationNext)
	if len(r.MigrationClaims) > 0 {
		fmt.Fprintf(&b, "  open-PR claims: %s\n", formatClaims(r.MigrationClaims))
	}
	return b.String()
}

func formatClaims(m map[int][]int) string {
	keys := make([]int, 0, len(m))
	for v := range m {
		keys = append(keys, v)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, v := range keys {
		prs := m[v]
		nums := make([]string, len(prs))
		for i, p := range prs {
			nums[i] = strconv.Itoa(p)
		}
		parts = append(parts, fmt.Sprintf("%d (PR %s)", v, strings.Join(nums, ",")))
	}
	return strings.Join(parts, ", ")
}

// NextNumbers computes the next free worklog and migration numbers for
// the repo rooted at root, considering the local tree, origin/main
// (when reachable), and open-PR claims (when discovery is available).
// Never fails: every unavailable source simply contributes nothing.
func NextNumbers(root string) NextNumberReport {
	claims := scanOpenClaims(root)
	// Nil-safe copies: scanOpenClaims degrades to nil when discovery
	// is unavailable; every access below then behaves as "no claims".
	var wlClaims, migClaims map[int][]int
	if claims != nil {
		wlClaims, migClaims = claims.Worklogs, claims.Migrations
	}

	// Worklogs: local + git-remote occupied sets.
	wlDir := filepath.Join(root, "worklogs")
	_, _, wlMaxLocal, _ := scanWorklogDir(wlDir)
	_, _, wlMaxRemote, _ := scanWorklogGit(wlDir)
	wlOccupied := map[int]bool{}
	if entries, err := os.ReadDir(wlDir); err == nil {
		for _, e := range entries {
			if m := WorklogNumberPattern.FindStringSubmatch(e.Name()); m != nil {
				if v, err := strconv.Atoi(m[1]); err == nil {
					wlOccupied[v] = true
				}
			}
		}
	}
	for v := range wlClaims {
		wlOccupied[v] = true
	}
	wlMax := wlMaxLocal
	if wlMaxRemote > wlMax {
		wlMax = wlMaxRemote
	}

	// Migrations: local + git-remote occupied sets.
	migDir := filepath.Join(root, "api", "migrations")
	migOccupied := map[int]bool{}
	migMax := 0
	if entries, err := os.ReadDir(migDir); err == nil {
		for _, e := range entries {
			if m := MigrationPattern.FindStringSubmatch(e.Name()); m != nil {
				if v, err := strconv.Atoi(m[1]); err == nil {
					migOccupied[v] = true
					if v > migMax {
						migMax = v
					}
				}
			}
		}
	}
	migRemote, migMaxRemote, _ := scanMigrationGit(root)
	for v := range migRemote {
		migOccupied[v] = true
	}
	if migMaxRemote > migMax {
		migMax = migMaxRemote
	}
	for v := range migClaims {
		migOccupied[v] = true
	}

	// Report only the claims that matter for the next pick: versions
	// strictly above the max already on local ∪ origin/main. Every open
	// PR trivially "claims" the shared history (1..max) via ancestry —
	// listing it is noise that drowns the signal (a live smoke showed
	// all 9 open PRs "claiming" all 43 migrations).
	filterHigh := func(m map[int][]int, max int) map[int][]int {
		if len(m) == 0 {
			return nil
		}
		out := map[int][]int{}
		for v, prs := range m {
			if v > max {
				out[v] = prs
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}

	return NextNumberReport{
		WorklogNext:     nextFree(wlMax, wlOccupied),
		WorklogMax:      wlMax,
		WorklogClaims:   filterHigh(wlClaims, wlMax),
		MigrationNext:   nextFree(migMax, migOccupied),
		MigrationMax:    migMax,
		MigrationClaims: filterHigh(migClaims, migMax),
	}
}

// nextFree returns the smallest int strictly greater than max that is
// not in occupied.
func nextFree(max int, occupied map[int]bool) int {
	for {
		max++
		if !occupied[max] {
			return max
		}
	}
}

// scanMigrationGit returns migration versions present on origin/main
// (indexed as a set) plus the max version, mirroring scanWorklogGit.
// Empty set on any git unavailability.
func scanMigrationGit(root string) (map[int]bool, int, error) {
	versions := map[int]bool{}
	maxVer := 0

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "ls-tree", "--name-only", mainlineRef, "--", "api/migrations/")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return versions, 0, nil
	}

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := MigrationPattern.FindStringSubmatch(filepath.Base(line))
		if m == nil {
			continue
		}
		if v, err := strconv.Atoi(m[1]); err == nil {
			versions[v] = true
			if v > maxVer {
				maxVer = v
			}
		}
	}
	return versions, maxVer, nil
}
