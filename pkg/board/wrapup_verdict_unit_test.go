// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// wrapup_verdict_unit_test.go — the wire schema's closed-vocabulary pins
// (epic wrap-up review, low-level design §4; high-level D6): the verdict
// enum is pass|gaps (a story verdict's approve|fail REFUSES — the schemas
// are siblings, never interchangeable), reportUrl is required on BOTH
// verdicts (the →done evidence), gapRefs are required non-empty IFF gaps
// (present on pass refuses; empty on gaps refuses), and notes are
// size-capped (the textguard posture).
package board_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func validWrapUpVerdict() board.WrapUpVerdictInput {
	return board.WrapUpVerdictInput{
		Verdict:   board.WrapUpVerdictPass,
		ReportURL: "https://ev.test/wrapup",
		Reviewer:  "wrapup-reviewer",
	}
}

func TestValidateWrapUpVerdictInput(t *testing.T) {
	t.Run("pass is valid", func(t *testing.T) {
		require.NoError(t, board.ValidateWrapUpVerdictInput(validWrapUpVerdict()))
	})
	t.Run("gaps with refs is valid", func(t *testing.T) {
		in := validWrapUpVerdict()
		in.Verdict = board.WrapUpVerdictGaps
		in.GapRefs = []string{"tsk-amber-falcon-glides"}
		require.NoError(t, board.ValidateWrapUpVerdictInput(in))
	})
	t.Run("notes ride along under the cap", func(t *testing.T) {
		in := validWrapUpVerdict()
		in.Notes = variedNotes(512)
		require.NoError(t, board.ValidateWrapUpVerdictInput(in))
	})

	t.Run("the enum is closed", func(t *testing.T) {
		for _, v := range []string{"", "approve", "fail", "PASS", "Gaps", "pending"} {
			in := validWrapUpVerdict()
			in.Verdict = board.WrappingVerdict(v)
			err := board.ValidateWrapUpVerdictInput(in)
			require.Error(t, err, "verdict %q must refuse", v)
			require.Contains(t, err.Error(), "pass")
		}
	})
	t.Run("reportUrl is required on both verdicts", func(t *testing.T) {
		in := validWrapUpVerdict()
		in.ReportURL = ""
		require.Error(t, board.ValidateWrapUpVerdictInput(in))
		in.Verdict = board.WrapUpVerdictGaps
		in.GapRefs = []string{"tsk-x"}
		require.Error(t, board.ValidateWrapUpVerdictInput(in))
	})
	t.Run("reviewer identity is required", func(t *testing.T) {
		in := validWrapUpVerdict()
		in.Reviewer = ""
		require.Error(t, board.ValidateWrapUpVerdictInput(in))
	})
	t.Run("gapRefs required non-empty IFF gaps", func(t *testing.T) {
		empty := validWrapUpVerdict()
		empty.Verdict = board.WrapUpVerdictGaps
		require.Error(t, board.ValidateWrapUpVerdictInput(empty), "gaps with no refs refuses")

		onPass := validWrapUpVerdict()
		onPass.GapRefs = []string{"tsk-x"}
		require.Error(t, board.ValidateWrapUpVerdictInput(onPass), "pass with refs refuses — a pass files no gaps")
	})
	t.Run("notes are size-capped", func(t *testing.T) {
		in := validWrapUpVerdict()
		in.Notes = variedNotes(board.MaxWrapUpNotesBytes + 1)
		err := board.ValidateWrapUpVerdictInput(in)
		require.Error(t, err)
		require.Contains(t, err.Error(), "notes")
	})
}

// variedNotes builds n bytes of non-degenerate prose (every line unique —
// the textguard's repetition heuristics stay quiet; only the CAP is under
// test here).
func variedNotes(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "wrap-up note line %d: findings reviewed, evidence linked. ", i)
	}
	return b.String()[:n]
}
