#!/usr/bin/env bash
# hack/migration-roundtrip.sh
#
# Apply all up migrations, snapshot the schema, apply all down
# migrations in reverse, verify the schema is empty, re-apply ups,
# snapshot again, and diff the two snapshots. They MUST match — that
# is the round-trip invariant.
#
# Catches: down migration drift (the class that bit us in worklog 0099
# / migration 000009 image_tag). If your down migration doesn't fully
# reverse the up, this fires.
#
# Required env vars: PGHOST, PGUSER, PGPASSWORD, PGDATABASE.
# Caller must ensure the database is empty (drop/recreate the public
# schema if needed) before running.

set -euo pipefail

cd "$(dirname "$0")/.."

# Schema-snapshot filter: drop volatile bits so the comparison is
# structural. \restrict / \unrestrict are pg_dump v17+ session tokens.
# COMMENT ON SCHEMA public is filtered because the default value
# differs across Postgres versions (PG16: empty string; PG17:
# "standard public schema") and across reset-via-DROP-CASCADE vs
# fresh-database initial states. It's metadata, not application schema.
# pg_attver is stripped (PG18 pg_dump emits `ALTER COLUMN ... SET
# (pg_attver=N)` per column): N is a catalog-assigned attribute version
# whose value depends on recreate order, so it differs between the two
# cycles on EVERY column — pure noise, same class as the filters above
# (broke every branch's round-trip leg from 2026-11-13; the #2043 lane
# verified the gate green again once stripped).
snapshot() {
    pg_dump --schema-only --no-owner --no-privileges \
        | grep -v '^--' \
        | grep -v '^SET ' \
        | grep -v '^SELECT pg_catalog.set_config' \
        | grep -v '^\\restrict' \
        | grep -v '^\\unrestrict' \
        | grep -v "^COMMENT ON SCHEMA public " \
        | sed -E 's/ SET \(pg_attver=[0-9]+\)//g' \
        | sort -u
}

# Drop and recreate the public schema so we start from a known-empty
# state. Without this, leftover tables from a previous run would
# pollute the snapshot.
echo "== Resetting public schema =="
psql -v ON_ERROR_STOP=1 -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"

echo "== Apply all up migrations =="
for f in $(ls api/migrations/*.up.sql | sort); do
    echo "  $f"
    psql -v ON_ERROR_STOP=1 -f "$f" >/dev/null
done

echo "== Snapshot schema after up =="
snapshot > /tmp/schema-after-up-1.sql
echo "  $(wc -l < /tmp/schema-after-up-1.sql) lines"

echo "== Apply all down migrations (reverse order) =="
for f in $(ls api/migrations/*.down.sql | sort -r); do
    echo "  $f"
    psql -v ON_ERROR_STOP=1 -f "$f" >/dev/null
done

echo "== Verify schema is empty =="
remaining=$(psql -t -A -c "SELECT tablename FROM pg_tables WHERE schemaname = 'public';")
if [ -n "$remaining" ]; then
    echo "FAIL: tables remain after all down migrations:" >&2
    echo "$remaining" >&2
    echo "" >&2
    echo "Each up migration must have a corresponding down that drops" >&2
    echo "what it created. See worklog 0101 for the class of drift" >&2
    echo "this gate prevents." >&2
    exit 1
fi
echo "  OK: schema is empty"

echo "== Re-apply all up migrations =="
for f in $(ls api/migrations/*.up.sql | sort); do
    psql -v ON_ERROR_STOP=1 -f "$f" >/dev/null
done

echo "== Snapshot schema after second up =="
snapshot > /tmp/schema-after-up-2.sql

echo "== Diff snapshots =="
if ! diff -u /tmp/schema-after-up-1.sql /tmp/schema-after-up-2.sql; then
    echo "" >&2
    echo "FAIL: schema differs after up→down→up cycle." >&2
    echo "Down migrations don't fully reverse their corresponding ups." >&2
    exit 1
fi

echo ""
echo "Round-trip OK: schema is identical after up→down→up."
