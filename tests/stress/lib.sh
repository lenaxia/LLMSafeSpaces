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
