// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package diskrecovery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeUsage is a scriptable statfs: tests set used bytes between calls
// to simulate deletions freeing space.
type fakeUsage struct {
	total int64
	used  int64
	err   error
	calls int
}

func (f *fakeUsage) get() (int64, int64, error) {
	f.calls++
	return f.used, f.total, f.err
}

// seedFile writes n bytes at path (n files of size bytes each under a
// dir) and returns the bytes written.
func seedTree(t *testing.T, root string, files, size int) int64 {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var total int64
	for i := 0; i < files; i++ {
		p := filepath.Join(root, "f"+string(rune('a'+i))+".dat")
		buf := make([]byte, size)
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		total += int64(size)
	}
	return total
}

// testManifest builds a manifest over temp dirs mirroring the
// production shape: a parent cache root (.cache) with two entries and
// a self root (npm), so ordering/stop logic is exercised multi-class.
func testManifest(t *testing.T) (Manifest, string, string) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home", "sandbox")
	cacheRoot := filepath.Join(home, ".cache")
	npmRoot := filepath.Join(home, ".npm")
	for _, d := range []string{cacheRoot, npmRoot, filepath.Join(dir, "workspace")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := Manifest{
		Base: "test",
		Roots: []CacheRoot{
			{Path: cacheRoot, Kind: RootParent},
			{Path: npmRoot, Kind: RootSelf},
		},
		Entries: []Entry{
			{Class: "go-build-cache", Path: filepath.Join(cacheRoot, "go-build"), Kind: EntryDir},
			{Class: "pip-cache", Path: filepath.Join(cacheRoot, "pip"), Kind: EntryDir},
			{Class: "npm-cache", Path: npmRoot, Kind: EntryDir},
		},
	}
	return m, cacheRoot, npmRoot
}

