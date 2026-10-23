// Command runwatchdog is the thin caller for pkg/runwatchdog: one
// workflow emission JSON on stdin (see WorkflowEmission — PRState plus
// min_age_minutes) → one Decision JSON on stdout. All detection logic
// lives in the package (unit-tested, red-first); the workflow YAML
// gathers inputs via gh and executes the decision verbatim — the
// numbering-gate pattern (bash gathers, Go decides), so the predicate
// can never drift between tests and CI. The emission contract is
// pinned by tests/ghaworkflows TestRunWatchdogWiringContract.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lenaxia/llmsafespaces/pkg/runwatchdog"
)

func main() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatal("read stdin: %v", err)
	}
	var in runwatchdog.WorkflowEmission
	if err := json.Unmarshal(data, &in); err != nil {
		fatal("parse PR state JSON: %v\ninput was: %s", err, string(data))
	}
	if err := json.NewEncoder(os.Stdout).Encode(runwatchdog.Evaluate(in.Normalize())); err != nil {
		fatal("encode decision: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "runwatchdog: "+format+"\n", args...)
	os.Exit(2)
}
