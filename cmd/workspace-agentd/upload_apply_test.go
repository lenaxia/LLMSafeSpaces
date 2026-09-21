// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// upload_apply_test.go — design 0060 §3.2/§4.4 pins for the supervisor
// engine: closed-param validation, write-time gates, copy+verify+rename
// atomicity, the additive ack fields, and the destination scrub.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func applyTestDigest(t *testing.T, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// availCell gives tests a MUTABLE avail — the TOCTOU shapes mutate it
// mid-apply (the margin flag is only reachable when OTHER writers
// consume the disk between the gate and the post-rename check).
type availCell struct{ v int64 }

func (c *availCell) statfs(string) (*statfsT, error) {
	return &statfsT{Bavail: uint64(c.v), Bsize: 1}, nil
}

func applyEngineFixture(t *testing.T, avail int64) (*uploadApplyEngine, string, string, *availCell) {
	t.Helper()
	tmp := t.TempDir()
	staging := filepath.Join(tmp, "staged-upload-files")
	uploads := filepath.Join(tmp, "uploads")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	cell := &availCell{v: avail}
	e := uploadApplyEngineFromEnv()
	e.stagingRoot = staging
	e.uploadsDir = uploads
	e.destMargin = 64 << 20
	e.ttl = 15 * time.Minute
	e.statfs = cell.statfs
	e.uuid = func() string { return "00000000-0000-4000-8000-000000000001" }
	e.now = time.Now
	return e, staging, uploads, cell
}

const testUploadID = "11111111-2222-4333-8444-555555555555"

