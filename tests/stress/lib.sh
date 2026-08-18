#!/usr/bin/env bash
# Shared parsing helper for the G7 harness (tests/stress/g7-stress.sh) and
# its self-test (g7-self-test.sh). Sourced, not duplicated — drift between
# a copy and the harness is invisible, which is how the round-1 head-1
# defect survived into round 2.
#
# metric_sum FILE FAMILY PREFIX: sum the LAST field of every line in FILE
# whose family{label-prefix...} matches. The prefix is the metric family
# plus an opening label substring, WITHOUT a trailing closing brace (the
# exposition continues ",workspace_id=...}" — a trailing } never matches).
metric_sum() {
  local file="$1" fam="$2"
  awk -v fam="$fam" 'index($0, fam) { sum+=$NF } END { print sum+0 }' "$file"
}

# cleanup_verify STORM_PID: check that the burn loops are actually gone.
# Fails (nonzero) when the verification is impossible or loops remain —
# inability to verify is NOT verified (harness README rule).
cleanup_verify() {
  local remain
  remain=$(kubectl exec "$POD" -n "$NS" -c workspace -- sh -c 'pgrep -fc "G7""LOAD" || true' 2>/dev/null | tr -d ' ') || return 1
  [ -n "$remain" ] || return 1
  [ "$remain" = "0" ] || return 2
  return 0
}
