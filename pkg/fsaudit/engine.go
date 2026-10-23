// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package fsaudit

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// ErrBusy is returned when another audit is in flight on this engine
// (the #1601 discipline — concurrent scans would double-read and race
// the budget accounting).
var ErrBusy = errors.New("fs audit already in progress")

// Engine scans a base tree for the #1808 zero-fill signature. Zero
// writes anywhere (response-only ruling); bounded by the request caps.
type Engine struct {
	base string
	mu   sync.Mutex
}

// NewEngine builds an engine over base (production: /workspace — the
// PVC subPath every workspace view shares).
func NewEngine(base string) *Engine {
	return &Engine{base: base}
}

// resolveRoot grounds req.Root inside the base: prefixing "/" before
// Clean collapses every ".."-escape harmlessly at the root
// (Clean("/a/../../etc") = "/etc" → joined under base), so NO request
// can point the scan outside the base. Deny-by-default without a
// rejection path to get wrong.
func (e *Engine) resolveRoot(root string) string {
	if root == "" || root == "." {
		return e.base
	}
	grounded := filepath.Clean("/" + strings.TrimPrefix(filepath.ToSlash(root), "/"))
	return filepath.Join(e.base, filepath.FromSlash(grounded))
}

// Audit runs one scan. See the package doc for the signature and the
// laws. Canceled contexts return an error — an incomplete scan never
// masquerades as a clean report.
func (e *Engine) Audit(ctx context.Context, req Request) (Report, error) {
	if !e.mu.TryLock() {
		return Report{}, ErrBusy
	}
	defer e.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Report{}, err
	}

	maxFile := clampCap(req.MaxFileBytes, DefaultMaxFileBytes)
	maxTotal := clampCap(req.MaxTotalBytes, DefaultMaxTotalBytes)

	report := Report{
		Root:      displayRoot(req.Root),
		NulFilled: []FileReport{},
		Suspect:   []FileReport{},
		Errors:    []FileReport{},
	}

	s := scanner{
		report:     &report,
		maxFile:    maxFile,
		budgetLeft: maxTotal,
		zeroChunk:  make([]byte, ChunkSize),
		chunk:      make([]byte, ChunkSize),
	}
	root := e.resolveRoot(req.Root)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable directory: report the walk error as a file-level
			// error entry (path = the dir) and CONTINUE — under-report,
			// never abort.
			if rel, rerr := filepath.Rel(e.base, path); rerr == nil {
				s.addList(&report.Errors, FileReport{Path: filepath.ToSlash(rel), Verdict: VerdictError, Reason: err.Error()})
				report.ErrorCount++
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			report.FilesSkipped++ // symlink/fifo/socket/device — never opened
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			rel, _ := filepath.Rel(e.base, path)
			s.addList(&report.Errors, FileReport{Path: filepath.ToSlash(rel), Verdict: VerdictError, Reason: ierr.Error()})
			report.ErrorCount++
			return nil
		}
		report.FilesScanned++
		rel, rerr := filepath.Rel(e.base, path)
		if rerr != nil {
			rel = path
		}
		s.classify(filepath.ToSlash(rel), path, info)
		if s.budgetLeft <= 0 {
			report.Truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		// ctx cancellation (the only error that escapes the walk fn):
		// incomplete = error, never a partial-clean report.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Report{}, ctxErr
		}
		return Report{}, err
	}
	if s.budgetLeft <= 0 {
		// Budget exhausted on the LAST file: nothing skipped after, but
		// the tree past the in-flight file may be unaudited.
		report.Truncated = true
	}
	return report, nil
}

// scanner carries the per-scan mutable state.
type scanner struct {
	report     *Report
	maxFile    int64
	budgetLeft int64
	zeroChunk  []byte
	chunk      []byte
}

// classify reads one regular file under the caps and records its
// verdict. Bytes accounting: only bytes actually read count against the
// budgets (the early exit is the whole point).
func (s *scanner) classify(rel, path string, info os.FileInfo) {
	size := info.Size()
	if size == 0 {
		s.report.FilesSkipped++ // zero-size: never a scan subject
		s.report.FilesScanned--
		return
	}

	f, err := os.Open(path) //nolint:gosec // path came from the walk under the engine base
	if err != nil {
		s.addList(&s.report.Errors, FileReport{Path: rel, Size: size, AllocatedBytes: allocatedBytes(info), Verdict: VerdictError, Reason: err.Error()})
		s.report.ErrorCount++
		return
	}
	defer func() { _ = f.Close() }()

	// The read limit: the file cap AND the remaining total budget.
	limit := s.maxFile
	if s.budgetLeft < limit {
		limit = s.budgetLeft
	}
	if limit > size {
		limit = size
	}

	var read int64
	allNul := true
	for read < limit {
		want := int64(ChunkSize)
		if limit-read < want {
			want = limit - read
		}
		n, rerr := f.Read(s.chunk[:want])
		if n > 0 {
			read += int64(n)
			if !bytes.Equal(s.chunk[:n], s.zeroChunk[:n]) {
				allNul = false
				break // early exit: a live byte exists
			}
		}
		if rerr != nil {
			break // EOF or read error — either way, no more content
		}
	}
	s.budgetLeft -= read
	s.report.BytesRead += read

	fr := FileReport{Path: rel, Size: size, AllocatedBytes: allocatedBytes(info)}
	switch {
	case !allNul:
		// clean — not listed (a clean list would be the whole tree)
	case read == size:
		// 100% of the file read, all NUL: the sparseness decides.
		if fr.AllocatedBytes >= size {
			fr.Verdict = VerdictNulFilled
			s.report.NulFilledCount++
			s.addList(&s.report.NulFilled, fr)
		} else {
			fr.Verdict = VerdictSparse
			s.report.SparseCount++
		}
	case s.maxFile <= read && read < size:
		// Per-file cap hit while still all-NUL: unconfirmed.
		fr.Verdict = VerdictSuspect
		fr.Reason = "all-NUL within the per-file read cap; 100%-NUL unconfirmed (raise maxFileBytes to confirm)"
		s.report.SuspectCount++
		s.addList(&s.report.Suspect, fr)
	default:
		// Total budget exhausted mid-file: the scan is truncated; the
		// file itself is unscanned, not suspect.
		fr.Verdict = VerdictSkipped
		fr.Reason = "total read budget exhausted at this file; scan truncated"
		s.report.FilesSkipped++
	}
}

// addList appends honoring MaxListEntries (counts stay exact upstream).
func (s *scanner) addList(list *[]FileReport, fr FileReport) {
	if len(*list) >= MaxListEntries {
		s.report.ListTruncated = true
		return
	}
	*list = append(*list, fr)
}

// allocatedBytes reports st_blocks*512 (the sparseness evidence).
// Non-posix fallback: assume non-sparse (size) — fail toward flagging,
// never toward hiding corruption.
func allocatedBytes(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

// clampCap defaults and floors a requested cap (at least one chunk,
// never a zero/negative budget that would truncate before reading).
func clampCap(v, def int64) int64 {
	if v <= 0 {
		return def
	}
	if v < ChunkSize {
		return ChunkSize
	}
	return v
}

// displayRoot echoes the requested root for the report ("" → ".").
func displayRoot(root string) string {
	if root == "" {
		return "."
	}
	return root
}
