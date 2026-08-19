-- #936: at most ONE default base row, structurally. The store's
-- clear-then-upsert (UpsertBase) and insert-only-default (SeedUpsertBase)
-- maintain it in the common paths, but a partial unique index makes the
-- invariant hold under every reachable state: seed-after-delete
-- re-inserting a second default, concurrent one-call default moves
-- (READ COMMITTED clear-then-upsert races), and any future writer.
--
-- DEDUP FIRST: databases already in a two-default state (the exact bug
-- this migration rescues) would fail CREATE UNIQUE INDEX and stall the
-- upgrade. Deterministic resolution: keep the highest (name, version)
-- — matching ComputeBaseUpdates' last-wins-over-ascending-ListBases
-- resolution, so pills don't change their answer on migration day.
UPDATE image_factory_bases
   SET is_default = FALSE, updated_at = now()
 WHERE is_default
   AND (name, version) NOT IN (
       SELECT name, version FROM image_factory_bases
        WHERE is_default
        ORDER BY name DESC, version DESC
        LIMIT 1
   );

-- NOTE: plain (not CONCURRENT) — migrations run inside a transaction
-- (golang-migrate), which forbids CREATE INDEX CONCURRENTLY; the table
-- is operator-catalog-sized (handful of rows), so the brief lock is fine.
CREATE UNIQUE INDEX IF NOT EXISTS uq_image_factory_bases_single_default
    ON image_factory_bases ((true)) WHERE is_default;
