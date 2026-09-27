#!/bin/bash
# worklog-spacing sweep (#1583 r19/r20): the pinned rules, unambiguous output.
# Rules: no double blank lines; every heading blank-separated on BOTH sides;
# exactly one trailing newline (a trailing blank AND a missing final newline
# both fail). Whitespace-only lines are not matched by the DBL rule (they
# still count as content for the abutment rules); single or doubled, they
# are out of this sweep's scope.
# Usage: scripts/worklog-spacing-sweep.sh <file>
#   Lines above the terminator are findings; terminator-only = clean.
#   Non-zero exit on findings OR on any sweep error (a failed sweep is not a pass).
f="$1"
out=$(mktemp); err=$(mktemp); trap 'rm -f "$out" "$err"' EXIT
{
  awk 'NR>1 && $0=="" && prev=="" {print NR": DBL"} {prev=$0}' "$f" 2>"$err"
  awk 'NR>1 && prev!="" && $0 ~ /^#/ {print NR": heading-abutment (above)"} {prev=$0}' "$f" 2>>"$err"
  awk 'NR>1 && prev ~ /^#/ && $0 != "" {print NR": heading-abutment (below)"} {prev=$0}' "$f" 2>>"$err"
  # EOF rule: exactly one trailing newline — neither a trailing blank
  # line NOR a missing final newline. (Whitespace-only lines, single
  # or doubled, are out of scope entirely — see the header.)
  if [ "$(tail -c 2 "$f" | od -An -c | tr -d ' ')" = "\n\n" ]; then echo "EOF: trailing blank"; fi
  if [ -n "$(tail -c 1 "$f")" ]; then echo "EOF: missing final newline"; fi
} > "$out"
if [ -s "$err" ]; then echo "SWEEP ERROR (not a pass):"; cat "$err"; exit 2; fi
cat "$out"
echo "== sweeps complete: no lines above = clean =="
[ ! -s "$out" ]
