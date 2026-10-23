// Package runwatchdog holds the detection predicate for the #1844
// delivery-fault watchdog: a pushed PR head that GitHub never turned
// into workflow runs (the dropped-synchronize-event class — zero
// check-runs on a head old enough that ci.yml would have started long
// ago).
//
// The decision lives here — not in the workflow YAML — so every edge is
// unit-testable the way numbering-gate's NumberingGateDecision is
// (pkg/repolint gate_test.go); .github/workflows/run-watchdog.yml is a
// thin caller that gathers inputs via gh and executes the decision.
package runwatchdog

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)

func aged(d time.Duration) time.Time { return now.Add(-d) }

func baseState() PRState {
	return PRState{
		Number:            1874,
		HeadSHA:           "550527877621b0ae1f6d5ac41e7f2a8ba15617e2",
		HeadBranch:        "feat/1832-c2-delete-flows",
		HeadCommitDate:    aged(40 * time.Minute), // the documented 40+ min persistent suppression shape
		HeadCommitMessage: "feat(board): delete flows",
		Now:               now,
		TotalCheckRuns:    0,
		MinAge:            15 * time.Minute,
	}
}

// TestEvaluate_FaultDetectedOnSuppressedHead pins the core detection:
// the #1843 instance shape (fresh ordinary push, zero check-runs on the
// head, 40 min old) is a FAULT, and both recovery actions fire.
func TestEvaluate_FaultDetectedOnSuppressedHead(t *testing.T) {
	d := Evaluate(baseState())
	if !d.Fault {
		t.Fatalf("suppressed head (age 40m, zero check-runs) not detected: %s", d.Reason)
	}
	if !d.ShouldComment {
		t.Error("fault must comment (the #1844 evidence trail)")
	}
	if !d.ShouldDispatch {
		t.Error("fault must dispatch CI by default (opt-out label only)")
	}
	if d.CommentBody == "" || !strings.Contains(d.CommentBody, FaultMarker(baseState().HeadSHA)) {
		t.Error("fault decision must carry a comment body containing the dedup marker")
	}
}

// TestEvaluate_SkipCiHeadIsNotAFault — the red that justified the full
// predicate: the renumber bot's own commits carry [skip ci] (loop
// prevention, #1867), so GitHub Actions itself creates no runs for
// them; a naive age+zero-runs predicate false-alarms on every bot
// commit. Control below pins that NaiveEvaluate really does get this
// wrong.
func TestEvaluate_SkipCiHeadIsNotAFault(t *testing.T) {
	for _, msg := range []string{
		"chore: renumber worklog post-rebase (1185) [skip ci]",
		"fix: things [CI SKIP]",
		"merge [no ci] marker",
		"bot push [skip actions]",
		"bot push [actions skip]",
	} {
		s := baseState()
		s.HeadCommitMessage = msg
		if d := Evaluate(s); d.Fault {
			t.Errorf("head with skip marker %q must not be a fault: %s", msg, d.Reason)
		}
	}
}

// TestNaivePredicateFlagsSkipCi pins the insufficiency of the naive
// age+zero-runs predicate (the control, #1867's convention: the
// baseline's wrong behavior is documented, not hidden) — it flags a
// [skip ci] head the full predicate correctly ignores.
func TestNaivePredicateFlagsSkipCi(t *testing.T) {
	s := baseState()
	s.HeadCommitMessage = "chore: renumber worklog post-rebase (1185) [skip ci]"
	if d := NaiveEvaluate(s); !d.Fault {
		t.Error("control drifted: NaiveEvaluate no longer flags [skip ci] heads — the control must keep documenting the naive predicate's blind spot")
	}
}

// TestEvaluate_YoungHeadNotFault — grace window: ci.yml normally starts
// within seconds, but event queuing and runner startup justify a
// threshold; a 5-minute-old head with zero runs is PENDING, not FAULT.
func TestEvaluate_YoungHeadNotFault(t *testing.T) {
	s := baseState()
	s.HeadCommitDate = aged(5 * time.Minute)
	if d := Evaluate(s); d.Fault {
		t.Errorf("5-minute-old head must not fault (default threshold 15m): %s", d.Reason)
	}
}

// TestEvaluate_ThresholdIsInclusive — at exactly MinAge the head is
// eligible (age >= MinAge faults when runs are absent).
func TestEvaluate_ThresholdIsInclusive(t *testing.T) {
	s := baseState()
	s.HeadCommitDate = aged(15 * time.Minute)
	if d := Evaluate(s); !d.Fault {
		t.Errorf("head aged exactly the threshold with zero runs must fault: %s", d.Reason)
	}
}

// TestEvaluate_CheckRunsPresentNotFault — check-runs on the head mean
// delivery happened; path-filtered workflows legitimately skip, which
// is why the predicate keys on TOTAL check-runs (ci.yml has no paths
// filter — every real synchronize produces at least its runs).
func TestEvaluate_CheckRunsPresentNotFault(t *testing.T) {
	s := baseState()
	s.TotalCheckRuns = 7
	if d := Evaluate(s); d.Fault {
		t.Errorf("head with check-runs must not fault: %s", d.Reason)
	}
}

// TestEvaluate_OneCheckRunSufficesNotFault — a single check-run (even a
// queued one) proves the event was delivered.
func TestEvaluate_OneCheckRunSufficesNotFault(t *testing.T) {
	s := baseState()
	s.TotalCheckRuns = 1
	if d := Evaluate(s); d.Fault {
		t.Errorf("head with a single check-run must not fault: %s", d.Reason)
	}
}

