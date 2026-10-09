// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package diskrecovery

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrBusy is returned when another recover is in flight on this pod
// (serialized by design — concurrent sweeps would double-walk and race
// unlinks).
var ErrBusy = errors.New("disk recovery already in progress")

// ErrNoUsage is returned when the volume usage could not be read
// (statfs failed). The engine refuses to delete anything without a
// trustworthy before/after measurement — an unmeasured sweep is exactly
// the maximal-sweep behavior #1601 forbids.
var ErrNoUsage = errors.New("workspace disk usage unavailable")

// UsageFunc reads (used, total) for the workspace volume. Production:
// statfs on /workspace (the PVC mounts /workspace, /home/sandbox and
// /tmp are subpath mounts of the same volume — any view reads the same
// numbers agentd's statusz disk gauge reports).
type UsageFunc func() (usedBytes, totalBytes int64, err error)

// Engine executes allowlisted cache recovery. Zero filesystem writes
// outside the validated unlinks themselves (owner ruling on #1601: the
// rescue path must work at 100% full — no temp files, no report
// persistence, response-only).
type Engine struct {
	manifest Manifest
	usage    UsageFunc
	now      func() time.Time
	volume   string // informational: the statfs target documented above

	mu sync.Mutex
}

// NewEngine builds an engine. usage is required (nil → every Recover
// returns ErrNoUsage: fail-closed, never an unmeasured sweep).
func NewEngine(m Manifest, usage UsageFunc) *Engine {
	return &Engine{
		manifest: m,
		usage:    usage,
		now:      time.Now,
		volume:   "/workspace",
	}
}

// SetClock overrides the staleness clock (tests).
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

// Recover runs one pass. Idempotent by construction: below-target
// volumes are a measured no-op; already-deleted classes report
// not_present on the next call.
func (e *Engine) Recover(ctx context.Context, req Request) (Report, error) {
	if !e.mu.TryLock() {
		return Report{}, ErrBusy
	}
	defer e.mu.Unlock()

	target := clampTarget(req.TargetRatio)

	used, total, err := e.usage()
	if err != nil || total <= 0 {
		return Report{}, ErrNoUsage
	}
	report := Report{
		DryRun:           req.DryRun,
		TargetRatio:      target,
		BeforeUsedBytes:  used,
		BeforeTotalBytes: total,
		AfterUsedBytes:   used,
		AfterTotalBytes:  total,
		BeforeRatio:      ratio(used, total),
		AfterRatio:       ratio(used, total),
		RuntimeBase:      e.manifest.Base,
		Classes:          nil,
	}

	// Idempotent fast path: already at/below target — nothing to do,
	// and (dry-run or not) nothing is measured or deleted.
	if report.BeforeRatio <= target {
		report.AlreadyBelowTarget = true
		return report, nil
	}

	// Measure every class (read-only), largest-first.
	type candidate struct {
		entry  Entry
		report ClassReport
		paths  []string // prefix classes: the stale children to remove
	}
	var candidates []candidate
	for _, entry := range e.manifest.Entries {
		cr := ClassReport{Class: entry.Class, Path: entry.Path}
		if err := validateEntry(e.manifest, entry); err != nil {
			cr.Status = ClassRefused
			cr.Reason = err.Error()
			report.Classes = append(report.Classes, cr)
			continue
		}
		bytes, count, paths, err := e.measure(ctx, entry)
		cr.Bytes, cr.Entries = bytes, count
		switch {
		case err != nil:
			cr.Status = ClassError
			cr.Reason = err.Error()
		case count == 0:
			cr.Status = ClassNotPresent
		default:
			candidates = append(candidates, candidate{entry: entry, report: cr, paths: paths})
			continue
		}
		report.Classes = append(report.Classes, cr)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].report.Bytes != candidates[j].report.Bytes {
			return candidates[i].report.Bytes > candidates[j].report.Bytes
		}
		return candidates[i].entry.Class < candidates[j].entry.Class
	})

	// Free ENOUGH: walk largest-first, stop the moment the volume is
	// below target. Remaining measured classes report
	// skipped_target_met — the sweep is intentionally not maximal.
	for i, c := range candidates {
		usedNow, totalNow, err := e.usage()
		if err == nil && totalNow > 0 {
			report.AfterUsedBytes, report.AfterTotalBytes = usedNow, totalNow
			report.AfterRatio = ratio(usedNow, totalNow)
		}
		if report.AfterRatio <= target {
			for _, rest := range candidates[i:] {
				rest.report.Status = ClassSkippedTargetMet
				report.Classes = append(report.Classes, rest.report)
			}
			report.StoppedEarly = true
			break
		}

		if req.DryRun {
			c.report.Status = ClassWouldFree
			report.Classes = append(report.Classes, c.report)
			continue
		}

		if _, derr := e.delete(c.paths); derr != nil {
			c.report.Status = ClassError
			c.report.Reason = derr.Error()
			report.Classes = append(report.Classes, c.report)
			continue
		}
		c.report.Status = ClassFreed
		c.report.BytesFreed = c.report.Bytes
		report.BytesFreed += c.report.Bytes
		report.Classes = append(report.Classes, c.report)
	}

	// Final measurement (covers the last deletion; dry-run leaves the
	// before==after pair intact).
	if !req.DryRun {
		if usedAfter, totalAfter, err := e.usage(); err == nil && totalAfter > 0 {
			report.AfterUsedBytes, report.AfterTotalBytes = usedAfter, totalAfter
			report.AfterRatio = ratio(usedAfter, totalAfter)
		}
	}
	return report, nil
}

