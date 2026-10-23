package runwatchdog

import (
	"fmt"
	"strings"
	"time"
)

// PRState is everything the predicate needs about one open PR, gathered
// by the workflow's bash (gh api / gh pr list) and piped to the binary
// as JSON. It is pure data — no I/O happens in this package, so every
// edge is unit-testable.
type PRState struct {
	Number             int           `json:"number"`
	HeadSHA            string        `json:"head_sha"`
	HeadBranch         string        `json:"head_branch"`
	HeadCommitDate     time.Time     `json:"head_commit_date"`
	HeadCommitMessage  string        `json:"head_commit_message"`
	Now                time.Time     `json:"now"`
	TotalCheckRuns     int           `json:"total_check_runs"`
	ExistingMarkerSHAs []string      `json:"existing_marker_shas"`
	NoAutoRetrigger    bool          `json:"no_auto_retrigger"`
	MinAge             time.Duration `json:"min_age"`
}

// Decision is the watchdog's verdict for one PR. The workflow executes
// it verbatim: comment (body included so the FORMAT lives here, pinned
// by tests, and the YAML cannot drift), dispatch ci.yml, or nothing.
type Decision struct {
	Number         int    `json:"number"`
	Fault          bool   `json:"fault"`
	ShouldComment  bool   `json:"should_comment"`
	ShouldDispatch bool   `json:"should_dispatch"`
	Reason         string `json:"reason"`
	CommentBody    string `json:"comment_body,omitempty"`
}

// DefaultMinAge heads older than this with zero check-runs fault.
// 15 minutes: ci.yml starts within seconds of a delivered synchronize
// event, so 15m covers event queuing + runner startup with a wide
// margin while still catching the documented persistent-suppression
// windows (40+ min) in the first sweep after the threshold.
const DefaultMinAge = 15 * time.Minute

// skipMarkers are the head-commit-message directives GitHub Actions
// itself honors by creating NO runs (docs: "skip instructions"). The
// renumber bot's commits carry [skip ci] as loop prevention (#1867) —
// without this set the watchdog would false-alarm on every bot commit
// on an open PR.
var skipMarkers = []string{
	"[skip ci]", "[ci skip]", "[no ci]", "[skip actions]", "[actions skip]",
}

