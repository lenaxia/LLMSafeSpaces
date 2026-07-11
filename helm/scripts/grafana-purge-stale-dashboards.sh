#!/bin/sh
# grafana-purge-stale-dashboards.sh — manual cleanup tool for orphaned
# llmsafespaces-* dashboards in Grafana.
#
# Background: when the chart was previously named `llmsafespace`
# (singular), it shipped dashboards with UIDs `llmsafespace-operational`
# and `llmsafespace-billing`. After the chart was renamed to
# `llmsafespaces` (plural), those rows lingered in Grafana's database
# (the sidecar provisioner does not garbage-collect rows whose source
# files no longer exist), and the UID-hash collision tripped Grafana's
# optimistic-concurrency check during dashboard upserts. Every panel
# returned "No data" because operators' bookmarked URLs pointed at the
# stale variant. See worklog 0522.
#
# This script lists every dashboard whose UID begins with `llmsafespaces-`
# (or `llmsafespace-`, the legacy singular prefix), compares against the
# UIDs the current chart is shipping, and offers to delete the orphans
# via Grafana's REST API. Operators run this manually after a chart
# upgrade if "found 2, desired 1" errors appear in Grafana's logs OR if
# dashboards persistently show "No data" despite metrics being scraped.
#
# Usage:
#
#   GRAFANA_URL=https://grafana.example.com \
#   GRAFANA_USER=admin \
#   GRAFANA_PASS=<password> \
#   ./grafana-purge-stale-dashboards.sh
#
# Or via kubectl exec on the Grafana pod (verified working layout):
#
#   GRAFANA_PASS=$(kubectl get secret -n monitoring grafana-admin-creds \
#       -o jsonpath='{.data.admin-password}' | base64 -d)
#   kubectl cp <this-script> monitoring/<grafana-pod>:/tmp/purge.sh -c grafana
#   kubectl exec -n monitoring -c grafana deploy/grafana -- sh -c "
#       export GRAFANA_URL=http://localhost:3000
#       export GRAFANA_USER=admin
#       export GRAFANA_PASS='$GRAFANA_PASS'
#       sh /tmp/purge.sh"
#
# Required tools in the execution environment: sh, curl, grep, sed.
# Intentionally does NOT depend on python3 or jq because Grafana's
# distroless container has neither.
#
# The script is idempotent and safe to run repeatedly. Dry-run mode is
# the default; pass `--apply` as the only argument to actually delete.

set -eu

URL="${GRAFANA_URL:-}"
USER="${GRAFANA_USER:-admin}"
PASS="${GRAFANA_PASS:-}"
APPLY="${1:-}"

if [ -z "$URL" ] || [ -z "$PASS" ]; then
    echo "ERROR: GRAFANA_URL and GRAFANA_PASS environment variables are required" >&2
    echo "" >&2
    echo "  GRAFANA_URL  base URL of Grafana (e.g. https://grafana.example.com)" >&2
    echo "  GRAFANA_USER admin username (default: admin)" >&2
    echo "  GRAFANA_PASS admin password" >&2
    echo "" >&2
    echo "Pass --apply as the only argument to actually delete; default is dry-run." >&2
    exit 2
fi

# Verify the basic tools are present. The default Grafana distroless
# image lacks python3 and jq; the script intentionally uses only
# POSIX-portable utilities.
for tool in curl grep sed sort; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "ERROR: required tool '$tool' not found in PATH" >&2
        exit 2
    fi
done

# The CURRENT set of dashboard UIDs the chart ships. Keep in sync with
# the top-level "uid" field in each helm/dashboards/*.json
# file. The chart_test TestMonitoring_DashboardUIDsAreStable enforces
# the JSON side of this contract; this list is the operator-facing side.
EXPECTED_UIDS="llmsafespaces-operational llmsafespaces-billing"

echo "==> Listing dashboards in Grafana matching prefix llmsafespace*"
LIST_JSON=$(curl -sf --max-time 30 -u "${USER}:${PASS}" "${URL}/api/search?type=dash-db&query=") || {
    echo "ERROR: failed to list dashboards from ${URL}" >&2
    exit 1
}

