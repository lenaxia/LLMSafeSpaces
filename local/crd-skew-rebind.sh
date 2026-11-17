#!/usr/bin/env bash
# crd-skew-rebind.sh — issue #2092's one-shot recovery helper for wedged
# born-unbound workspaces (RECOVERY arm; the SOFTENED ruling: no new
# controller re-attempt loop — the 10m backfill cadence already retries,
# the LLMSafeSpacesCRDSkew alert brings the human, and THIS script is
# the human's sweep).
#
# THE SHAPE IT HEALS: a workspace created without a harness on a cluster
# whose Workspace CRD predates 0.44.15 (no spec.harness — helm never
# upgrades crds/, so a normally-upgraded cluster can serve it). The
# apiserver PRUNES every spec.harness write and returns success, so the
# workspace parks in Creating forever: HarnessBindingPending=True,
# annotation llmsafespaces.dev/harness-unbound="true", no pod. Once the
# operator applies the current CRD, the bind persists and the phase
# machine builds the pod on the next reconcile — this script enumerates
# the wedged set and re-drives the bind.
#
# USAGE
#   ./local/crd-skew-rebind.sh                 # enumerate only (always safe)
#   ./local/crd-skew-rebind.sh --rebind        # re-bind wedged → opencode
#   ./local/crd-skew-rebind.sh --rebind=pi     # re-bind wedged → pi
#   ./local/crd-skew-rebind.sh --rebind --yes  # no confirmation prompt
#   ./local/crd-skew-rebind.sh -n prod-ws ...  # namespace (default: all)
#   ./local/crd-skew-rebind.sh --context NAME  # kubectl context
#
# ORDER OF OPERATIONS (the operator runbook, one shot):
#   1. Apply the current CRD FIRST — Flux path bump or:
#        kubectl apply -f helm/crds/workspace.yaml
#      (rebinding before this step is harmless but cannot converge;
#      the script's read-back verify will tell you).
#   2. Run this script with --rebind. Each re-bind is one merge patch:
#      spec.harness=<harness> + the marker annotation removed. The
#      webhook admits ""→value on the marker (bind-once ruling 7a).
#   3. The script VERIFIES every bind read back non-empty (the same
#      discipline as the API's harness_bind_not_persisted check): a
#      still-empty read-back means the CRD is still stale — exit 1 with
#      the remediation, never a silent partial success.
#
# Exit: 0 = enumerated/re-bound cleanly; 1 = a bind did not persist
# (apply the CRD and re-run) or usage error.
set -euo pipefail

MARKER_ANNOTATION="llmsafespaces.dev/harness-unbound"
REBIND=""
HARNESS="opencode"
ASSUME_YES=0
NAMESPACE_ARGS=(-A)
KUBECTL=(kubectl)

usage() { sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --rebind)      REBIND=1 ;;
    --rebind=*)    REBIND=1; HARNESS="${1#--rebind=}" ;;
    --yes|-y)      ASSUME_YES=1 ;;
    -A|--all-namespaces) NAMESPACE_ARGS=(-A) ;;
    -n|--namespace) shift; NAMESPACE_ARGS=(-n "$1") ;;
    --context)     shift; KUBECTL=(kubectl --context "$1") ;;
    -h|--help)     usage ;;
    *) echo "unknown flag: $1" >&2; usage ;;
  esac
  shift
done

case "$HARNESS" in
  opencode|pi) ;;
  *) echo "ERROR: --rebind harness must be opencode or pi (got: $HARNESS)" >&2; exit 1 ;;
esac

