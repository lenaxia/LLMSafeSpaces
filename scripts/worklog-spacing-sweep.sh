#!/bin/bash
# worklog-spacing sweep (#1583 r19/r20): the pinned rules, unambiguous output.
# Rules: no double blank lines; every heading blank-separated on BOTH sides;
# exactly one trailing newline (a trailing blank AND a missing final newline
# both fail; single whitespace-only tail lines are out of scope).
# Usage: scripts/worklog-spacing-sweep.sh <file>
#   Lines above the terminator are findings; terminator-only = clean.
#   Non-zero exit on findings OR on any sweep error (a failed sweep is not a pass).
f="$1"
out=$(mktemp); err=$(mktemp); trap 'rm -f "$out" "$err"' EXIT
{
  awk 'NR>1 && $0=="" && prev=="" {print NR": DBL"} {prev=$0}' "$f" 2>"$err"
  awk 'NR>1 && prev!="" && $0 ~ /^#/ {print NR": heading-abutment (above)"} {prev=$0}' "$f" 2>>"$err"
  awk 'NR>1 && prev ~ /^#/ && $0 != "" {print NR": heading-abutment (below)"} {prev=$0}' "$f" 2>>"$err"
  # EOF rule (full): exactly one trailing newline — neither a trailing
  # blank line NOR a missing final newline. (Whitespace-only final
  # lines are caught by the DBL sweep only when doubled; a single
  # whitespace-only tail is out of scope and the header says so.)
  if [ "$(tail -c 2 "$f" | od -An -c | tr -d ' ')" = "\n\n" ]; then echo "EOF: trailing blank"; fi
  if [ -n "$(tail -c 1 "$f")" ]; then echo "EOF: missing final newline"; fi
} > "$out"
if [ -s "$err" ]; then echo "SWEEP ERROR (not a pass):"; cat "$err"; exit 2; fi
cat "$out"
echo "== sweeps complete: no lines above = clean =="
[ ! -s "$out" ]
