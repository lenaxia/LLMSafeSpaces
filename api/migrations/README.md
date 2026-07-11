# `api/migrations/` — Database migrations (canonical source)

This is the **canonical** location for database migration files. The API
service's local-dev `make migrate-up`/`make migrate-down` targets in
`api/Makefile` read directly from here.

## File naming

Every migration MUST be a pair of files:

```
NNNNNN_<snake_case_description>.up.sql
NNNNNN_<snake_case_description>.down.sql
```

Where `NNNNNN` is a **strictly sequential, zero-padded six-digit version
number** starting at `000001`. Constraints enforced by `cmd/repolint`:

1. **Unique version numbers** — no two `.up.sql` files may share the same
   `NNNNNN` prefix. (This is the bug that broke the cluster on
   2026-05-30: two agents both used `000009` in parallel; lex order
   silently picked one and the other migration never ran. See worklog
   0097.)
2. **Contiguous sequence** — version numbers must form `1, 2, 3, …, N`
   with no gaps. A missing number means a migration was deleted from
   history (which is unsafe — `schema_migrations` would still record it
   as applied on existing clusters).
3. **Matched up/down pair** — every `NNNNNN_*.up.sql` must have a
   corresponding `NNNNNN_*.down.sql` with the **same name suffix**.
4. **No legacy three-digit files** — `001_initial_schema.sql` and
   `001_initial_schema_rollback.sql` are pre-canonical-naming relics.
   Tracked for removal; do NOT add new files in that format.

These rules are enforced by:

- `pkg/repolint` unit tests (run as part of `make test`)
- `make repolint` CLI
- `.githooks/pre-commit` (run `make install-hooks` once per clone)
- The `Lint` job in `.github/workflows/ci.yml`

## Drift with `helm/migrations/`

The Helm chart bundles migrations into a `ConfigMap` consumed by the
`pre-install,pre-upgrade` migrate `Job`. Helm cannot read files outside
the chart directory, so a copy of every `.sql` file lives at
`helm/migrations/`.

**The two directories MUST be byte-identical.** The repo has been bitten
twice by drift here — the canonical fix is to bundle migrations into the
API container image (or a sidecar migrate image) and remove the chart
copy entirely. Until then:

1. After adding a migration to `api/migrations/`, run
   `make chart-sync-migrations` (forthcoming Make target) which
   `cp -a api/migrations/. helm/migrations/`.
2. `cmd/repolint` validates the two directories match byte-for-byte on
   every commit and CI run.

## Future refactor

The current arrangement has two structural problems:

1. **Split source of truth.** Two parallel directories that must be
   manually kept in sync. Pre-commit catches drift but doesn't prevent
   it.
2. **No coupling of binary version and schema version.** A new API
   image can ship without its expected migrations being on the cluster
   (which is exactly what happened in worklog 0096+0097 — image
   `sha-49dc726` expected `image_tag` column but only the chart's stale
   migration copy was applied).

The proposed refactor:

- Bundle `api/migrations/*.sql` into the API container image at
  `/migrations/`.
- Replace the Helm `pre-install,pre-upgrade` migrate `Job`
  (which uses an external `migrate/migrate` image + ConfigMap) with a
  `pre-install,pre-upgrade` `Job` that uses **the API image itself**
  (e.g. `image: ghcr.io/lenaxia/llmsafespaces/api:<tag>`,
  `args: ["--migrate-only"]`).
- Add a `--migrate-only` flag to the API binary that runs the embedded
  migrations and exits.
- Delete `helm/migrations/` and the configmap template.

After the refactor: schema version is locked to image version. The
"image expects column X but cluster doesn't have it" failure mode
becomes impossible because deploying image `vN` runs `vN`'s migrations
before the API starts.
