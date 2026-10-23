// Command runwatchdog is the thin caller for pkg/runwatchdog: one PR
// state JSON on stdin → one Decision JSON on stdout. All detection
// logic lives in the package (unit-tested, red-first); the workflow
// YAML gathers inputs via gh and executes the decision verbatim — the
// numbering-gate pattern (bash gathers, Go decides), so the predicate
// can never drift between tests and CI.
//
//	Input : {"number":1874,"head_sha":"...","head_branch":"...",
//	         "head_commit_date":"2026-10-23T01:44:56Z",
//	         "head_commit_message":"feat: x","total_check_runs":0,
//	         "existing_marker_shas":[],"no_auto_retrigger":false,
//	         "min_age_minutes":15}
//	Output: {"fault":true,"should_comment":true,...,"comment_body":"..."}
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/runwatchdog"
)

// stdinState wraps pkg/runwatchdog.PRState with an ergonomic
// min_age_minutes field: nanosecond durations are natural in Go and
// hostile in jq, and the workflow builds the input with jq.
type stdinState struct {
	runwatchdog.PRState
	MinAgeMinutes int `json:"min_age_minutes,omitempty"`
}

func main() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatal("read stdin: %v", err)
	}
	var in stdinState
	if err := json.Unmarshal(data, &in); err != nil {
		fatal("parse PR state JSON: %v\ninput was: %s", err, string(data))
	}
	if in.MinAgeMinutes > 0 {
		in.MinAge = time.Duration(in.MinAgeMinutes) * time.Minute
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	if err := json.NewEncoder(os.Stdout).Encode(runwatchdog.Evaluate(in.PRState)); err != nil {
		fatal("encode decision: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "runwatchdog: "+format+"\n", args...)
	os.Exit(2)
}
