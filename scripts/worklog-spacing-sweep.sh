#!/bin/bash
# worklog-spacing sweep (#1583 r18): both pinned rules, unambiguous output.
# Usage: detect.sh <file>   — lines above the terminator are findings;
# only the terminator line means clean.
f="$1"
awk 'NR>1 && $0=="" && prev=="" {print NR": DBL"} {prev=$0}' "$f"
awk 'NR>1 && prev!="" && $0 ~ /^#/ {print NR": heading-abutment"} {prev=$0}' "$f"
if [ "$(tail -c 2 "$f" | od -An -c | tr -d ' ')" = "\n\n" ]; then echo "EOF: trailing blank"; fi
echo "== sweeps complete: no lines above = clean =="