# Extract every "uid":"..." substring whose value starts with our
# prefix. Grafana's /api/search response is a flat JSON array of dashboard
# summary objects, so a simple grep over the raw response is sufficient
# (and avoids requiring jq/python3, which Grafana's distroless image
# does not ship). Matches both `"uid":"foo"` and `"uid": "foo"` JSON
# spacings.
ALL_OUR_UIDS=$(printf '%s' "$LIST_JSON" \
    | grep -oE '"uid"[[:space:]]*:[[:space:]]*"(llmsafespace-|llmsafespaces-)[^"]*"' \
    | sed -E 's/.*"(llmsafespace[s]?-[^"]+)"/\1/' \
    | sort -u)

if [ -z "$ALL_OUR_UIDS" ]; then
    echo "No llmsafespace[s]-* dashboards found in Grafana. Nothing to do."
    exit 0
fi

echo "  Found dashboards:"
for uid in $ALL_OUR_UIDS; do
    echo "    $uid"
done
echo ""

# Compute orphans: rows in Grafana whose UID is not in EXPECTED_UIDS.
ORPHANS=""
for uid in $ALL_OUR_UIDS; do
    is_expected=0
    for exp in $EXPECTED_UIDS; do
        if [ "$uid" = "$exp" ]; then
            is_expected=1
            break
        fi
    done
    if [ $is_expected -eq 0 ]; then
        ORPHANS="$ORPHANS $uid"
    fi
done

if [ -z "$ORPHANS" ]; then
    echo "==> No orphans. All dashboards in Grafana match the chart's expected UIDs."
    exit 0
fi

echo "==> Orphans (in Grafana but NOT in the chart's expected UID set):"
for uid in $ORPHANS; do
    echo "    $uid"
done
echo ""

if [ "$APPLY" != "--apply" ]; then
    echo "Dry-run mode. Re-run with --apply to delete the orphans:"
    echo "    $0 --apply"
    exit 0
fi

echo "==> Deleting orphans..."
# Track failures so we can report at the end without `set -e` aborting
# mid-loop. The script is idempotent (re-running completes the cleanup),
# but a transient `curl` failure on one orphan should NOT prevent us
# from attempting the others.
DELETE_FAILURES=0
for uid in $ORPHANS; do
    printf "  %s ... " "$uid"
    response=$(curl -s --max-time 30 -u "${USER}:${PASS}" -X DELETE "${URL}/api/dashboards/uid/${uid}" 2>&1) || {
        echo "TRANSPORT ERROR: $response (will retry on next script run)"
        DELETE_FAILURES=$((DELETE_FAILURES + 1))
        continue
    }
    # Grafana returns {"title":"...","message":"Dashboard ... deleted"} on
    # success, {"message":"...","title":"Not found"} on miss. Either is OK
    # (idempotent — a dashboard already gone is the desired state).
    case "$response" in
        *deleted*) echo "deleted" ;;
        *"Not found"*) echo "already gone (skipped)" ;;
        *)
            echo "UNEXPECTED RESPONSE: $response"
            DELETE_FAILURES=$((DELETE_FAILURES + 1))
            ;;
    esac
done

if [ $DELETE_FAILURES -gt 0 ]; then
    echo ""
    echo "==> $DELETE_FAILURES delete(s) failed. Re-run with --apply to retry only the remaining orphans."
    exit 1
fi

echo ""
echo "==> Cleanup complete. The Grafana sidecar provisioner should now be able"
echo "    to upsert the chart's expected dashboards without the 'found 2'"
echo "    optimistic-concurrency error. If it doesn't, scale Grafana to 0"
echo "    and back up to clear in-memory state across replicas:"
echo ""
echo "        kubectl scale -n <grafana-ns> deploy/grafana --replicas=0"
echo "        kubectl scale -n <grafana-ns> deploy/grafana --replicas=<original>"
