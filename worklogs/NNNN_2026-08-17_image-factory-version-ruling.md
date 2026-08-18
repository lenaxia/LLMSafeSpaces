# Worklog: image-factory version-model ruling (#29 — the base is the version axis)

**Date:** 2026-08-17
**PR:** #929
**Issue:** #928 (follow-up implementation tracker)
**Session:** image-factory version-model evaluation with maintainer
**Status:** ruling recorded in design/0046; implementation deferred to #928

## Objective

Decide how the image factory handles extension version selection:
the multi-version question (ffmpeg 6 + 7), and cross-base hash
stability ("take a schematic from bookworm to trixie and get the same
extensions", the Talos property).

## Context

Triggered by user-facing questions: a user wanting two images with two
different ffmpeg versions; a user wanting both versions in ONE image;
and the maintainer's requirement that a hash carried across bases mean
the same extension set (explicitly Talos-shaped).

## Options evaluated

1. **Version-as-ID (status quo)** — `ffmpeg6`/`ffmpeg7` as separate
   flat catalog entries. Works today; catalog clutter; no structure.
2. **Families + version axis** — `ffmpeg@6` structured IDs, chips UX,
   hash preimage still sorted concrete IDs. No hash change; catalog
   schema + picker work.
3. **Aliases over immutable pins** — floating `ffmpeg@recommended`
   resolving at save; enables Renovate-style automation; complex.
4. **Talos-style floating identities** — hash over extension identities
   ONLY, base a resolution domain; same hash on any base. This is the
   option the maintainer initially wanted. Rejected on evaluation: for
   APT-track packages it degenerates (the Debian suite fixes the
   version, so identity+base resolution is just base selection with
   extra machinery), it weakens the reproducer guarantee (same hash,
   silently different bytes across bases), and it requires a one-time
   hash migration of every existing config.

## Decision (ruling #29, recorded in design/0046)

**The base IS the version axis for apt-track system packages.** Version
selection = base selection. Consequences:

- Extension IDs are per-base in meaning; same selection + different
  base = different schematic (base name is in the preimage — intended).
- No version metadata, no floating identities, no resolution table.
- Multi-version-same-tool self-serves: the runtime ships build tools
  and a PVC; users compile the extra version themselves.
- Escape hatch (documented, unbuilt): static-build `file`-type
  extensions at versioned paths are base-independent and co-installable.
- The base-update pill (#928) is the sanctioned migration path.

The maintainer's summary ruling: "if someone needs a specific version
they pick the corresponding base; if they want ffmpeg 6 AND 7 they
install 7 via the base and build 6 themselves."

## Assumption validated (Rule 7)

- Hash preimage includes base name: verified selection.go:60-66 +
   selection_test.go pin (also independently verified by PR review).
- Debian-per-suite single-version constraint for apt packages:
   well-known apt semantics; per-suite version tables consulted.

## Tests Run

Docs-only PR; no code paths touched. Review verified all factual
claims against the live implementation.

## Next Steps

- #928: base-update pill + refresh flow (option E implementation).
- Nothing else; the ruling forecloses the version-selection work.

## Files Modified

design/0046_2026-08-01_image-factory.md (ruling subsection, decision
#29, superseded example fix, affordance naming canonicalized).
