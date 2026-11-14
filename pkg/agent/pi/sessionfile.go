// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// sessionfile.go — the persisted-JSONL layer. pi's session store IS
// the .jsonl file ("Sessions can be removed by deleting their .jsonl
// files", session-format.md; the format is documented with a parsing
// example), so VerifyDelivery (#987/#1119) and CountMessages (#1481)
// answer by scanning the file — which works while pi is down, the
// property the acceptance criteria pin.
//
// Every scan is streaming (json.Decoder over the reader — no full-file
// buffering; a >16MiB session must not balloon the API pod, the #737
// lesson) and defensive (unknown entry kinds and undecodable lines are
// skipped, never fatal — pi's own consumer posture: "treat unknown
// kind values as normal usage rather than rejecting them").
//
// The leaf of the tree is the LAST entry in append order: pi appends
// new work at the current position, so the final line is always the
// newest append — the position get_entries reports as leafId.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// verifyClockSkew widens the since-floor on the delivery scan: the
// platform's clock and the pod's clock are not the same clock (the
// opencode seam's 2m constant).
const verifyClockSkew = 2 * time.Minute

// sessionFileSummary is what one full-file pass learns. The scan is
// single-pass: title, counts, and delivery matching all ride it.
type sessionFileSummary struct {
	// Header fields (absent when the file's first line is not a
	// session header — tolerated; the scan still answers from entries).
	SessionID string
	CreatedAt time.Time
	// Title is the latest session_info name, else the first user
	// message text (truncated), else "".
	Title string
	// LeafID is the last entry id in append order ("" for an empty
	// file).
	LeafID string
	// Entries holds every decoded entry in append order (the tree; the
	// active branch is derived on demand).
	Entries []piEntry
}

// scanSessionFile streams one .jsonl into a summary. r is consumed to
// EOF (the VerifyDelivery absence proof needs the whole file).
func scanSessionFile(r io.Reader) (*sessionFileSummary, error) {
	sum := &sessionFileSummary{}
	br := bufio.NewReaderSize(r, 64*1024)
	first := true
	for {
		line, err := readJSONLLine(br)
		if len(line) > 0 {
			var probe struct {
				Type string `json:"type"`
			}
			if jerr := json.Unmarshal(line, &probe); jerr == nil {
				if first && probe.Type == "session" {
					var hdr struct {
						ID        string `json:"id"`
						Timestamp string `json:"timestamp"`
					}
					if jerr := json.Unmarshal(line, &hdr); jerr == nil {
						sum.SessionID = hdr.ID
						sum.CreatedAt = parseISO(hdr.Timestamp)
					}
				} else if probe.Type != "session" {
					var e piEntry
					if jerr := json.Unmarshal(line, &e); jerr == nil && e.ID != "" {
						sum.Entries = append(sum.Entries, e)
						sum.LeafID = e.ID
						if e.Type == "session_info" && e.Name != nil && *e.Name != "" {
							sum.Title = *e.Name
						}
					}
				}
			}
			// Undecodable lines and unknown types: skipped (defensive).
			first = false
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return sum, nil
			}
			return sum, err
		}
	}
}

// readJSONLLine reads one LF-terminated record, stripping an optional
// preceding CR (json.md framing: split ONLY on LF — a bufio.Scanner
// would also honor Unicode separators as boundaries and caps lines at
// 64KiB; pi's agent_end records carry whole message arrays).
func readJSONLLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadString('\n')
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	return []byte(line), err
}

func parseISO(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// firstUserText returns the first user-message text on the ACTIVE
// branch (the title fallback — mirrors what pi's own /resume listing
// shows when no session_info name is set).
func (s *sessionFileSummary) firstUserText() string {
	byID := make(map[string]*piEntry, len(s.Entries))
	for i := range s.Entries {
		byID[s.Entries[i].ID] = &s.Entries[i]
	}
	for _, e := range activeBranch(byID, s.LeafID) {
		if e.Message != nil && e.Message.Role == "user" {
			t := e.Message.textContent()
			if len(t) > 80 {
				t = t[:80]
			}
			return t
		}
	}
	return ""
}

// title resolves the list title: explicit session_info name, else the
// first user message, else "".
func (s *sessionFileSummary) title() string {
	if s.Title != "" {
		return s.Title
	}
	return s.firstUserText()
}

// transcriptMessages derives the platform transcript from the file
// (active-branch translation — the same function the live get_entries
// path uses, so file-derived and RPC-derived history agree by
// construction).
func (s *sessionFileSummary) transcriptMessages() []session.Message {
	return translateEntries(s.Entries, s.LeafID)
}

// sessionIDFromName parses a session file name
// (<timestamp>_<session-id>.jsonl): the id is everything after the
// FIRST '_' (the timestamp never contains '_'; ids may — validated
// against the real persisted name in testdata).
func sessionIDFromName(name string) string {
	name = strings.TrimSuffix(name, ".jsonl")
	i := strings.Index(name, "_")
	if i < 0 || i == len(name)-1 {
		return ""
	}
	return name[i+1:]
}

// verifyScan answers the VerifyDelivery oracle over one session file:
// delivered=true iff a user message whose text equals `text` persists
// at or after the skewed floor. EOF without a match is DEFINITIVE
// absence — the file is the durable store, and pi persists the user
// message entry when the run starts (assistant "pending" messages are
// the only non-persisted kind, message-types.md).
func verifyScan(r io.Reader, text string, floor time.Time) (delivered, definitive bool, err error) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, rerr := readJSONLLine(br)
		if len(line) > 0 {
			var e piEntry
			if jerr := json.Unmarshal(line, &e); jerr == nil && e.Message != nil && e.Message.Role == "user" {
				at := e.Message.createdAt(&e)
				if !at.Before(floor) && e.Message.textContent() == text {
					return true, true, nil
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return false, true, nil
			}
			// A read that died mid-file has NOT proven absence.
			return false, false, rerr
		}
	}
}

// entryTimestampFloor converts the since instant to the entry-clock
// floor (message timestamps are unix millis; entries carry ISO — the
// comparison uses whichever the entry carries, so the floor is checked
// in both scales by createdAt).
func entryTimestampFloor(since time.Time) time.Time {
	return since.Add(-verifyClockSkew)
}

// errSessionFileNotFound wraps the typed gone sentinel for
// transport-level file absence (#1479 gone-classification input).
func errSessionFileNotFound(sessionID string) error {
	return fmt.Errorf("pi session %s: %w", sessionID, agent.ErrSessionNotFound)
}
