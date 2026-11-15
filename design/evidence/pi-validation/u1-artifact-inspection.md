# U-1 — pinned-artifact inspection (pi v1.5.2, linux-x64)

Read-only inspection of the release asset (`tar -tzf`, `file`, `ldd`, `sha256sum`).
No execution of the binary. Asset: `pi-linux-x64.tar.gz`, 58,922,566 bytes,
downloaded 2026-11-10 (scoping phase), re-verified 2026-11-14.

```
$ sha256sum pi-linux-x64.tar.gz
811c6f2c9e7aaee252b5404252e556fb713aa209c1f7f924226aa77d4ef70cb5

$ grep linux-x64 SHA256SUMS        # fetched from the v1.5.2 release
811c6f2c9e7aaee252b5404252e556fb713aa209c1f7f924226aa77d4ef70cb5  pi-linux-x64.tar.gz    # MATCH

$ tar -tzf pi-linux-x64.tar.gz | wc -l
234                                  # full listing: u1-tarball-listing.txt

$ file pi/pi
ELF 64-bit LSB executable, x86-64, version 1 (SYSV), dynamically linked,
interpreter /lib64/ld-linux-x86-64.so.2, for GNU/Linux 3.2.0,
BuildID[sha1]=588d7307f333293c728b74df19b7323c20736899, not stripped

$ ldd pi/pi
libc.so.6 => /lib/x86_64-linux-gnu/libc.so.6
/lib64/ld-linux-x86-64.so.2
libpthread.so.0 / libdl.so.2 / libm.so.6     # pure glibc set; no libstdc++, no libssl

$ ls -la pi/pi
-rwxr-xr-x  122,902,704 bytes (117 MiB); tree total 125 MiB

$ sha256sum pi/pi                   # the RELEASE-tarball binary
6a2ac1c5b0d08b91ce6095591c1960b469d0665c7fc5f32812960a4a17ca0c57
```

## Tree shape (what the base image must provide)

The tarball is a directory TREE, not one file: `pi/` (the bun-compiled
executable), `package.json` (adjacent — `--compile-autoload-package-json`),
`assets/`, `docs/`, `examples/`, `export-html/`, `native/` (linux prebuilds),
`theme/`, `photon_rs_bg.wasm`, `CHANGELOG.md`, `README.md`. The binary is
dynamically linked against **glibc only** — it executes against the workspace
container's userland (the stripped base provides it), exactly as
`runtimes/pi/Dockerfile` rules. Verified live in kind: S5.2d/S5.2e of run
43373000078 (tree mounted RO at `/pi`, `PiVerified=True`).

## Reproducibility cross-check

The overlay image is built from the release **source** archive:
`runtimes/pi/Dockerfile` `PI_SOURCE_SHA256=da14b72e…` — equal to
`SHA256SUMS[pi-1.5.2-source.tar.gz]` (ingress verification holds).

The CI-built binary (kind run 43373000078 pins line) hashes `54c7cc79a793…`
— **not** bit-identical to the release-tarball binary (`6a2ac1c5…`). The
source build does not byte-reproduce upstream's release asset (embedded
build paths/timestamps; upstream makes no reproducibility claim).

Consequence (already the shipped posture, now evidenced): CI stamps and the
supervisor verifies the sha256 of the artifact **the platform builds**; the
release `SHA256SUMS` is ingress verification for the build input, not the
runtime pin. The §8.5 trust shape (hash-the-binary + digest-the-tree) is
unaffected — it never assumed tarball-binary equality.