# wedge_filter — the born-unbound shape ONLY: spec.harness=="" AND the
# marker (legacy marker-less empties are the controller's backfill
# business, not a wedged bind).
wedge_filter() {
  jq -r --arg marker "$MARKER_ANNOTATION" '
    .items
    | map(select(.spec.harness == "" and (.metadata.annotations[$marker] // "") == "true"))
    | sort_by(.metadata.name)
    | .[]
    | [
        .metadata.namespace // "-",
        .metadata.name,
        (.status.phase // "-"),
        ((.status.conditions // [])
          | map(select(.type == "HarnessBindingPending"))
          | (.[0].reason // "none")),
        ((.status.conditions // [])
          | map(select(.type == "HarnessBindingPending"))
          | (.[0].message // ""))
      ]
    | @tsv'
}

echo "== wedged born-unbound workspaces (spec.harness empty + $MARKER_ANNOTATION marker) =="
WEDGED="$("${KUBECTL[@]}" get workspaces.llmsafespaces.dev "${NAMESPACE_ARGS[@]}" -o json | wedge_filter || true)"

if [ -z "$WEDGED" ]; then
  echo "   none — no born-unbound workspaces are waiting on a bind."
  exit 0
fi

echo "$WEDGED" | column -t -s $'\t' -N NAMESPACE,NAME,PHASE,BIND_REASON -q 2>/dev/null || echo "$WEDGED"
WEDGED_COUNT="$(echo "$WEDGED" | wc -l | tr -d ' ')"
echo "   ($WEDGED_COUNT workspace(s); BindNotPersisted = binds were failing against the stale CRD)"

if [ -z "$REBIND" ]; then
  echo "== enumerate-only. Re-drive the bind with --rebind (after applying the CRD). =="
  exit 0
fi

if [ "$ASSUME_YES" -eq 0 ]; then
  printf "Re-bind %s workspace(s) to harness=%q? [y/N] " "$WEDGED_COUNT" "$HARNESS"
  read -r answer
  case "$answer" in
    y|Y|yes|YES) ;;
    *) echo "aborted."; exit 1 ;;
  esac
fi

FAILED=0
BOUND=0
while IFS=$'\t' read -r NS NAME _PHASE _REASON _MSG; do
  [ -z "$NAME" ] && continue
  NS_ARGS=(-n "$NS")
  [ "$NS" = "-" ] && NS_ARGS=()
  echo "-- re-binding ${NS}/${NAME} → $HARNESS"
  # One merge patch: the bind + the marker removal (the marker only ever
  # marked the waiting state; the webhook admits ""→value on it).
  if ! "${KUBECTL[@]}" patch workspace "$NAME" "${NS_ARGS[@]}" --type=merge \
      -p "{\"spec\":{\"harness\":\"$HARNESS\"},\"metadata\":{\"annotations\":{\"$MARKER_ANNOTATION\":null}}}" >/dev/null 2>&1; then
    echo "   ERROR: patch refused (webhook/RBAC?) — inspect manually:" >&2
    "${KUBECTL[@]}" patch workspace "$NAME" "${NS_ARGS[@]}" --type=merge \
      -p "{\"spec\":{\"harness\":\"$HARNESS\"}}" >/dev/null || true
    FAILED=$((FAILED+1))
    continue
  fi
  # VERIFY (the #2092 discipline): read the object back — a still-empty
  # spec.harness means the apiserver PRUNED the bind: the CRD is still
  # stale. Never report success on a pruned write.
  READBACK="$("${KUBECTL[@]}" get workspace "$NAME" "${NS_ARGS[@]}" -o jsonpath='{.spec.harness}' 2>/dev/null || true)"
  if [ -z "$READBACK" ]; then
    echo "   ERROR: bind did NOT persist (spec.harness reads back empty) — the cluster's Workspace CRD still predates 0.44.15." >&2
    echo "          Apply it first: kubectl apply -f helm/crds/workspace.yaml (or bump the Flux crds path), then re-run." >&2
    FAILED=$((FAILED+1))
    continue
  fi
  echo "   bound (spec.harness=$READBACK); the controller builds the pod on the next reconcile."
  BOUND=$((BOUND+1))
done <<< "$WEDGED"

echo "== done: $BOUND bound, $FAILED failed =="
if [ "$FAILED" -gt 0 ]; then
  exit 1
fi
