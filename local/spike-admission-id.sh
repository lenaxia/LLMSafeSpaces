#!/usr/bin/env bash
# admission-id spike harness (Epic 69 US-69.6, design 0055 §Open items —
# "Caller-supplied admission ID"). Runs against a live pinned opencode pod
# (staged pool) and prints the accept/reject matrix:
#
#   1. baseline prompt (no caller ID)      -> expect 2xx (control)
#   2. prompt with FRESH unique caller ID  -> accept (2xx)?  enables exact
#      entryID->messageID correlation; the localhost text-match fallback in
#      the delivery ledger is then deleted outright (US-69.7/.8)
#   3. prompt REUSING the same caller ID   -> 409 collision per F17, or
#      duplicate admission (recorded — would force the fallback to stay)
#
# Usage:
#   ./local/spike-admission-id.sh <pod-host> <port> <password> <sessionID> [harness-build-dir]
# The harness binary is built on demand into /tmp unless a build dir is given.

set -euo pipefail

HOST="${1:?pod host}"
PORT="${2:?pod port}"
PASS="${3:?workspace password}"
SESSION="${4:?session ID}"
BUILDDIR="${5:-/tmp/spike-admission-id}"

if [ ! -x "$BUILDDIR/admissionid" ]; then
  mkdir -p "$BUILDDIR"
  cat > "$BUILDDIR/main.go" <<'EOF'
// Spike probe: opencode V2 prompt endpoint with caller-supplied IDs.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	host, port, pass, session := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	base := fmt.Sprintf("http://%s:%s", host, port)
	id := fmt.Sprintf("spike-%d", time.Now().UnixNano())

	post := func(body map[string]any) (int, string) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+"/api/session/"+session+"/prompt", bytes.NewReader(b))
		req.SetBasicAuth("opencode", pass)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return -1, err.Error()
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.String()
	}

	code, body := post(map[string]any{
		"prompt": map[string]any{"text": "admission-id spike: baseline (no caller id)"},
		"delivery": "queue",
	})
	fmt.Printf("baseline            -> %d %s\n", code, clip(body))

	// Fresh unique caller ID: the spike's core question.
	code, body = post(map[string]any{
		"id":      id,
		"prompt":  map[string]any{"text": "admission-id spike: fresh unique caller id"},
		"delivery": "queue",
	})
	fmt.Printf("fresh-unique-id     -> %d %s\n", code, clip(body))

	// Reuse: collision semantics (F17 allows 409 on collision).
	code, body = post(map[string]any{
		"id":      id,
		"prompt":  map[string]any{"text": "admission-id spike: DUPLICATE caller id"},
		"delivery": "queue",
	})
	fmt.Printf("duplicate-id-reuse  -> %d %s\n", code, clip(body))
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
EOF
  (cd "$BUILDDIR" && go mod init spike-admission-id >/dev/null 2>&1 || true && go build -o admissionid .)
fi

"$BUILDDIR/admissionid" "$HOST" "$PORT" "$PASS" "$SESSION"
echo
echo "Record the matrix per pinned opencode version in design/0055 §Open items"
echo "(spike: caller-supplied admission ID). Fresh-unique accept + duplicate"
echo "409 => delete the localhost text-match fallback in US-69.7."