func stageTestObject(t *testing.T, staging, id, content string) string {
	t.Helper()
	p := filepath.Join(staging, id)
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func applyParams(over map[string]any) map[string]any {
	params := map[string]any{
		"upload_id":   testUploadID,
		"staged_name": testUploadID,
		"size":        float64(5),
		"sha256":      "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", // sha256("hello") — a valid 64-hex default
		"target_name": "notes.txt",
	}
	for k, v := range over {
		params[k] = v
	}
	return params
}

func TestUploadApply_HappyPath(t *testing.T) {
	e, _, uploads, _ := applyEngineFixture(t, 1<<40)
	content := "hello"
	// real sha256 of the content
	digest := applyTestDigest(t, content)
	_ = stageTestObject(t, e.stagingRoot, testUploadID, content)

	res, aerr := e.Apply(applyParams(map[string]any{"sha256": digest, "size": float64(len(content))}))
	if aerr != nil {
		t.Fatalf("apply: %+v", aerr)
	}
	if res["applied"] != true {
		t.Fatalf("applied: %+v", res)
	}
	path := res["path"].(string)
	if !strings.HasPrefix(path, uploads) || !strings.HasSuffix(path, "-notes.txt") {
		t.Fatalf("path: %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("final object must exist: %v", err)
	}
	// Additive ack fields present (A.1-legal).
	if _, ok := res["dest_avail_after"]; !ok {
		t.Fatal("dest_avail_after must ride the ack")
	}
	if mc, ok := res["margin_consumed"].(bool); !ok {
		t.Fatal("margin_consumed must be a bool")
	} else if mc {
		t.Fatal("a huge avail must not flag margin_consumed")
	}
}

func TestUploadApply_ParamValidation(t *testing.T) {
	e, _, _, _ := applyEngineFixture(t, 1<<40)
	cases := []struct {
		name   string
		params map[string]any
		code   string
	}{
		{"non-uuid upload_id", applyParams(map[string]any{"upload_id": "../../etc"}), "bad_request"},
		{"mismatched ids", applyParams(map[string]any{"staged_name": "99999999-9999-4999-8999-999999999999"}), "bad_request"},
		{"zero size", applyParams(map[string]any{"size": float64(0)}), "bad_request"},
		{"short sha", applyParams(map[string]any{"sha256": "abc"}), "bad_request"},
		{"traversal target", applyParams(map[string]any{"target_name": "../../etc/passwd"}), "target_rejected"},
		{"slash target", applyParams(map[string]any{"target_name": "a/b.txt"}), "target_rejected"},
	}
	for _, tc := range cases {
		_, aerr := e.Apply(tc.params)
		if aerr == nil || aerr.code != tc.code {
			t.Fatalf("%s: expected %q, got %+v", tc.name, tc.code, aerr)
		}
	}
}

func TestUploadApply_StagedMissing(t *testing.T) {
	e, _, _, _ := applyEngineFixture(t, 1<<40)
	_, aerr := e.Apply(applyParams(nil))
	if aerr == nil || aerr.code != "staged_missing" {
		t.Fatalf("got %+v", aerr)
	}
}

func TestUploadApply_ChecksumMismatchLeavesNothingVisible(t *testing.T) {
	e, _, uploads, _ := applyEngineFixture(t, 1<<40)
	_ = stageTestObject(t, e.stagingRoot, testUploadID, "hello")
	_, aerr := e.Apply(applyParams(map[string]any{"sha256": applyTestDigest(t, "tampered")}))
	if aerr == nil || aerr.code != "checksum_mismatch" {
		t.Fatalf("got %+v", aerr)
	}
	// §3.4: nothing visible in the destination, no .tmp residue.
	entries, _ := os.ReadDir(uploads)
	if len(entries) != 0 {
		t.Fatalf("checksum failure must leave no artifacts, found %d", len(entries))
	}
}

func TestUploadApply_SizeMismatch(t *testing.T) {
	e, _, _, _ := applyEngineFixture(t, 1<<40)
	_ = stageTestObject(t, e.stagingRoot, testUploadID, "hello")
	_, aerr := e.Apply(applyParams(map[string]any{"sha256": applyTestDigest(t, "hello"), "size": float64(4)}))
	if aerr == nil || aerr.code != "size_mismatch" {
		t.Fatalf("got %+v", aerr)
	}
}

func TestUploadApply_DestDiskFullWriteTime(t *testing.T) {
	// The TOCTOU-authoritative gate: avail below size+margin at write
	// time → dest_disk_full, staged object untouched.
	e, staging, _, _ := applyEngineFixture(t, 10<<20) // 10 MiB avail, 64 MiB margin
	_ = stageTestObject(t, e.stagingRoot, testUploadID, "hello")
	_, aerr := e.Apply(applyParams(map[string]any{"sha256": applyTestDigest(t, "hello")}))
	if aerr == nil || aerr.code != "dest_disk_full" {
		t.Fatalf("got %+v", aerr)
	}
	if _, err := os.Stat(filepath.Join(staging, testUploadID)); err != nil {
		t.Fatalf("the staged object must be untouched: %v", err)
	}
}

func TestUploadApply_MarginConsumedFlag(t *testing.T) {
	// §4.4: the flag is only reachable when OTHER writers consume the
	// disk between the pre-copy gate and the post-rename check — the
	// TOCTOU interleave the observation exists for (a single writer can
	// never trip it: size+margin ≤ avail implies post-write ≥ margin).
	// The rename seam stands in for the concurrent writer.
	e, _, _, cell := applyEngineFixture(t, 1<<40)
	_ = stageTestObject(t, e.stagingRoot, testUploadID, "hello")
	e.rename = func(oldpath, newpath string) error {
		cell.v = 1 << 20 // another writer fills the volume post-gate
		return os.Rename(oldpath, newpath)
	}
	res, aerr := e.Apply(applyParams(map[string]any{"sha256": applyTestDigest(t, "hello")}))
	if aerr != nil {
		t.Fatalf("apply (the write succeeded — correctness first): %+v", aerr)
	}
	if mc, _ := res["margin_consumed"].(bool); !mc {
		t.Fatalf("margin_consumed must flag the post-write consumption: %+v", res)
	}
}

func TestUploadApply_UnknownParamKeysIgnored(t *testing.T) {
	// A.1 forward compatibility: unknown KEYS are ignored (the decoder
	// drops them); the known set still drives the apply.
	e, _, _, _ := applyEngineFixture(t, 1<<40)
	_ = stageTestObject(t, e.stagingRoot, testUploadID, "hello")
	params := applyParams(map[string]any{"sha256": applyTestDigest(t, "hello"), "future_field": "whatever"})
	res, aerr := e.Apply(params)
	if aerr != nil || res["applied"] != true {
		t.Fatalf("unknown keys must not reject: %+v", aerr)
	}
}

func TestDestinationScrub_BootAndTTL(t *testing.T) {
	e, _, uploads, _ := applyEngineFixture(t, 1<<40)
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	crashed := filepath.Join(uploads, "old-id-notes.txt.tmp")
	freshFinal := filepath.Join(uploads, "new-id-notes.txt")
	freshTmp := filepath.Join(uploads, "newer-id-notes.txt.tmp")
	for _, p := range []string{crashed, freshFinal, freshTmp} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * e.ttl)
	if err := os.Chtimes(crashed, past, past); err != nil {
		t.Fatal(err)
	}

	if n := e.scrubDestination(e.ttl); n != 1 {
		t.Fatalf("TTL scrub removes only the aged .tmp, got %d", n)
	}
	if _, err := os.Stat(freshTmp); err != nil {
		t.Fatalf("fresh .tmp survives the TTL scrub: %v", err)
	}
	if _, err := os.Stat(freshFinal); err != nil {
		t.Fatalf("final objects are never scrubbed: %v", err)
	}
	if n := e.scrubDestination(0); n != 1 {
		t.Fatalf("boot scrub removes the fresh .tmp too, got %d", n)
	}
	if _, err := os.Stat(crashed); err == nil {
		t.Fatal("boot scrub must have removed everything .tmp")
	}
}
