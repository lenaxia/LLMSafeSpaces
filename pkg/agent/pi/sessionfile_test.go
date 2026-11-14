// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// sessionfile_test.go — the persisted-JSONL layer's pins: file-name id
// parsing (ids may contain '_', timestamps never do), the streaming
// scan over the RECORDED artifact, verify-scan floors, CRLF framing
// tolerance, and the active-branch cycle guard.

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestSessionIDFromName(t *testing.T) {
	cases := map[string]string{
		"2026-11-13T07-15-23-825Z_aaaabbbb-cccc-dddd-eeee-ffff00001111.jsonl": "aaaabbbb-cccc-dddd-eeee-ffff00001111",
		"2026-11-13T07-15-23-825Z_with_underscores.jsonl":                     "with_underscores",
		"malformed.jsonl":     "",
		"no-ext-2026-11-13_x": "x",
	}
	for name, want := range cases {
		if got := sessionIDFromName(name); got != want {
			t.Errorf("sessionIDFromName(%q) = %q want %q", name, got, want)
		}
	}
}

func TestScanSessionFile_RecordedArtifact(t *testing.T) {
	f, err := os.Open("testdata/fixture_session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sum, err := scanSessionFile(f)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sum.SessionID != fixtureSessionID {
		t.Fatalf("header id: %q", sum.SessionID)
	}
	if !sum.CreatedAt.Equal(time.Date(2026, 11, 13, 7, 15, 23, 825000000, time.UTC)) {
		t.Fatalf("header ts: %v", sum.CreatedAt)
	}
	// Entries: model_change, thinking_level_change, system, user,
	// assistant (the header is not an entry).
	if len(sum.Entries) != 5 {
		t.Fatalf("entries: %d", len(sum.Entries))
	}
	if sum.LeafID != "6cd91035" {
		t.Fatalf("leaf: %q", sum.LeafID)
	}
	// No session_info in the recorded file: title falls back to the
	// first user message.
	if got := sum.title(); got != "remember the number 42" {
		t.Fatalf("title fallback: %q", got)
	}
	msgs := sum.transcriptMessages()
	// model_change + user + assistant (thinking/system entries have no
	// transcript surface).
	if len(msgs) != 3 {
		t.Fatalf("transcript: %d", len(msgs))
	}
}

func TestScanSessionFile_CRLFTolerantAndDefensive(t *testing.T) {
	// CRLF framing + garbage lines + unknown entry kinds: none fatal.
	content := "{\"type\":\"session\",\"version\":3,\"id\":\"s1\",\"timestamp\":\"2026-11-13T10:00:00.000Z\",\"cwd\":\"/w\"}\r\n" +
		"not json\r\n" +
		"{\"type\":\"brand_new_kind\",\"id\":\"b1\",\"parentId\":null,\"timestamp\":\"2026-11-13T10:00:01.000Z\"}\r\n" +
		"{\"type\":\"message\",\"id\":\"u1\",\"parentId\":null,\"timestamp\":\"2026-11-13T10:00:02.000Z\",\"message\":{\"role\":\"user\",\"content\":\"hi\",\"timestamp\":100}}\r\n"
	sum, err := scanSessionFile(strings.NewReader(content))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sum.SessionID != "s1" {
		t.Fatalf("id: %q", sum.SessionID)
	}
	// The unknown kind still counts as an entry (id present); garbage
	// line skipped.
	if len(sum.Entries) != 2 {
		t.Fatalf("entries: %d", len(sum.Entries))
	}
	if sum.LeafID != "u1" {
		t.Fatalf("leaf: %q", sum.LeafID)
	}
	msgs := sum.transcriptMessages()
	if len(msgs) != 1 || msgs[0].Text != "hi" {
		t.Fatalf("transcript: %+v", msgs)
	}
}

func TestVerifyScan_FloorsAndMultiBlockText(t *testing.T) {
	at := time.UnixMilli(1794554401000).UTC()
	iso := at.Format("2006-01-02T15:04:05.000Z")
	file := "{\"type\":\"session\",\"version\":3,\"id\":\"s\",\"timestamp\":\"" + iso + "\",\"cwd\":\"/\"}\n" +
		"{\"type\":\"message\",\"id\":\"a\",\"parentId\":null,\"timestamp\":\"" + iso + "\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"hello \"},{\"type\":\"text\",\"text\":\"world\"}],\"timestamp\":" +
		int64String(at.UnixMilli()) + "}}\n"

	// Multi-block text concatenates: "hello world".
	delivered, definitive, err := verifyScan(strings.NewReader(file), "hello world", at.Add(-time.Minute))
	if err != nil || !delivered || !definitive {
		t.Fatalf("multi-block match: (%v,%v,%v)", delivered, definitive, err)
	}
	// Wrong text: definitive absence.
	delivered, definitive, err = verifyScan(strings.NewReader(file), "hello", at.Add(-time.Minute))
	if err != nil || delivered || !definitive {
		t.Fatalf("partial text must not match: (%v,%v,%v)", delivered, definitive, err)
	}
	// Floor after the message: excluded, definitive absence.
	delivered, definitive, err = verifyScan(strings.NewReader(file), "hello world", at.Add(time.Hour))
	if err != nil || delivered || !definitive {
		t.Fatalf("future floor: (%v,%v,%v)", delivered, definitive, err)
	}
	// Clock-skew window: the ADAPTER widens the caller floor by
	// verifyClockSkew (entryTimestampFloor); at the raw scan level a
	// floor 30s after the message still matches once widened.
	delivered, definitive, err = verifyScan(strings.NewReader(file), "hello world", at.Add(30*time.Second-verifyClockSkew))
	if err != nil || !delivered || !definitive {
		t.Fatalf("skew floor: (%v,%v,%v)", delivered, definitive, err)
	}
	// A truncated read is INCONCLUSIVE (absence not proven).
	delivered, definitive, err = verifyScan(iotest.ErrReader(os.ErrClosed), "hello world", at.Add(-time.Minute))
	if err == nil || delivered || definitive {
		t.Fatalf("failed read must be inconclusive: (%v,%v,%v)", delivered, definitive, err)
	}
}

func int64String(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestActiveBranch_CycleGuard(t *testing.T) {
	// A hand-edited cycle (a→b→a) must not spin; the chain truncates.
	a, b := "a", "b"
	ap, bp := a, b
	byID := map[string]*piEntry{
		a: {ID: a, ParentID: &bp},
		b: {ID: b, ParentID: &ap},
	}
	chain := activeBranch(byID, "a")
	if len(chain) != 2 {
		t.Fatalf("cycle chain: %d", len(chain))
	}
}