// measure computes the reclaimable bytes/files for one entry. For
// prefix classes only stale children (mtime older than MinAgeSecs)
// count, and their paths are returned for deletion.
func (e *Engine) measure(ctx context.Context, entry Entry) (bytes int64, count int, paths []string, err error) {
	if entry.Kind == EntryPrefixStale {
		cutoff := e.now().Add(-time.Duration(entry.MinAgeSecs) * time.Second)
		parent := filepath.Dir(entry.Path)
		prefix := filepath.Base(entry.Path)
		dirents, derr := os.ReadDir(parent)
		if derr != nil {
			if os.IsNotExist(derr) {
				return 0, 0, nil, nil
			}
			return 0, 0, nil, derr
		}
		for _, d := range dirents {
			if !strings.HasPrefix(d.Name(), prefix) {
				continue
			}
			info, ierr := d.Info()
			if ierr != nil {
				continue
			}
			if info.ModTime().After(cutoff) {
				continue // live build — never reap
			}
			p := filepath.Join(parent, d.Name())
			b, n := walkSum(ctx, p)
			bytes += b
			count += n
			paths = append(paths, p)
		}
		return bytes, count, paths, nil
	}

	fi, err := os.Lstat(entry.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil, nil
		}
		return 0, 0, nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// validateEntry already passed the resolved view; the literal
		// leaf being a symlink means the cache itself moved — measure
		// the resolved target, delete the symlink is NOT attempted.
		resolved, rerr := filepath.EvalSymlinks(entry.Path)
		if rerr != nil {
			return 0, 0, nil, nil
		}
		b, n := walkSum(ctx, resolved)
		return b, n, []string{entry.Path}, nil
	}
	b, n := walkSum(ctx, entry.Path)
	return b, n, []string{entry.Path}, nil
}

// delete removes one class's measured paths. It returns the class's
// measured byte figure (the volume-level before/after statfs pair in
// the Report is the accounting truth; per-class precision is the
// measurement, flagged error on any unlink failure rather than
// mis-reported).
func (e *Engine) delete(paths []string) (int64, error) {
	var firstErr error
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return 0, firstErr
}

// walkSum sums file sizes and file count under root (root itself
// excluded from count). ctx-cancellation between entries keeps a huge
// modcache walk bounded by the request deadline.
func walkSum(ctx context.Context, root string) (int64, int) {
	var bytes int64
	var count int
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: under-count, never abort
		}
		if d.IsDir() {
			return nil
		}
		select {
		case <-ctx.Done():
			return filepath.SkipAll
		default:
		}
		if info, ierr := d.Info(); ierr == nil {
			bytes += info.Size()
			count++
		}
		return nil
	})
	return bytes, count
}

// clampTarget defaults and bounds the requested target ratio.
func clampTarget(r float64) float64 {
	if r == 0 {
		return DefaultTargetRatio
	}
	if r < MinTargetRatio {
		return MinTargetRatio
	}
	if r > MaxTargetRatio {
		return MaxTargetRatio
	}
	return r
}

// ratio computes used/total; 0 when unknown (fail-safe: never looks
// "above target" spuriously).
func ratio(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total)
}

// ValidateProductionManifests is a wiring-time self-check: every
// compiled-in manifest must pass its own boundary validation (fail the
// process on a bad manifest — a manifest bug must be loud at boot, not
// a per-request refusal in production).
func ValidateProductionManifests() error {
	for base, m := range manifests {
		if err := ValidateManifest(m); err != nil {
			return fmt.Errorf("runtime base %s: %w", base, err)
		}
	}
	return nil
}
