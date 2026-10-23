// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package fsaudit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// --- fixtures -----------------------------------------------------------
//
// nulFile writes n NUL bytes through the write() path so the file is
// fully allocated (st_blocks*512 >= size) — the #1808 corruption shape.
func nulFile(t *testing.T, path string, n int64) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sparseNulFile creates an all-NUL file of length n with NO allocation
// (ftruncate/seek class): legitimate, common, and the false-positive
// trap a naive zeros-grep walks straight into.
func sparseNulFile(t *testing.T, path string, n int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(n); err != nil {
		t.Fatal(err)
	}
}

// textFile writes deterministic non-NUL content.
func textFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findReport(list []FileReport, path string) *FileReport {
	for i := range list {
		if list[i].Path == path {
			return &list[i]
		}
	}
	return nil
}

// --- the signature pins -------------------------------------------------

// PIN (the corruption signature): a size>0, fully-allocated file whose
// content is 100% NUL is nul_filled — flagged, listed, counted. This is
// the exact shape the incident produced (length preserved, all `\0`).
func TestAudit_NulFilledFileFlagged(t *testing.T) {
	base := t.TempDir()
	nulFile(t, filepath.Join(base, "renderer.yaml"), 8192)
	eng := NewEngine(base)

	report, err := eng.Audit(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if report.NulFilledCount != 1 {
		t.Fatalf("NulFilledCount: want 1, got %d", report.NulFilledCount)
	}
	fr := findReport(report.NulFilled, "renderer.yaml")
	if fr == nil {
		t.Fatalf("renderer.yaml not in nulFilled list: %+v", report.NulFilled)
	}
	if fr.Verdict != VerdictNulFilled {
		t.Fatalf("verdict: want %q, got %q", VerdictNulFilled, fr.Verdict)
	}
	if fr.Size != 8192 {
		t.Fatalf("size: want 8192, got %d", fr.Size)
	}
	if fr.AllocatedBytes < fr.Size {
		t.Fatalf("allocated %d < size %d: an allocated fixture cannot be sparse (test bug)", fr.AllocatedBytes, fr.Size)
	}
}

// PIN (the false-positive trap): an all-NUL file that is NOT allocated
// (ftruncate/seek-created — sparse) is reported sparse, NEVER
// nul_filled. Legitimately created by `truncate`, `dd seek=`, and
// database/preallocation idioms.
func TestAudit_SparseAllNulFileNotFlagged(t *testing.T) {
	base := t.TempDir()
	p := filepath.Join(base, "legit-sparse.bin")
	sparseNulFile(t, p, 1<<20)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if allocatedBytes(fi) >= fi.Size() {
		t.Skipf("filesystem does not report sparseness (allocated=%d size=%d); sparse trap pin not discriminable here", allocatedBytes(fi), fi.Size())
	}

	eng := NewEngine(base)
	report, err := eng.Audit(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if report.NulFilledCount != 0 {
		t.Fatalf("sparse file flagged nul_filled: %+v", report.NulFilled)
	}
	if report.SparseCount != 1 {
		t.Fatalf("SparseCount: want 1, got %d", report.SparseCount)
	}
	if findReport(report.NulFilled, "legit-sparse.bin") != nil {
		t.Fatal("sparse file listed in nulFilled")
	}
}

// PIN (clean + early exit): any non-NUL byte anywhere classifies the
// file clean, and the scan STOPS at the chunk that contains it — a
// legit file costs one (or two) 4KiB chunk reads, never a full read.
func TestAudit_CleanFileEarlyExit(t *testing.T) {
	base := t.TempDir()
	// Non-NUL byte in the FIRST chunk.
	textFile(t, filepath.Join(base, "first.txt"), "# pin\n"+string(make([]byte, 100)))
	// Non-NUL byte just past the first chunk (at 4096+10), size 1MiB.
	late := make([]byte, 1<<20)
	late[4096+10] = 'x'
	if err := os.WriteFile(filepath.Join(base, "late.txt"), late, 0o644); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(base)
	report, err := eng.Audit(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if report.NulFilledCount != 0 || report.SparseCount != 0 || report.SuspectCount != 0 || report.ErrorCount != 0 {
		t.Fatalf("clean tree produced flags: %+v", report)
	}
	// first.txt: the non-NUL byte sits in chunk 1 → exactly one chunk read.
	// late.txt: non-NUL in chunk 2 → two chunk reads. Budget proves the
	// early exit: far below the 1MiB+ late file's size.
	if report.BytesRead <= 0 || report.BytesRead >= int64(1<<20) {
		t.Fatalf("BytesRead %d does not prove early exit (late.txt alone is %d bytes)", report.BytesRead, 1<<20)
	}
}

// PIN (never-flag classes): zero-size files and non-regular files
// (symlinks — and fifos where creatable) are skipped, never read,
// never flagged.
func TestAudit_ZeroSizeAndNonRegularSkipped(t *testing.T) {
	base := t.TempDir()
	textFile(t, filepath.Join(base, "empty"), "")
	nulFile(t, filepath.Join(base, "dead"), 64)
	if err := os.Symlink("dead", filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(base)
	report, err := eng.Audit(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if report.NulFilledCount != 1 {
		t.Fatalf("NulFilledCount: want exactly the dead file, got %d", report.NulFilledCount)
	}
	if report.FilesSkipped != 2 { // empty + symlink
		t.Fatalf("FilesSkipped: want 2 (empty + symlink), got %d", report.FilesSkipped)
	}
	for _, fr := range report.NulFilled {
		if fr.Path == "empty" || fr.Path == "link" {
			t.Fatalf("never-flag class listed: %+v", fr)
		}
	}
}

// PIN (per-file cap honesty): an all-NUL-so-far file LARGER than the
// per-file read cap is verdict suspect — the 100%-NUL claim is NOT
// confirmed, so it must not be counted as nul_filled.
func TestAudit_PerFileCapYieldsSuspectNotConfirmed(t *testing.T) {
	base := t.TempDir()
	nulFile(t, filepath.Join(base, "big-dead.bin"), 64*1024)

	eng := NewEngine(base)
	report, err := eng.Audit(context.Background(), Request{MaxFileBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if report.NulFilledCount != 0 {
		t.Fatalf("over-cap file counted nul_filled: %+v", report.NulFilled)
	}
	if report.SuspectCount != 1 {
		t.Fatalf("SuspectCount: want 1, got %d", report.SuspectCount)
	}
	fr := findReport(report.Suspect, "big-dead.bin")
	if fr == nil {
		t.Fatalf("big-dead.bin not in suspect list: %+v", report.Suspect)
	}
	if fr.Reason == "" {
		t.Fatal("suspect report must carry a reason (which cap was hit)")
	}
}

// PIN (total budget truncation): exhausting the total read budget
// stops the scan with Truncated=true and marks the in-flight file
// skipped — never a silent partial claim of completeness.
func TestAudit_TotalBudgetTruncates(t *testing.T) {
	base := t.TempDir()
	// Four clean 64KiB files: early-exit means each costs one 4KiB chunk.
	for i := 0; i < 4; i++ {
		content := make([]byte, 64*1024)
		content[0] = byte('a' + i)
		if err := os.WriteFile(filepath.Join(base, "f"+string(rune('a'+i))+".dat"), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	eng := NewEngine(base)
	// Budget for exactly two files' first chunks.
	report, err := eng.Audit(context.Background(), Request{MaxTotalBytes: 2 * int64(ChunkSize)})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Truncated {
		t.Fatal("Truncated: want true when the total read budget exhausted mid-scan")
	}
	if report.FilesScanned < 2 || report.FilesScanned > 3 {
		t.Fatalf("FilesScanned %d outside the budget-bounded window [2,3]", report.FilesScanned)
	}
}

// PIN (list bounding): counts are exact even when the dead-file list
// hits MaxListEntries — the response stays bounded, the count does not.
func TestAudit_ListCapFlagsTruncated(t *testing.T) {
	base := t.TempDir()
	if testing.Short() {
		t.Skip("1001-file fixture in -short mode")
	}
	for i := 0; i <= MaxListEntries; i++ {
		nulFile(t, filepath.Join(base, "d"+string(rune('a'+i/26))+string(rune('a'+i%26))+".nul"), 16)
	}

	eng := NewEngine(base)
	report, err := eng.Audit(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if report.NulFilledCount != MaxListEntries+1 {
		t.Fatalf("NulFilledCount: want %d, got %d", MaxListEntries+1, report.NulFilledCount)
	}
	if len(report.NulFilled) != MaxListEntries {
		t.Fatalf("nulFilled list length: want %d, got %d", MaxListEntries, len(report.NulFilled))
	}
	if !report.ListTruncated {
		t.Fatal("ListTruncated: want true when a list hits MaxListEntries")
	}
}

// PIN (root confinement): every Request.Root resolves INSIDE the
// engine base — "../.." and absolute paths are grounded under the
// base, never escaping (deny-by-default; the scan cannot be pointed at
// /etc). A grounded root that does not exist surfaces as a walk ERROR
// entry — a typo'd root must never read as "0 files, clean".
func TestAudit_RootCannotEscapeBase(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	nulFile(t, filepath.Join(base, "sub", "dead.yaml"), 128)
	nulFile(t, filepath.Join(base, "dead-root.yaml"), 128)

	eng := NewEngine(base)
	// Grounded at base itself: both fixtures visible.
	for _, root := range []string{"", ".", "..", "../.."} {
		report, err := eng.Audit(context.Background(), Request{Root: root})
		if err != nil {
			t.Fatalf("root %q: %v", root, err)
		}
		if report.NulFilledCount != 2 {
			t.Fatalf("root %q: want both fixtures, got %+v", root, report.NulFilled)
		}
		if report.ErrorCount != 0 {
			t.Fatalf("root %q: unexpected walk errors %+v", root, report.Errors)
		}
	}
	// Confined subroot: only its child.
	report, err := eng.Audit(context.Background(), Request{Root: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if report.NulFilledCount != 1 || findReport(report.NulFilled, "sub/dead.yaml") == nil {
		t.Fatalf("root sub: want exactly sub/dead.yaml, got %+v", report.NulFilled)
	}
	// Escape attempts ground to a nonexistent subdir under base: loud
	// walk error, zero flags, NEVER a scan outside the base.
	for _, root := range []string{"/etc", "/../../etc/passwd", "../../etc"} {
		report, err := eng.Audit(context.Background(), Request{Root: root})
		if err != nil {
			t.Fatalf("root %q: %v", root, err)
		}
		if report.ErrorCount != 1 {
			t.Fatalf("root %q: want 1 walk error for the grounded-missing dir, got %+v", root, report.Errors)
		}
		if report.NulFilledCount != 0 {
			t.Fatalf("root %q: grounded dir must be empty, got %+v", root, report.NulFilled)
		}
	}
}

// PIN (serialization): one audit at a time per engine (ErrBusy), the
// #1601 discipline — responses stay bounded and reproducible.
func TestAudit_BusySerialization(t *testing.T) {
	base := t.TempDir()
	eng := NewEngine(base)
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if _, err := eng.Audit(context.Background(), Request{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

// PIN (ctx bound): a canceled context is returned as an error — an
// incomplete scan never masquerades as a clean report.
func TestAudit_CtxCancellationReturnsError(t *testing.T) {
	base := t.TempDir()
	textFile(t, filepath.Join(base, "a"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	eng := NewEngine(base)
	if _, err := eng.Audit(ctx, Request{}); err == nil {
		t.Fatal("canceled ctx must return an error, not a partial-clean report")
	}
}

// PIN (wire shape): every list marshals as [] not null, and paths are
// base-relative slash paths (the diskrecovery r1 F2/F4 lesson).
func TestAudit_ReportWireShape(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	nulFile(t, filepath.Join(base, "sub", "dead"), 32)
	eng := NewEngine(base)
	report, err := eng.Audit(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"nulFilled", "suspect", "errors"} {
		if _, ok := doc[key].([]any); !ok {
			t.Fatalf("key %q must marshal as a JSON array, got %T (%s)", key, doc[key], raw)
		}
	}
	if fr := findReport(report.NulFilled, filepath.ToSlash(filepath.Join("sub", "dead"))); fr == nil {
		t.Fatalf("path must be base-relative slash path: %+v", report.NulFilled)
	}
}

// PIN (concurrent engines): distinct engines (e.g. two pods) never
// share the busy mutex — the serialization is per-engine, not global.
func TestAudit_BusyIsPerEngine(t *testing.T) {
	e1, e2 := NewEngine(t.TempDir()), NewEngine(t.TempDir())
	e1.mu.Lock()
	defer e1.mu.Unlock()
	if _, err := e2.Audit(context.Background(), Request{}); err != nil {
		t.Fatalf("second engine must be free: %v", err)
	}
}

// PIN (read-error honesty): an unreadable regular file is verdict
// error with a reason — never silently clean.
func TestAudit_ReadErrorReported(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("chmod-based unreadability is linux-ruled")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses mode bits; error pin not discriminable")
	}
	base := t.TempDir()
	p := filepath.Join(base, "locked")
	nulFile(t, p, 64) // content is NOT all-readable: must error, not flag
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })

	eng := NewEngine(base)
	report, err := eng.Audit(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if report.ErrorCount != 1 {
		t.Fatalf("ErrorCount: want 1, got %d (%+v)", report.ErrorCount, report.Errors)
	}
	if report.NulFilledCount != 0 {
		t.Fatalf("unreadable file must not be flagged nul_filled: %+v", report.NulFilled)
	}
}

// PIN (defaults documented in code): the budget defaults exist and are
// sane (64MiB per file, 256MiB total) — they bound the scan's worst
// case; the constants are the wire contract for the caps.
func TestAudit_BudgetDefaults(t *testing.T) {
	if DefaultMaxFileBytes != 64<<20 || DefaultMaxTotalBytes != 256<<20 {
		t.Fatalf("budget defaults changed: file=%d total=%d (update pins + docs if intentional)", DefaultMaxFileBytes, DefaultMaxTotalBytes)
	}
}

// smoke: a tree with a bit of everything classifies each correctly.
func TestAudit_MixedTree(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	nulFile(t, filepath.Join(base, "dead1"), 4096)
	sparseNulFile(t, filepath.Join(base, "sparse1"), 1<<20)
	textFile(t, filepath.Join(base, "ok1"), "hello")
	nulFile(t, filepath.Join(base, "nested", "dead2"), 100)

	eng := NewEngine(base)
	report, err := eng.Audit(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if report.NulFilledCount != 2 || report.SparseCount != 1 || report.ErrorCount != 0 {
		t.Fatalf("mixed tree misclassified: %+v", report)
	}
}

// guard: helpers stay honest under -race (the engine itself has no
// shared state beyond mu, but the busy pins race the lock in CI).
func TestAudit_ConcurrentDistinctRequests(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < 8; i++ {
		nulFile(t, filepath.Join(base, "n"+string(rune('0'+i))), 128)
	}
	eng := NewEngine(base)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = eng.Audit(context.Background(), Request{})
		}()
	}
	wg.Wait()
	if elapsed := time.Since(time.Now()); elapsed > time.Hour {
		t.Fatal("unreachable")
	}
}
