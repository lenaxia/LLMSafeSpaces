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

// pathSize pairs one deletion-candidate path with the measured share
// of the class's bytes it carries — the unit of bytesFreed honesty.
type pathSize struct {
	path  string
	bytes int64
}

// Engine executes allowlisted cache recovery. Zero filesystem writes
// outside the validated unlinks themselves (owner ruling on #1601: the
// rescue path must work at 100% full — no temp files, no report
// persistence, response-only).
type Engine struct {
	manifest Manifest
	usage    UsageFunc
	now      func() time.Time
	// scanOpen snapshots the pod's open file handles once per sweep
	// (#1744: never delete anything with an open handle). Seam-injected
	// in tests.
	scanOpen func() openHandles
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
		scanOpen: scanOpenHandles,
		volume:   "/workspace",
	}
}

// SetClock overrides the staleness clock (tests).
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

// setOpenHandleScan overrides the open-handle snapshot source (tests).
func (e *Engine) setOpenHandleScan(f func() openHandles) { e.scanOpen = f }

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
		// Non-nil by construction on every path (review r1 F2/F4): a
		// nil slice marshals to JSON null and crashes JSON-shape-naive
		// consumers; "classes":[] is the wire contract.
		Classes: []ClassReport{},
	}

	// Idempotent fast path: already at/below target — nothing to do,
	// and (dry-run or not) nothing is measured or deleted.
	if report.BeforeRatio <= target {
		report.AlreadyBelowTarget = true
		return report, nil
	}

	// One open-handle snapshot for the whole sweep (#1744: never
	// delete anything with an open handle). Taken AFTER the
	// below-target fast path so a healthy volume pays nothing.
	// Snapshot semantics (review r2): a handle opened AFTER this
	// instant is invisible — the same inherent TOCTOU class as the
	// measure→delete window below; the report's statfs before/after
	// pair remains the accounting truth, and re-running recover once
	// the holder closes is the documented remediation.
	oh := e.scanOpen()

	// Measure every class (read-only), largest-first.
	type candidate struct {
		entry  Entry
		report ClassReport
		paths  []pathSize // prefix: one per stale child; symlink: [link 0, target n]; dir: [dir n]
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
		sizes, held, err := e.measure(ctx, entry, oh)
		var bytes int64
		for _, ps := range sizes {
			bytes += ps.bytes
		}
		// The deletion set keeps EVERY measured path (zero-byte link
		// inodes ride along for cleanup); candidacy — and Bytes/Entries
		// — counts only reclaimable data, keeping not_present meaning
		// "nothing to free" (the pre-refactor count==0 contract).
		reclaimable := 0
		for _, ps := range sizes {
			if ps.bytes > 0 {
				reclaimable++
			}
		}
		cr.Bytes, cr.Entries = bytes, reclaimable
		// A fully-held class (dir arm) reports the PINNED bytes as its
		// measured figure — the operator's "why is the disk still full"
		// answer is the whole point of surfacing in_use (#1744).
		if held.count > 0 && len(sizes) == 0 {
			cr.Bytes = held.bytes
		}
		switch {
		case err != nil:
			cr.Status = ClassError
			cr.Reason = err.Error()
		case held.count > 0 && reclaimable == 0:
			cr.Status = ClassInUse
			cr.Reason = held.reason
		case reclaimable == 0:
			cr.Status = ClassNotPresent
		case bytes < entry.MinBytes:
			// #1744 size floor: a tree below the floor is a partial
			// download or debris, not the calibrated full set.
			cr.Status = ClassNotPresent
			cr.Reason = fmt.Sprintf("below size floor: %d bytes measured < %d floor", bytes, entry.MinBytes)
		default:
			if held.count > 0 {
				cr.Reason = fmt.Sprintf("%d stale path(s) (%d bytes) %s — skipped", held.count, held.bytes, held.reason)
			}
			candidates = append(candidates, candidate{entry: entry, report: cr, paths: sizes})
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

		freed, derr := e.delete(c.paths)
		if derr != nil {
			c.report.Status = ClassError
			c.report.Reason = derr.Error()
			report.Classes = append(report.Classes, c.report)
			continue
		}
		// Per-path honesty (reviews r2/r3): bytesFreed counts only the
		// measured shares whose paths were still present at delete
		// time. Aliased-away or externally-removed paths (any position
		// in the set — prefix children included) contribute zero, so
		// no class can claim bytes it did not actually unlink.
		if freed == 0 {
			c.report.Status = ClassNotPresent
			c.report.Reason = "already removed (aliased with an earlier class or externally deleted)"
			c.report.Bytes = 0
			c.report.Entries = 0
			report.Classes = append(report.Classes, c.report)
			continue
		}
		c.report.Status = ClassFreed
		c.report.BytesFreed = freed
		if freed < c.report.Bytes {
			c.report.Reason = fmt.Sprintf("partial: %d of %d measured bytes were still present at delete time", freed, c.report.Bytes)
		}
		report.BytesFreed += freed
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

// heldShare is the measured-but-pinned portion of a class: paths this
// sweep must not touch (#1744). For dir classes count is 0 or 1 and
// bytes carries the whole tree; for prefix classes it accumulates the
// held stale children.
type heldShare struct {
	count  int
	bytes  int64
	reason string
}

// measure computes the per-path reclaimable sizes for one entry. The
// returned pathSize list is BOTH the measured figure AND the deletion
// set — bytesFreed honesty (reviews r2/r3) rests on the two never
// diverging. For prefix classes only stale children (mtime older than
// MinAgeSecs, no open handles) count, one pathSize each; held children
// are excluded from the deletion set and reported in the heldShare.
// Dir classes with a MinAgeSecs gate are held whole while their own
// mtime is younger than the gate, and held whole on any open handle
// under them (a tree cannot be half-RemoveAll'd).
func (e *Engine) measure(ctx context.Context, entry Entry, oh openHandles) ([]pathSize, heldShare, error) {
	var held heldShare
	type ps = pathSize
	if entry.Kind == EntryPrefixStale {
		cutoff := e.now().Add(-time.Duration(entry.MinAgeSecs) * time.Second)
		parent := filepath.Dir(entry.Path)
		prefix := filepath.Base(entry.Path)
		dirents, derr := os.ReadDir(parent)
		if derr != nil {
			if os.IsNotExist(derr) {
				return nil, held, nil
			}
			return nil, held, derr
		}
		var out []ps
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
			if oh.holds(p) {
				b, _ := walkSum(ctx, p)
				held.count++
				held.bytes += b
				continue // open handle under it — never reap
			}
			b, _ := walkSum(ctx, p)
			out = append(out, ps{path: p, bytes: b})
		}
		if held.count > 0 {
			held.reason = "held by open file handles"
			if oh.blocked() {
				held.reason = oh.holdReason()
			}
		}
		return out, held, nil
	}

	fi, err := os.Lstat(entry.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, held, nil
		}
		return nil, held, err
	}
	// #1744 hot-directory gate: a gated dir class whose own mtime is
	// younger than MinAgeSecs is likely mid-population — held whole.
	if entry.MinAgeSecs > 0 && fi.ModTime().After(e.now().Add(-time.Duration(entry.MinAgeSecs)*time.Second)) {
		b, _ := walkSum(ctx, entry.Path)
		return nil, heldShare{count: 1, bytes: b, reason: fmt.Sprintf("directory modified within %ds (hot — likely mid-install or in active use)", entry.MinAgeSecs)}, nil
	}
	target := entry.Path
	if fi.Mode()&os.ModeSymlink != 0 {
		// In-root symlinked cache (validateEntry already proved the
		// resolved target stays inside the same root). Measure AND
		// delete must agree (review r1 F3): the bytes come from the
		// TARGET tree, so the deletion set is [link 0, target n] —
		// RemoveAll on the literal alone would unlink just the link
		// inode and report the target's bytes as freed.
		resolved, rerr := filepath.EvalSymlinks(entry.Path)
		if rerr != nil {
			return nil, held, nil
		}
		// TOCTOU close (review r2 robustness): this FRESH resolution is
		// what enters the deletion set — re-run the full boundary
		// validation on it before it does. A retargeted link between
		// validateEntry and here dies here, loudly.
		if err := validateEntry(e.manifest, Entry{Class: entry.Class, Path: resolved, Kind: EntryDir}); err != nil {
			return nil, held, err
		}
		target = resolved
	}
	if oh.holds(entry.Path) || oh.holds(target) {
		b, _ := walkSum(ctx, target)
		reason := "open file handles under the cache tree"
		if oh.blocked() {
			reason = oh.holdReason()
		}
		return nil, heldShare{count: 1, bytes: b, reason: reason}, nil
	}
	b, _ := walkSum(ctx, target)
	if target != entry.Path {
		return []ps{{path: entry.Path}, {path: target, bytes: b}}, held, nil
	}
	return []ps{{path: entry.Path, bytes: b}}, held, nil
}

// delete removes one class's measured paths and returns the measured
// share whose paths were still present at Lstat time (per path — any
// position in the set): a path absent at delete time (aliasing,
// external removal between measure and delete) contributes zero, so
// bytesFreed claims only shares present when THIS sweep unlinked them.
// (A microsecond window remains between the Lstat and the RemoveAll —
// inherent to the approach; the volume-level before/after statfs pair
// in the Report is the accounting truth.) firstErr carries any unlink
// failure.
func (e *Engine) delete(paths []pathSize) (freed int64, firstErr error) {
	for _, p := range paths {
		if _, serr := os.Lstat(p.path); serr != nil {
			continue // absent: aliased/externally removed — no credit
		}
		if err := os.RemoveAll(p.path); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		freed += p.bytes
	}
	return freed, firstErr
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
