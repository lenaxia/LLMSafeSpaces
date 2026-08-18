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

1. Publish the new base's versions first; then set `isDefault` on the
   intended row (POST-upsert the bases resource — there is no PUT; the
   upsert keys on name+version). Exactly ONE default row — with
   multiple `isDefault` rows, pill computation resolves to the
   highest-sorted name/version (ListBases orders ascending and the
   resolver keeps the last IsDefault row it sees), so treat extra
   defaults as operator error.
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

- **Refresh save 500 "failed to save config"**: duplicate scoped name.
  The create paths currently return 500 (not 409) on the scoped-unique
  violation; the prefill avoids the common cases by de-conflicting the
  suggested name against existing configs (`name (base version)`, with
  numeric suffix on repeat refreshes). If the user hand-edits back into
  a collision, the error surfaces as a toast — a 4xx mapping for this
  case is a known backend debt item.
- **Refresh save 422 per-extension**: an extension in the selection is
  unsupported on the target base, or was retired after the config was
  saved. Both surface in the form before save ("Not available on …");
  retired extensions are auto-dropped from the prefill with an info
  toast (a fully-retired selection aborts the refresh).
- **Pill absent after a publish**: check the config is `ready` (pills
  are Ready-only) and the bases table actually lists the new version.
