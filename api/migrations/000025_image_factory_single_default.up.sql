-- #936: at most ONE default base row, structurally. The store's
-- clear-then-upsert (UpsertBase) and insert-only-default (SeedUpsertBase)
-- maintain it in the common paths, but a partial unique index makes the
-- invariant hold under every reachable state: seed-after-delete
-- re-inserting a second default, concurrent one-call default moves
-- (READ COMMITTED clear-then-upsert races), and any future writer.
-- NOTE: plain (not CONCURRENT) — migrations run inside a transaction
-- (golang-migrate), which forbids CREATE INDEX CONCURRENTLY; the table
-- is operator-catalog-sized (handful of rows), so the brief lock is fine.
CREATE UNIQUE INDEX IF NOT EXISTS uq_image_factory_bases_single_default
    ON image_factory_bases ((true)) WHERE is_default;
