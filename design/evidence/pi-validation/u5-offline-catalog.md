# U-5 — does `--offline`/`PI_OFFLINE=1` fully suppress the RPC-mode startup catalog refresh?

Question (scoping §8 R4, #11812 background thread): is offline one seam or
several; does anything still dial pi.dev at RPC startup?

## Sources (fetched at tag v1.5.2)

- `packages/coding-agent/src/main.ts:616-620` (flag→env normalization)
- `packages/coding-agent/src/cli/args.ts:343,465` (flag docs)
- `packages/coding-agent/src/core/model-runtime.ts:242-266` (the seam)
- `packages/coding-agent/src/core/remote-catalog-provider.ts` (`refreshModels`)
- `packages/coding-agent/docs/environment-variables.md` (`PI_OFFLINE`)

## Findings

`--offline` is sugar for the env var — one gate, normalized before any
runtime exists:

```ts
// main.ts:616-620
const offlineMode = args.includes("--offline") || isTruthyEnvFlag(process.env.PI_OFFLINE);
if (offlineMode) {
    process.env.PI_OFFLINE = "1";
    process.env.PI_SKIP_VERSION_CHECK = "1";   // also kills the pi.dev version probe
}
```

The catalog network gate is a SINGLE construction argument:

```ts
// model-runtime.ts:242-249
const runtime = new ModelRuntime(
    credentials, config, modelsPath, modelsStore, providers,
    process.env.PI_OFFLINE === undefined,          // modelNetworkEnabled
);
...
// model-runtime.ts:252-263
const refreshFromNetwork = runtime.modelNetworkEnabled && options.allowModelNetwork === true;
...
if (options.refreshOnCreate !== false) {
    await runtime.refresh({ allowNetwork: refreshFromNetwork, signal });
}
```

Every provider refresh honors `context.allowNetwork` before any fetch:

```ts
// remote-catalog-provider.ts:133 (withRemoteCatalog — and :222 withPersonalizedCatalog)
if (!context.allowNetwork || context.signal.aborted) return;
```

With `PI_OFFLINE` set: the startup refresh still RUNS but restores the
persisted overlay from `models-store.json` and returns before the network
leg — zero egress. The 4 h interval check (`REMOTEOTE_CATALOG_REFRESH_INTERVAL_MS`,
line 16) is a secondary throttle behind the same gate. The overlay image is
built `--offline-model-data`, so the bundled catalog + any persisted overlay
is the complete offline model surface.

## Ruling input

Both flag forms are the same seam; there is no second network path at RPC
startup. The platform wires `--offline` per spawn (`pi_rpc.go`, from
`PI_OFFLINE=1` on the supervisor env — scoping §8 R4 "wired explicitly"):
egress-restricted pods are covered by construction. Platform pods should
NOT set PI_OFFLINE unless egress-restricted — the refresh (etag-revalidated,
4 h cadence) is how catalog currency happens for reachable pods; the pinned
binary + S6 fixtures are the compatibility boundary either way.