// TestEvaluate_MarkerSuppressesCommentAndDispatch — one marker per
// head SHA gates BOTH recovery actions (the ai-workflows verdict-marker
// pattern, extended to the dispatch per the r1 review: a dispatched
// ci.yml run PUSHES a sha-/ts- image set from the unmerged branch, so
// repeat dispatches per sweep would publish repeat image sets —
// exactly-once-per-head is the amplification cap). The fault itself
// still stands (the head still has zero runs).
func TestEvaluate_MarkerSuppressesCommentAndDispatch(t *testing.T) {
	s := baseState()
	s.ExistingMarkerSHAs = []string{s.HeadSHA}
	d := Evaluate(s)
	if !d.Fault {
		t.Fatal("fault persists — the head still has zero runs")
	}
	if d.ShouldComment {
		t.Error("marker for the same head SHA must suppress the comment")
	}
	if d.ShouldDispatch {
		t.Error("marker for the same head SHA must suppress the dispatch too — a ci.yml dispatch publishes an image set from the unmerged branch; exactly-once-per-SHA is the cap")
	}
}

// TestEvaluate_DedupMarkerOtherSHAIgnored — markers pinned to OLDER
// heads suppress NEITHER action for a NEW suppressed head (a new push
// resets the per-SHA dedup space).
func TestEvaluate_DedupMarkerOtherSHAIgnored(t *testing.T) {
	s := baseState()
	s.ExistingMarkerSHAs = []string{"1111111111111111111111111111111111111111"}
	d := Evaluate(s)
	if !d.ShouldComment {
		t.Error("marker on a different SHA must not suppress this head's comment")
	}
	if !d.ShouldDispatch {
		t.Error("marker on a different SHA must not suppress this head's dispatch")
	}
}

// TestEvaluate_MarkerAndLabelBothSuppressDispatch — the per-SHA dedup
// and the opt-out label are independent dispatch suppressors; with
// both present the comment stays suppressed by the marker and the
// dispatch by either (combination pin — the r2 semantics).
func TestEvaluate_MarkerAndLabelBothSuppressDispatch(t *testing.T) {
	s := baseState()
	s.ExistingMarkerSHAs = []string{s.HeadSHA}
	s.NoAutoRetrigger = true
	d := Evaluate(s)
	if !d.Fault {
		t.Fatal("fault persists")
	}
	if d.ShouldComment || d.ShouldDispatch {
		t.Errorf("marker+label must suppress both actions, got comment=%v dispatch=%v", d.ShouldComment, d.ShouldDispatch)
	}
}

// TestEvaluate_NoAutoRetriggerLabel — the opt-out: comment still posts
// (evidence), the ci.yml dispatch is withheld.
func TestEvaluate_NoAutoRetriggerLabel(t *testing.T) {
	s := baseState()
	s.NoAutoRetrigger = true
	d := Evaluate(s)
	if !d.Fault || !d.ShouldComment {
		t.Fatal("opt-out label affects dispatch only — fault detection and evidence comment must stand")
	}
	if d.ShouldDispatch {
		t.Error("watchdog:no-auto-retrigger must suppress the CI dispatch")
	}
}

// TestEvaluate_MinAgeDefault — zero MinAge falls back to the 15-minute
// default (thin callers may omit it).
func TestEvaluate_MinAgeDefault(t *testing.T) {
	s := baseState()
	s.MinAge = 0
	if d := Evaluate(s); !d.Fault {
		t.Errorf("default threshold (15m) must apply when MinAge is zero: %s", d.Reason)
	}
	s.HeadCommitDate = aged(10 * time.Minute)
	if d := Evaluate(s); d.Fault {
		t.Errorf("default threshold must still grant the grace window: %s", d.Reason)
	}
}

// TestFaultMarkerFormat pins the exact marker literal shared with the
// bash callers (pr-review.yml's dispatch-recovery posts the identical
// marker once that PR merges — until then the two pins live in their
// respective trees; any drift breaks cross-workflow dedup).
func TestFaultMarkerFormat(t *testing.T) {
	got := FaultMarker("abc123")
	want := "<!-- run-watchdog: fault sha=abc123 -->"
	if got != want {
		t.Errorf("marker drifted: got %q want %q — bash callers dedup on this literal", got, want)
	}
}

// TestFaultCommentBodyContract pins the comment body contract: the
// escalation tag (#1844 — instances accumulate as evidence), the head
// SHA, the recovery lever, and the trailing dedup marker.
func TestFaultCommentBodyContract(t *testing.T) {
	body := FaultCommentBody(baseState().HeadSHA, 40*time.Minute, baseState().HeadBranch, true)
	for _, needle := range []string{"#1844", baseState().HeadSHA, "ci.yml", FaultMarker(baseState().HeadSHA)} {
		if !strings.Contains(body, needle) {
			t.Errorf("comment body lost contract element %q", needle)
		}
	}
	if !strings.Contains(body, "auto-dispatched") {
		t.Error("auto-dispatched body must say so (operator trust)")
	}
	manual := FaultCommentBody(baseState().HeadSHA, 40*time.Minute, baseState().HeadBranch, false)
	if !strings.Contains(manual, "manual") && strings.Contains(manual, "auto-dispatched") {
		t.Error("non-dispatched body must not claim auto-dispatch")
	}
}
