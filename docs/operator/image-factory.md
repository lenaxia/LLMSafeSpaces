# Image Factory Operations (Base Lifecycle)

Operator guide for managing the workspace-image base lifecycle: how base
publishes and default moves reach (and never silently change) user
configs. User-facing behavior: `docs/operator/agentd-delivery.md` is
unrelated; this page is about the image factory (design/0046).

## The model (ruling #29)

- Extension IDs are immutable catalog entries; **the base is the version
  axis for apt-track system packages** (ffmpeg on bookworm = 5.1, on
  trixie = 7.x — same ID, different bytes, by design).
- A config pins `(hash, base_name, base_version)` at save and launches
  that image **forever**. New base versions never touch existing configs.

## Publishing a base version

1. Add the row (admin API `POST /v1/admin/image-factory/bases` or the
   seed): name, version, image ref (digest preferred), `isDefault: false`.
2. Existing configs on that base name gain a `base 0.9.0 available`
   pill (computed on read: same-name, newer version). Nothing rebuilds.
3. Users refresh explicitly: prefill → review (same extensions, new
   base) → save → NEW config/hash/build. The original stays launchable.

## Moving the default base (e.g. bookworm → trixie)

**Moving the default is one call.** A `POST /bases` upsert with
`isDefault: true` clears every other default in the same statement, and
a partial unique index (`uq_image_factory_bases_single_default`,
migration 000025) makes "at most one default" structural — enforced by
the database under every path, including concurrent admin upserts and
seed-after-delete restarts. Explicit `isDefault: false` upserts leave no
default (nothing auto-promotes).

1. Publish the new base's versions and their extension coverage first
   (see below).
2. POST-upsert the intended row with `isDefault: true`. Done.

**API restarts do not revert or duplicate runtime defaults.** The boot
seed applies its `isDefault` only to rows it INSERTs, and only when no
default exists (a NOT EXISTS guard, backed by the unique index). If an
operator deletes the default base row entirely, a restart re-seeds the
seed's base WITHOUT re-defaulting it while another default lives. Fresh
installs (empty catalog) still get the seed's default. (#936)
2. Configs on other base names gain the `new base: trixie` pill with
   the migration tooltip (package versions follow the Debian suite).
3. **Before flipping the default**, verify extension coverage: the pill
   offers a refresh to the new base, and save validates every selected
   extension's `supportedBases`. With a catalog where extensions only
   list the old base, every migration refresh fails validation —
   publish `supportedBases` updates for the new base first.
4. Old base versions stay in the catalog (configs pin them). Retiring a
   base name entirely removes the pill for its configs (nothing to
   suggest) but never blocks launching existing Ready configs.

## What users see

| Event | Settings pill | Launch picker | Action |
|---|---|---|---|
| New version of their base | `base 0.9.0 available` | ↻ | Refresh → review → save new config |
| Default base moved | `new base: trixie` | ↻ | Same; banner adds the Debian-suite caveat |
| Nothing new | (none) | (none) | — |

Launching a stale config is always allowed — the frozen image is valid;
staleness is information, never a lockout.

## Failure modes

- **Refresh save 409**: duplicate scoped name. The API returns 409
  naming the colliding config (#936); the form surfaces it directly.
  The prefill avoids the common cases by de-conflicting the suggested
  name against existing configs (`name (base version)`, with numeric
  suffix on repeat refreshes). The fresh-dispatch path also cancels the
  already-fired GH run on conflict (bounded orphan even when the run ID
  is not yet known).
- **Refresh save 422 per-extension**: an extension in the selection is
  unsupported on the target base, or was retired after the config was
  saved. Both surface in the form before save ("Not available on …");
  retired extensions are auto-dropped from the prefill with an info
  toast (a fully-retired selection aborts the refresh).
- **Pill absent after a publish**: check the config is `ready` (pills
  are Ready-only) and the bases table actually lists the new version.
