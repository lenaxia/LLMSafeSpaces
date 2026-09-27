#!/bin/bash
# worklog-spacing sweep (#1583 r19/r20): the pinned rules, unambiguous output.
# Rules: no double blank lines; every heading blank-separated on BOTH sides;
# single trailing newline at EOF.
# Usage: scripts/worklog-spacing-sweep.sh <file>
#   Lines above the terminator are findings; terminator-only = clean.
#   Non-zero exit on findings OR on any sweep error (a failed sweep is not a pass).
f="$1"
out=$(mktemp); err=$(mktemp); trap 'rm -f "$out" "$err"' EXIT
{
  awk 'NR>1 && $0=="" && prev=="" {print NR": DBL"} {prev=$0}' "$f" 2>"$err"
  awk 'NR>1 && prev!="" && $0 ~ /^#/ {print NR": heading-abutment (above)"} {prev=$0}' "$f" 2>>"$err"
  awk 'NR>1 && prev ~ /^#/ && $0 != "" {print NR": heading-abutment (below)"} {prev=$0}' "$f" 2>>"$err"
  if [ "$(tail -c 2 "$f" | od -An -c | tr -d ' ')" = "\n\n" ]; then echo "EOF: trailing blank"; fi 2>>"$err"
} > "$out"
if [ -s "$err" ]; then echo "SWEEP ERROR (not a pass):"; cat "$err"; exit 2; fi
cat "$out"
echo "== sweeps complete: no lines above = clean =="
[ ! -s "$out" ]