// PIN (dry-run deletes nothing): a dry-run report lists would_free
// classes with byte figures and the filesystem is byte-for-byte
// untouched.
func TestDryRunDeletesNothing(t *testing.T) {
	m, cacheRoot, npmRoot := testManifest(t)
	goBytes := seedTree(t, filepath.Join(cacheRoot, "go-build"), 3, 1000)
	npmBytes := seedTree(t, npmRoot, 2, 500)
	usage := &fakeUsage{total: 10000, used: 9600} // 96%
	e := NewEngine(m, usage.get)

	rep, err := e.Recover(context.Background(), Request{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.DryRun != true || rep.AlreadyBelowTarget {
		t.Fatalf("expected dry-run above target, got %+v", rep)
	}
	if rep.BytesFreed != 0 {
		t.Fatalf("dry-run must report zero freed, got %d", rep.BytesFreed)
	}
	var wouldFree int
	for _, c := range rep.Classes {
		if c.Status == ClassWouldFree {
			wouldFree++
			if c.BytesFreed != 0 {
				t.Fatalf("dry-run class %s must not claim bytesFreed", c.Class)
			}
		}
		if c.Status == ClassFreed {
			t.Fatalf("dry-run must never report freed: %+v", c)
		}
	}
	if wouldFree != 2 {
		t.Fatalf("expected 2 would_free classes (go-build + npm seeded), got %d in %+v", wouldFree, rep.Classes)
	}
	var wantBytes = goBytes + npmBytes
	var gotBytes int64
	for _, c := range rep.Classes {
		if c.Status == ClassWouldFree {
			gotBytes += c.Bytes
		}
	}
	if gotBytes != wantBytes {
		t.Fatalf("dry-run bytes: want %d, got %d", wantBytes, gotBytes)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "go-build")); err != nil {
		t.Fatalf("dry-run must not delete: %v", err)
	}
	if _, err := os.Stat(npmRoot); err != nil {
		t.Fatalf("dry-run must not delete: %v", err)
	}
}

// PIN (free ENOUGH): recovery stops once below target — smaller
// classes report skipped_target_met, nothing beyond the needed classes
// is deleted.
func TestFreeEnoughStopsAtTarget(t *testing.T) {
	m, cacheRoot, npmRoot := testManifest(t)
	seedTree(t, filepath.Join(cacheRoot, "go-build"), 3, 1000) // 3000
	seedTree(t, filepath.Join(cacheRoot, "pip"), 2, 100)       // 200
	seedTree(t, npmRoot, 2, 500)                               // 1000

	// 10_000 total; usage script: pre-read 9600, loop check #1 (before
	// go-build) still 9600 → delete go-build (−3000); loop check #2
	// (before npm): 6600 → below 0.85·10000=8500 → stop; pip + npm
	// must remain.
	usage := &fakeUsage{total: 10000, used: 9600}
	usageAfter := int64(6600)
	e := NewEngine(m, func() (int64, int64, error) {
		u, tot, _ := usage.get()
		if usage.calls > 2 {
			return usageAfter, tot, nil
		}
		return u, tot, nil
	})

	rep, err := e.Recover(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.StoppedEarly {
		t.Fatalf("expected StoppedEarly after target met, got %+v", rep)
	}
	byClass := map[string]ClassReport{}
	for _, c := range rep.Classes {
		byClass[c.Class] = c
	}
	if byClass["go-build-cache"].Status != ClassFreed {
		t.Fatalf("largest class must free, got %+v", byClass["go-build-cache"])
	}
	if byClass["pip-cache"].Status != ClassSkippedTargetMet {
		t.Fatalf("smaller class must be skipped_target_met, got %+v", byClass["pip-cache"])
	}
	if byClass["npm-cache"].Status != ClassSkippedTargetMet {
		t.Fatalf("npm class must be skipped_target_met, got %+v", byClass["npm-cache"])
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "pip")); err != nil {
		t.Fatalf("skipped class must remain on disk: %v", err)
	}
	if _, err := os.Stat(npmRoot); err != nil {
		t.Fatalf("skipped class must remain on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "go-build")); !os.IsNotExist(err) {
		t.Fatalf("freed class must be gone, got %v", err)
	}
}

// PIN (idempotent / already below target): at/below target the engine
// measures and deletes nothing — the second click is a no-op report.
func TestAlreadyBelowTargetIsNoOp(t *testing.T) {
	m, cacheRoot, _ := testManifest(t)
	seedTree(t, filepath.Join(cacheRoot, "go-build"), 2, 100)
	usage := &fakeUsage{total: 10000, used: 5000} // 50% ≤ 85%
	e := NewEngine(m, usage.get)

	rep, err := e.Recover(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AlreadyBelowTarget {
		t.Fatalf("expected AlreadyBelowTarget, got %+v", rep)
	}
	if len(rep.Classes) != 0 {
		t.Fatalf("below-target report must not enumerate classes (no-op fast path), got %+v", rep.Classes)
	}
	if rep.BytesFreed != 0 || usage.calls != 1 {
		t.Fatalf("below-target must be a single-usage-read no-op, calls=%d freed=%d", usage.calls, rep.BytesFreed)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "go-build")); err != nil {
		t.Fatalf("below-target must not delete: %v", err)
	}
	// Idempotency: same call again, same result.
	rep2, err := e.Recover(context.Background(), Request{DryRun: true})
	if err != nil || !rep2.AlreadyBelowTarget {
		t.Fatalf("second (dry-run) call must be the same no-op, got %+v err=%v", rep2, err)
	}
}

// PIN (busy serialization): a second concurrent Recover returns
// ErrBusy rather than interleaving unlinks.
func TestConcurrentRecoverBusy(t *testing.T) {
	m, _, _ := testManifest(t)
	usage := &fakeUsage{total: 10000, used: 9600}
	e := NewEngine(m, usage.get)
	e.mu.Lock() // simulate an in-flight sweep
	defer e.mu.Unlock()
	if _, err := e.Recover(context.Background(), Request{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
}

// PIN (fail-closed without usage): no statfs truth → refuse to act,
// even with deletable caches sitting right there.
func TestNoUsageRefusesToAct(t *testing.T) {
	m, cacheRoot, _ := testManifest(t)
	seedTree(t, filepath.Join(cacheRoot, "go-build"), 1, 100)
	e := NewEngine(m, func() (int64, int64, error) { return 0, 0, errors.New("statfs broken") })
	if _, err := e.Recover(context.Background(), Request{}); !errors.Is(err, ErrNoUsage) {
		t.Fatalf("expected ErrNoUsage, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "go-build")); err != nil {
		t.Fatalf("must not delete without usage truth: %v", err)
	}
}

// PIN (age filter): prefix-class children newer than MinAgeSecs are
// never measured or reaped; stale ones are.
func TestPrefixStaleAgeFilter(t *testing.T) {
	dir := t.TempDir()
	tmpRoot := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(tmpRoot, "go-build111")
	live := filepath.Join(tmpRoot, "go-build222")
	seedTree(t, stale, 2, 100)
	seedTree(t, live, 2, 100)
	old := time.Now().Add(-10 * time.Minute)
	now := time.Now()
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(live, now, now); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(tmpRoot, "go.cov")
	if err := os.WriteFile(unrelated, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := Manifest{
		Base:  "test",
		Roots: []CacheRoot{{Path: filepath.Join(tmpRoot, "go-build"), Kind: RootParent}},
		Entries: []Entry{
			{Class: "tmp-build-residue", Path: filepath.Join(tmpRoot, "go-build"), Kind: EntryPrefixStale, MinAgeSecs: 120},
		},
	}
	usage := &fakeUsage{total: 10000, used: 9600}
	e := NewEngine(m, usage.get)
	e.SetClock(func() time.Time { return now })

	rep, err := e.Recover(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Classes) != 1 || rep.Classes[0].Status != ClassFreed {
		t.Fatalf("expected the stale residue freed, got %+v", rep.Classes)
	}
	if rep.Classes[0].Bytes != 200 || rep.Classes[0].Entries != 2 {
		t.Fatalf("stale class must count only stale files, got %+v", rep.Classes[0])
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live build dir must survive the age filter: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("non-matching /tmp entry must survive: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale corpse must be reaped, got %v", err)
	}
}

// PIN (write-free rescue path): a real recover creates no new files or
// directories anywhere under the volume root — the only mutations are
// the validated unlinks (owner ruling on #1601).
func TestRecoverWritesNothing(t *testing.T) {
	dir := t.TempDir()
	cacheRoot := filepath.Join(dir, "home", "sandbox", ".cache")
	if err := os.MkdirAll(filepath.Join(cacheRoot, "go-build"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedTree(t, filepath.Join(cacheRoot, "go-build"), 1, 100)
	before := snapshotTree(t, dir)

	m := Manifest{
		Base:  "test",
		Roots: []CacheRoot{{Path: cacheRoot, Kind: RootParent}},
		Entries: []Entry{
			{Class: "go-build-cache", Path: filepath.Join(cacheRoot, "go-build"), Kind: EntryDir},
		},
	}
	usage := &fakeUsage{total: 10000, used: 9600}
	e := NewEngine(m, usage.get)
	if _, err := e.Recover(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	after := snapshotTree(t, dir)
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Fatalf("recover created a new path %s — the rescue path must be write-free", p)
		}
	}
	if len(after) >= len(before) {
		t.Fatalf("expected the cache to shrink: before=%d after=%d", len(before), len(after))
	}
}

func snapshotTree(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out[p] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// PIN (refused entry in manifest): a manifest carrying an entry that
// fails the boundary is reported per-class as refused and the rest of
// the recovery proceeds.
func TestRefusedEntryReportedNotSilent(t *testing.T) {
	dir := t.TempDir()
	cacheRoot := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Base:  "test",
		Roots: []CacheRoot{{Path: cacheRoot, Kind: RootParent}},
		Entries: []Entry{
			{Class: "legit", Path: filepath.Join(cacheRoot, "go-build"), Kind: EntryDir},
			// Refused on purpose: targets the parent root itself.
			{Class: "greedy", Path: cacheRoot, Kind: EntryDir},
		},
	}
	seedTree(t, filepath.Join(cacheRoot, "go-build"), 1, 100)
	usage := &fakeUsage{total: 10000, used: 9600}
	e := NewEngine(m, usage.get)
	rep, err := e.Recover(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	var refused, freed bool
	for _, c := range rep.Classes {
		if c.Class == "greedy" {
			if c.Status != ClassRefused || c.Reason == "" {
				t.Fatalf("greedy entry must be loudly refused, got %+v", c)
			}
			refused = true
		}
		if c.Class == "legit" && c.Status == ClassFreed {
			freed = true
		}
	}
	if !refused || !freed {
		t.Fatalf("expected greedy refused AND legit freed, classes=%+v", rep.Classes)
	}
}

// PIN (target clamp): absurd targets are clamped into
// [MinTargetRatio, MaxTargetRatio]; zero means the default.
func TestTargetClamped(t *testing.T) {
	m, cacheRoot, _ := testManifest(t)
	cases := []struct {
		in, want float64
	}{
		{0, DefaultTargetRatio},
		{0.01, MinTargetRatio},
		{0.99, MaxTargetRatio},
		{-1, MinTargetRatio},
		{0.7, 0.7},
	}
	for _, c := range cases {
		if got := clampTarget(c.in); got != c.want {
			t.Errorf("clampTarget(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	// End-to-end: a 0.99 target clamps to 0.90. Script: pre-read 9600
	// (above → proceed); loop check before go-build: still 9600 (above
	// → delete it); loop check before npm: 8600 (86% ≤ 90% → stop; npm
	// survives despite being measured).
	seedTree(t, filepath.Join(cacheRoot, "go-build"), 5, 1000)
	npmPath := npmRoot2(t, m)
	seedTree(t, npmPath, 1, 1000)
	usage := &fakeUsage{total: 10000, used: 9600}
	e := NewEngine(m, func() (int64, int64, error) {
		switch usage.calls {
		case 0, 1: // pre-read + first loop check
			usage.calls++
			return 9600, 10000, nil
		default:
			usage.calls++
			return 8600, 10000, nil
		}
	})
	rep, err := e.Recover(context.Background(), Request{TargetRatio: 0.99})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.StoppedEarly {
		t.Fatalf("0.99 target must clamp to 0.90 and stop once below it, got %+v", rep)
	}
	if rep.TargetRatio != MaxTargetRatio {
		t.Fatalf("target must be clamped on the wire, got %v", rep.TargetRatio)
	}
	byClass := map[string]ClassReport{}
	for _, c := range rep.Classes {
		byClass[c.Class] = c
	}
	if byClass["go-build-cache"].Status != ClassFreed {
		t.Fatalf("largest class frees to cross the target, got %+v", byClass["go-build-cache"])
	}
	if byClass["npm-cache"].Status != ClassSkippedTargetMet {
		t.Fatalf("target met → remaining class skipped, got %+v", byClass["npm-cache"])
	}
	if _, err := os.Stat(npmPath); err != nil {
		t.Fatalf("skipped class must remain on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "go-build")); !os.IsNotExist(err) {
		t.Fatalf("freed class must be gone, got %v", err)
	}
}

// npmRoot2 re-derives the manifest's npm path (testManifest hides it).
func npmRoot2(t *testing.T, m Manifest) string {
	t.Helper()
	for _, e := range m.Entries {
		if e.Class == "npm-cache" {
			return e.Path
		}
	}
	t.Fatal("no npm-cache entry")
	return ""
}

// PIN (not_present): absent allowlisted paths report not_present and
// are not errors.
func TestAbsentClassNotPresent(t *testing.T) {
	m, _, _ := testManifest(t) // nothing seeded
	usage := &fakeUsage{total: 10000, used: 9600}
	e := NewEngine(m, usage.get)
	rep, err := e.Recover(context.Background(), Request{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Classes {
		if c.Status != ClassNotPresent {
			t.Fatalf("empty cache must report not_present, got %+v", c)
		}
	}
	if len(rep.Classes) != 3 {
		t.Fatalf("expected all three classes enumerated, got %+v", rep.Classes)
	}
}

// PIN (wire shape, review r1 F2/F4): every report path marshals
// "classes":[] — never null. A nil slice would crash null-naive
// consumers (the below-target fast path and the empty unknown-base
// manifest both hit this).
func TestReportClassesNeverNilOnWire(t *testing.T) {
	m, _, _ := testManifest(t)
	usage := &fakeUsage{total: 10000, used: 5000}
	e := NewEngine(m, usage.get)
	rep, err := e.Recover(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"classes":null`) {
		t.Fatalf("below-target report must marshal classes:[], got %s", raw)
	}
	// Unknown-base manifest: empty entries, above target → still [].
	empty := NewEngine(Manifest{Base: "unknown"}, func() (int64, int64, error) { return 9600, 10000, nil })
	rep2, err := empty.Recover(context.Background(), Request{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Classes == nil || len(rep2.Classes) != 0 {
		t.Fatalf("unknown-base report must have empty NON-nil classes, got %#v", rep2.Classes)
	}
	raw2, _ := json.Marshal(rep2)
	if strings.Contains(string(raw2), `"classes":null`) {
		t.Fatalf("unknown-base report must marshal classes:[], got %s", raw2)
	}
}

// PIN (measure/delete agreement, review r1 F3): an in-root symlinked
// cache must free its TARGET tree — RemoveAll on the literal link
// alone would report the target's bytes as freed while the bytes stay
// on disk (fabricated bytesFreed).
func TestSymlinkedCacheDeletesMeasuredTarget(t *testing.T) {
	dir := t.TempDir()
	cacheRoot := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cacheRoot, "go-build.real")
	wantBytes := seedTree(t, target, 3, 1000)
	link := filepath.Join(cacheRoot, "go-build")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	m := Manifest{
		Base:  "test",
		Roots: []CacheRoot{{Path: cacheRoot, Kind: RootParent}},
		Entries: []Entry{
			{Class: "go-build-cache", Path: link, Kind: EntryDir},
		},
	}
	usage := &fakeUsage{total: 10000, used: 9600}
	e := NewEngine(m, usage.get)
	rep, err := e.Recover(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	var cr ClassReport
	for _, c := range rep.Classes {
		if c.Class == "go-build-cache" {
			cr = c
		}
	}
	if cr.Status != ClassFreed {
		t.Fatalf("symlinked in-root cache must free, got %+v", cr)
	}
	if cr.Bytes != wantBytes || cr.BytesFreed != wantBytes {
		t.Fatalf("bytes must be the measured target bytes %d, got %+v", wantBytes, cr)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the symlink TARGET tree must be deleted (measure/delete agreement): %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("the stale link itself must be removed too: %v", err)
	}
}

// PIN (aliasing honesty, review r2): two classes resolving to the same
// tree — the second must report not-present with zero bytes, never
// double-count the measured figure as freed.
func TestAliasedClassesDoNotDoubleCount(t *testing.T) {
	dir := t.TempDir()
	cacheRoot := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cacheRoot, "go-build.real")
	seedTree(t, target, 2, 500)
	if err := os.Symlink(target, filepath.Join(cacheRoot, "go-build")); err != nil {
		t.Fatal(err)
	}
	// Second symlink to the SAME tree under a different class.
	if err := os.Symlink(target, filepath.Join(cacheRoot, "pip")); err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Base:  "test",
		Roots: []CacheRoot{{Path: cacheRoot, Kind: RootParent}},
		Entries: []Entry{
			{Class: "go-build-cache", Path: filepath.Join(cacheRoot, "go-build"), Kind: EntryDir},
			{Class: "pip-cache", Path: filepath.Join(cacheRoot, "pip"), Kind: EntryDir},
		},
	}
	usage := &fakeUsage{total: 10000, used: 9600}
	e := NewEngine(m, usage.get)
	rep, err := e.Recover(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	freedCount, ghost := 0, false
	for _, c := range rep.Classes {
		if c.Status == ClassFreed {
			freedCount++
		}
		if c.Status == ClassNotPresent && c.BytesFreed != 0 {
			t.Fatalf("aliased class must not claim bytes: %+v", c)
		}
		if c.Status == ClassFreed && c.BytesFreed != c.Bytes {
			t.Fatalf("freed class bytes mismatch: %+v", c)
		}
		_ = c
	}
	// Exactly one of the two aliased classes may claim the bytes.
	if freedCount > 1 {
		ghost = true
	}
	if ghost {
		t.Fatalf("aliasing double-count: %d classes claimed the same tree", freedCount)
	}
	if rep.BytesFreed != 1000 {
		t.Fatalf("total freed must be the tree size once (1000), got %d", rep.BytesFreed)
	}
}
