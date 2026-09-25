package utilities

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// FindDuplicateKeys scans raw JSON and reports every object key that
// appears more than once within the same object, as dotted paths
// rooted at "$" (e.g. `$.params.arguments.session_id`).
//
// Why: Go's encoding/json collapses duplicate keys SILENTLY (last
// wins) — a duplicated id field in a tools/call body therefore
// misbinds with zero trace (#1530's comment-thread exposure class:
// the model-emission slip that duplicates a value fragment can just
// as easily duplicate a KEY; the fragment shape recovers loudly, the
// key shape misdelivers silently). The seam refuses such bodies
// (-32602) instead of guessing intent; this scanner is the detector.
//
// Malformed JSON returns the decode error. Callers must FAIL CLOSED
// on it: a streaming json.Decoder.Decode accepts some bodies this
// scanner rejects (trailing garbage after the top-level value), so
// "scanner errored" can mean "duplicate keys hid beneath salvage" —
// the MCP tools/call seam refuses such bodies (-32700) rather than
// dispatching past the gate.
//
// Implementation: encoding/json cannot re-emit duplicates (map
// collapse), so this walks the Decoder token-by-token with explicit
// recursive descent — keys are structurally unambiguous at this
// layer (objects are key, value, key, value…), unlike the raw token
// stream's undifferentiated strings.
// Reporting bounds (r3's measured finding: a 619KB duplicate-bearing
// body — 8000-deep chain + 45K dup pairs — made the uncapped scan
// render 89,999 full-depth paths: 7.63s CPU, 17.78GB allocated, and
// the seam's strings.Join then built a 3.93GB error string. Duplicates
// are this scanner's TARGET input, not a rare accident — both factors
// (dups × depth) are attacker-chosen within the 1MiB body cap, so the
// REPORT is bounded by construction: first maxReportedDupPaths paths,
// each capped at maxDupPathLength chars, plus the TOTAL count so the
// refusal still says how many).
const (
	maxReportedDupPaths = 16
	maxDupPathLength    = 4096
)

type dupKeyScanner struct {
	dec      *json.Decoder
	dups     []string
	dupTotal int
	objs     []map[string]bool
	segs     []string
}

// FindDuplicateKeys reports duplicated object keys as dotted paths
// rooted at "$" (array elements qualify as [i]); the SECOND return is
// the TOTAL duplicate-occurrence count (the first return carries at
// most maxReportedDupPaths truncated paths). Both zero = no
// duplicates.
func FindDuplicateKeys(data []byte) ([]string, int, error) {
	s := &dupKeyScanner{dec: json.NewDecoder(bytes.NewReader(data))}
	s.dec.UseNumber()
	if err := s.value(); err != nil {
		if err == io.EOF {
			return nil, 0, fmt.Errorf("unexpected EOF")
		}
		return nil, 0, err
	}
	// reject trailing garbage after the top-level value
	if _, err := s.dec.Token(); err != io.EOF {
		return nil, 0, fmt.Errorf("trailing data after JSON value")
	}
	return s.dups, s.dupTotal, nil
}

func (s *dupKeyScanner) value() error {
	tok, err := s.dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar
	}
	switch d {
	case '{':
		return s.object()
	case '[':
		return s.array()
	default:
		return fmt.Errorf("unexpected delimiter %v", d)
	}
}

func (s *dupKeyScanner) object() error {
	s.objs = append(s.objs, map[string]bool{})
	defer func() { s.objs = s.objs[:len(s.objs)-1] }()
	for s.dec.More() {
		tok, err := s.dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("object key is not a string: %v", tok)
		}
		frame := s.objs[len(s.objs)-1]
		// Path is built LAZILY, only on a duplicate, and the REPORT is
		// capped: eager per-key builds made VALID-body scans quadratic
		// (r1), and uncapped dup reports made DUPLICATE-bearing scans
		// explode (r3 — dups are the target input, both factors
		// attacker-chosen). First maxReportedDupPaths paths render
		// (each truncated to maxDupPathLength); every further
		// duplicate costs one increment.
		if frame[key] {
			s.dupTotal++
			if len(s.dups) < maxReportedDupPaths {
				s.dups = append(s.dups, truncatePath(s.path(key)))
			}
		}
		frame[key] = true
		s.segs = append(s.segs, "."+key)
		valErr := s.value() // the key's value
		s.segs = s.segs[:len(s.segs)-1]
		if valErr != nil {
			return valErr
		}
	}
	_, err := s.dec.Token() // consume '}'
	return err
}

func (s *dupKeyScanner) array() error {
	for i := 0; s.dec.More(); i++ {
		s.segs = append(s.segs, fmt.Sprintf("[%d]", i))
		err := s.value()
		s.segs = s.segs[:len(s.segs)-1]
		if err != nil {
			return err
		}
	}
	_, err := s.dec.Token() // consume ']'
	return err
}

// path renders the dotted path for a key relative to the scanner's
// current position; array segments ([i]) join without a dot.
func (s *dupKeyScanner) path(key string) string {
	var b strings.Builder
	b.WriteString("$")
	for _, seg := range s.segs {
		b.WriteString(seg)
	}
	b.WriteString("." + key)
	return b.String()
}

// truncatePath bounds a reported path: paths render at O(depth), and
// depth is attacker-chosen — a 10,000-deep nest yields ~60KB paths.
// Capped at maxDupPathLength with an explicit truncation marker.
func truncatePath(p string) string {
	if len(p) <= maxDupPathLength {
		return p
	}
	return p[:maxDupPathLength] + "…(truncated)"
}
