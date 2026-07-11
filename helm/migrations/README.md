# `helm/migrations/` — Helm-bundled copy of database migrations

This directory is a **mirror** of `api/migrations/` so the Helm chart
can package every `.sql` file into a `ConfigMap` (see
`templates/migrations-configmap.yaml`). The configmap is consumed by
the `pre-install,pre-upgrade` `Job` defined in
`templates/migration-job.yaml`, which runs `migrate up` against
PostgreSQL before the API/controller pods start.

## Why this directory exists

Helm's `.Files.Glob` is sandboxed to the chart directory — it cannot
read files outside `helm/`. So the migrations have to
live somewhere inside the chart.

## Rule: keep this dir byte-identical to `api/migrations/`

**Drift has broken the cluster twice.** Most recently, on 2026-05-30,
two agents working in parallel each added a `000009_*.sql` migration
to `api/migrations/`, but only one of them copied to this directory.
Result:

- Cluster ran the migration that *was* in this dir
  (`000009_drop_workspace_phase_cache`).
- The other migration (`000009_workspace_version_info`, which adds
  `image_tag` and `agent_version` columns) was never applied.
- `schema_migrations.version=9` looked correct, but the workspaces
  table was missing the columns the API code expected.
- All `GET /api/v1/workspaces` returned **500
  `column "image_tag" does not exist`**.

See worklog 0097 for the full incident analysis.

## How to keep them in sync (today)

After every change to `api/migrations/`:

```bash
make chart-sync-migrations    # cp -a api/migrations/. helm/migrations/
```

(That target is being added alongside this README.) Without that step,
your commit will fail the pre-commit hook and CI lint job, both of
which run `cmd/repolint` to assert byte-identical content here vs
`api/migrations/`.

## Future refactor (tracked, TBD)

This entire directory should go away. The plan is to bundle migrations
into the API container image and have the Helm `pre-install` `Job` run
the API binary (e.g. `args: ["--migrate-only"]`) instead of the external
`migrate/migrate` image plus a ConfigMap copy. After that refactor, the
schema version is coupled to the image version and the "image deployed
but migrations not applied" failure mode becomes impossible.

See `api/migrations/README.md` for the full background and the proposed
design.