// HasSkipMarker reports whether a commit message suppresses Actions
// runs (case-insensitive substring, matching GitHub's own matching).
func HasSkipMarker(msg string) bool {
	lower := strings.ToLower(msg)
	for _, m := range skipMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// FaultMarker returns the exact HTML comment the watchdog and the
// pr-review.yml dispatch-recovery shim dedup on — ONE fault comment
// per head SHA across both callers. The literal is pinned by
// TestFaultMarkerFormat; changing it breaks cross-workflow dedup.
func FaultMarker(sha string) string {
	return "<!-- run-watchdog: fault sha=" + sha + " -->"
}

// NaiveEvaluate is the CONTROL baseline (do not use): age + zero
// check-runs only. It false-alarms on [skip ci] heads — pinned by
// TestNaivePredicateFlagsSkipCi as the documented insufficiency the
// full Evaluate exists to fix (#1867's control-test convention).
func NaiveEvaluate(s PRState) Decision {
	minAge := s.MinAge
	if minAge == 0 {
		minAge = DefaultMinAge
	}
	age := s.Now.Sub(s.HeadCommitDate)
	if age < minAge {
		return Decision{Reason: "head age " + age.Truncate(time.Second).String() + " below threshold"}
	}
	if s.TotalCheckRuns > 0 {
		return Decision{Reason: "check-runs present on head"}
	}
	return Decision{Fault: true, Reason: "aged head with zero check-runs (naive: skip-ci blind)"}
}

// Evaluate is the #1844 detection predicate. A PR head is a delivery
// fault iff ALL hold:
//
//  1. age >= MinAge (default 15m — the grace window for event queuing
//     and runner startup; ci.yml starts within seconds when delivered),
//  2. the head commit message carries NO Actions skip marker
//     ([skip ci] heads legitimately have zero runs),
//  3. TOTAL check-runs on the head SHA is zero. Total, not
//     per-workflow: ci.yml triggers on every pull_request with no
//     paths filter, so ANY delivered synchronize produces at least its
//     runs; path-filtered workflows legitimately skip, and keying on
//     them would false-positive.
//
// Recovery actions: comment (deduped per head SHA via FaultMarker —
// the ai-workflows verdict-marker pattern) and, unless the
// watchdog:no-auto-retrigger label opts out, dispatch ci.yml on the
// branch. Dispatch is self-limiting: a dispatched run creates
// check-runs on the SAME head SHA, so the fault (and with it the
// dispatch) clears on the next sweep.
//
// The auto-retrigger default is OPT-OUT, not opt-in: the fault is
// silent and GitHub-side (four documented instances — nobody labels a
// PR in advance for a fault they cannot see), a ci.yml dispatch is
// side-effect-free (build+test, no pushes), and dedup + self-limiting
// cap amplification.
func Evaluate(s PRState) Decision {
	minAge := s.MinAge
	if minAge == 0 {
		minAge = DefaultMinAge
	}
	age := s.Now.Sub(s.HeadCommitDate)
	if age < minAge {
		return Decision{Reason: fmt.Sprintf("head age %s below threshold %s", age.Truncate(time.Second), minAge)}
	}
	if HasSkipMarker(s.HeadCommitMessage) {
		return Decision{Reason: "head commit carries an Actions skip marker ([skip ci] class) — zero runs is expected"}
	}
	if s.TotalCheckRuns > 0 {
		return Decision{Reason: fmt.Sprintf("%d check-run(s) present on head — delivery happened", s.TotalCheckRuns)}
	}

	// Fault: recovery actions.
	d := Decision{
		Number: s.Number,
		Fault:  true,
		Reason: fmt.Sprintf("head %s aged %s with zero check-runs — synchronize event presumed dropped (#1844)", shortSHA(s.HeadSHA), age.Truncate(time.Second)),
	}
	d.ShouldComment = !contains(s.ExistingMarkerSHAs, s.HeadSHA)
	d.ShouldDispatch = !s.NoAutoRetrigger
	if d.ShouldComment {
		d.CommentBody = FaultCommentBody(s.HeadSHA, age, s.HeadBranch, d.ShouldDispatch)
	}
	return d
}

// FaultCommentBody builds the PR comment: the #1844 escalation tag
// (instances accumulate as issue evidence), the head SHA, the age, the
// recovery levers, and the trailing dedup marker. Emitted by the
// decision (not built in bash) so tests pin the format.
func FaultCommentBody(sha string, age time.Duration, branch string, autoDispatched bool) string {
	var dispatchLine string
	if autoDispatched {
		dispatchLine = fmt.Sprintf("- CI: **auto-dispatched** — `gh workflow run ci.yml --ref %s` (this watchdog; verified working during the 2026-10-23 branch-scoped suppression)", branch)
	} else {
		dispatchLine = fmt.Sprintf("- CI: `gh workflow run ci.yml --ref %s` (auto-dispatch withheld — the `watchdog:no-auto-retrigger` label is set)", branch)
	}
	return fmt.Sprintf("### run-watchdog: delivery fault detected (#1844)\n\n"+
		"Head `%s` was pushed %s ago and carries **zero workflow check-runs** — "+
		"GitHub appears to have dropped the synchronize event for this PR (issue #1844). "+
		"The lane looks \"awaiting review\" while nothing was ever dispatched.\n\n"+
		"Recovery:\n%s\n"+
		"- review round: post `/review` as an admitted author, or push an empty `ci: retrigger` commit (tree-identical) when events are flowing\n"+
		"- escalate: reference this PR + `%s` in a comment on #1844 (evidence trail)\n\n"+
		"Deduplicated per head SHA — a new push resets detection; this comment will not repeat for `%s`.\n\n"+
		"<!-- run-watchdog: fault sha=%s -->",
		sha, age.Truncate(time.Minute), dispatchLine, sha, sha, sha)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
