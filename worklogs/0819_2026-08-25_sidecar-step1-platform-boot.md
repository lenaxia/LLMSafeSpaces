
## Addendum 4 (2026-08-26 19:45 UTC): catalog 500 — NULL digest scan

GET /image-factory/catalog 500ed ("failed to load bases"): the factory
catalog row added earlier today via direct SQL carried digest NULL,
and ListBases scans into a plain string. The table has no NOT NULL
constraints; the API's own admin handlers never write NULL, but the
operational path this repo's runbooks use (psql) can — reader
availability must not depend on writer discipline. Hotfix: real index
digest into the row (catalog green within 4 min of the report). Durable
fix (#N): scan-side NULL tolerance (sql.NullString) in ListBases/
GetBase, pinned by sqlmock regression tests using the incident's exact
row shape.
