import { useState, useEffect, useCallback, useRef } from "react";
import { useOutletContext } from "react-router-dom";
import { imageFactoryApi, type Catalog, type Config, type Extension } from "../../api/imageFactory";
import type { OrgResponse } from "../../api/orgs";
import { Spinner } from "../ui/Spinner";
import { useToast } from "../../providers/ToastProvider";
import { useConfirmDialog } from "../../hooks/useConfirmDialog";

type ImageScope = "user" | "org" | "platform";

interface WorkspaceImagesTabProps {
  scope?: ImageScope;
}

export function WorkspaceImagesTab({ scope = "user" }: WorkspaceImagesTabProps) {
  const outlet = useOutletContext<{ org?: OrgResponse }>();
  const orgId = scope === "org" ? outlet?.org?.id : undefined;

  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [configs, setConfigs] = useState<Config[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const { toast } = useToast();
  const { confirm: confirmAction, dialog: confirmDialog } = useConfirmDialog();

  const [name, setName] = useState("");
  const [baseName, setBaseName] = useState("");
  const [baseVersion, setBaseVersion] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [expandedConfig, setExpandedConfig] = useState<string | null>(null);
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const [renameValue, setRenameValue] = useState("");
  // #928 phase 2: refresh-flow prefill source. When set, the create form
  // is pre-filled from this config and the base is pre-targeted at the
  // update (bump or migration) so a re-save produces the new-hash config.
  const [refreshSource, setRefreshSource] = useState<Config | null>(null);

  // Track whether the default base has been auto-selected on first load.
  // Using a ref (not state) avoids re-creating the `load` callback when
  // baseName changes, which would trigger a spurious re-fetch loop:
  // load → setBaseName → load recreated → useEffect → load → setLoading(true).
  const defaultBaseSetRef = useRef(false);

  // Determine which configs are editable based on scope. For user scope,
  // only member configs are editable. For org scope, only org configs.
  // For platform scope, only platform configs.
  // Member configs are NEVER editable from org/platform tabs — each member
  // manages their own from their personal settings.
  const canEdit = (cfg: Config): boolean => {
    if (scope === "platform") return cfg.scope === "platform";
    if (scope === "org") return cfg.scope === "org";
    return cfg.scope === "member";
  };

  // Determine which create function to call based on scope.
  const createConfig = (req: { name: string; selection: string[]; baseName: string; baseVersion?: string }) => {
    if (scope === "org" && orgId) return imageFactoryApi.createOrgConfig(orgId, req);
    if (scope === "platform") return imageFactoryApi.createPlatformConfig(req);
    return imageFactoryApi.createConfig(req);
  };

  // The scope label for newly created configs (for the section heading).
  const createScopeLabel = scope === "org" ? "Org" : scope === "platform" ? "Platform" : "Personal";

  const handleDelete = (hash: string, name: string) => {
    confirmAction({
      title: "Delete image?",
      description: `Delete "${name}"? This cannot be undone.`,
      confirmLabel: "Delete",
      destructive: true,
      onConfirm: async () => {
        try {
          await imageFactoryApi.deleteConfig(hash);
          setConfigs(configs.filter((c) => c.hash !== hash));
          setExpandedConfig(null);
          toast("Image config deleted", "success");
        } catch (e) {
          toast(e instanceof Error ? e.message : "Failed to delete config", "error");
        }
      },
    });
  };

  const handleRename = async (hash: string) => {
    const trimmed = renameValue.trim();
    if (!trimmed) return;
    try {
      const updated = await imageFactoryApi.renameConfig(hash, trimmed);
      setConfigs(configs.map((c) => (c.hash === hash ? updated : c)));
      setRenamingId(null);
      setRenameValue("");
      toast("Image config renamed", "success");
    } catch (e) {
      toast(e instanceof Error ? e.message : "Failed to rename config", "error");
    }
  };

  // #928 refresh flow: prefill the create form from a stale config with
  // the base pre-targeted at the update. The form IS the diff review —
  // same extension selection, new base visible in the Base Image select;
  // saving creates a NEW config (new hash, new build; coalesced if the
  // combo exists). The original config is left untouched — migration is
  // explicit consent per ruling #29, never a mutation.
  const handleRefreshPrefill = (cfg: Config) => {
    if (!cfg.updatesAvailable) return;
    const u = cfg.updatesAvailable;
    const targetName = u.kind === "base_migration" && u.defaultBaseName ? u.defaultBaseName : u.currentBaseName;
    const targetVersion = u.kind === "base_migration" && u.defaultBaseVersion
      ? u.defaultBaseVersion
      : u.latestBaseVersion || u.currentBaseVersion;
    // Only pre-target if the catalog actually offers that base (retired
    // bases can't be re-targeted; the pill wouldn't show for them, but
    // be defensive).
    const target = catalog?.bases.find((b) => b.name === targetName && b.version === targetVersion)
      ?? catalog?.bases.find((b) => b.name === targetName);
    if (!target) {
      toast("Update target base not found in catalog", "error");
      return;
    }
    // R2 (#928 review r3): extensions retired since this config was saved
    // are absent from the catalog — invisible checkboxes, unsaveable
    // selection (save 422s "extension is retired"). Auto-drop them from
    // the prefill and say so; an empty remainder aborts BEFORE any state
    // mutation so no banner/prefill half-lands.
    const liveSelection = cfg.selection.filter((id) =>
      catalog?.extensions.some((e) => e.id === id && !e.retired),
    );
    const dropped = cfg.selection.length - liveSelection.length;
    if (liveSelection.length === 0) {
      toast("Every extension in this config has been retired — nothing to refresh; create a new config instead", "error");
      return;
    }
    setRefreshSource(cfg);
    // Scoped name uniqueness (same scope+owner) means the original name
    // is taken — default to a de-conflicted name carrying the update
    // target; the user can edit before saving. R3 (#928 review r3):
    // a SECOND refresh of the same original deterministically collides
    // with the first refresh's config — dedup against the in-hand list.
    let suggested = `${cfg.name} (${target.name} ${target.version})`;
    if (configs.some((c) => c.name === suggested)) {
      for (let n = 2; ; n++) {
        const candidate = `${suggested} ${n}`;
        if (!configs.some((c) => c.name === candidate)) { suggested = candidate; break; }
      }
    }
    setName(suggested);
    if (dropped > 0) {
      toast(`${dropped} retired extension${dropped > 1 ? "s" : ""} dropped from the refresh (no longer in the catalog)`, "success");
    }
    setSelected(new Set(liveSelection));
    setBaseName(target.name);
    setBaseVersion(target.version);
    setExpandedConfig(null);
  };

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [cat, cfgs] = await Promise.all([
        imageFactoryApi.getCatalog(),
        imageFactoryApi.listConfigs(),
      ]);
      setCatalog(cat);
      setConfigs(cfgs);
      if (cat.bases.length > 0 && !defaultBaseSetRef.current) {
        const def = cat.bases.find((b) => b.isDefault) ?? cat.bases[0];
        if (def) {
          setBaseName(def.name);
          setBaseVersion(def.version);
        }
        defaultBaseSetRef.current = true;
      }
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to load image factory data");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const toggleExtension = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const handleSave = async () => {
    if (!name.trim() || selected.size === 0 || !baseName) return;
    try {
      const cfg = await createConfig({
        name: name.trim(),
        selection: Array.from(selected).sort(),
        baseName,
        baseVersion: baseVersion || undefined,
      });
      setConfigs((prev) => [...prev, cfg]);
      setName("");
      setSelected(new Set());
      setRefreshSource(null);
      // N3 (#931 review r5): a refresh-save leaves the base pre-targeted
      // at the update — the cancel-side twin of round-2's C5. Restore
      // the default so the next manual create doesn't silently target
      // the migration base.
      if (refreshSource) {
        const def = catalog?.bases.find((b) => b.isDefault) ?? catalog?.bases[0];
        if (def) { setBaseName(def.name); setBaseVersion(def.version); }
      }
      toast(
        refreshSource
          ? `Refreshed ${refreshSource.name} onto ${baseName} ${baseVersion}: new config is building (the original is unchanged)`
          : `Image config created: ${cfg.name} is building`,
        "success",
      );
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Failed to create config", "error");
    }
  };

  if (loading) return <Spinner />;
  if (error) return <p className="text-destructive">{error}</p>;
  if (!catalog) return <p>No catalog data</p>;

  const currentSelection = Array.from(selected).sort();
  // #928 refresh (C4): selected extensions that don't support the CURRENT
  // target base. ResolveSelection 422s on these at save; surfacing them
  // before the save — especially when a refresh prefill moved the base —
  // is the difference between "why did it fail" and seeing it coming.
  const unsupportedOnBase = currentSelection.filter((id) => {
    const ext = catalog?.extensions.find((e) => e.id === id);
    // Absent-from-catalog = retired (the endpoint excludes retired) —
    // R2: flag those too; they can never save and their checkbox is
    // invisible. If the endpoint ever INCLUDES retired entries, flag
    // them as well (retired can never save either way).
    if (!ext) return true;
    if (ext.retired) return true;
    return !ext.supportedBases.includes(baseName);
  });
  const isCurrentSelectionBlocked = (): boolean => {
    if (currentSelection.length === 0) return false;
    if (!catalog.knownFailures || catalog.knownFailures.length === 0) return false;
    return catalog.knownFailures.some((kf) => {
      if (kf.baseName !== baseName || kf.retriable) return false;
      const kfSorted = [...kf.selection].sort();
      return kfSorted.length === currentSelection.length &&
        kfSorted.every((v, i) => v === currentSelection[i]);
    });
  };

  const statusPill = (status: Config["status"]): string => {
    switch (status) {
      case "ready": return "bg-green-500/15 text-green-600 dark:text-green-400";
      case "building": return "bg-yellow-500/15 text-yellow-600 dark:text-yellow-400";
      case "rejected": return "bg-red-500/15 text-red-600 dark:text-red-400";
    }
  };

  const scopePill = (cfgScope: string) => {
    switch (cfgScope) {
      case "platform": return { label: "Platform", cls: "bg-blue-500/15 text-blue-600 dark:text-blue-400" };
      case "org": return { label: "Org", cls: "bg-purple-500/15 text-purple-600 dark:text-purple-400" };
      default: return { label: "Personal", cls: "bg-gray-500/15 text-gray-600 dark:text-gray-400" };
    }
  };

  // Split configs into sections based on scope. Each tab shows its own
  // managed configs (editable) first, then cross-scope configs (read-only),
  // then member configs (read-only). User scope shows all in one flat list.
  //
  // Org tab: "Org Images" (editable) + "Platform Images" (read-only) + "Member Images" (read-only)
  // Platform tab: "Platform Images" (editable) + "Org Images" (read-only) + "Member Images" (read-only)
  // User tab: "My Workspace Images" (all, editable)
  const isOrgOrPlatform = scope === "org" || scope === "platform";
  const platformConfigs = configs.filter((c) => c.scope === "platform");
  const orgConfigs = configs.filter((c) => c.scope === "org");
  const memberConfigs = configs.filter((c) => c.scope === "member");
  const showMemberSection = isOrgOrPlatform && memberConfigs.length > 0;
  const showOrgSection = scope === "platform" && orgConfigs.length > 0;

  // Primary managed section: what this tab creates/manages.
  const managedConfigs = scope === "platform"
    ? platformConfigs
    : scope === "org"
      ? orgConfigs
      : [];
  const showManagedSection = isOrgOrPlatform;
  // Cross-scope visibility: platform configs from org tab, org configs from platform tab.
  const platformFromOrgTab = scope === "org" && platformConfigs.length > 0;

  const managedHeading = scope === "org" ? "Org Images" : "Platform Images";
  const managedEmptyMsg = scope === "org" ? "No org images yet." : "No platform images yet.";

  const renderConfigCard = (cfg: Config) => {
    const sc = scopePill(cfg.scope);
    const isExpanded = expandedConfig === cfg.id;
    const editable = canEdit(cfg);
    return (
      <div key={cfg.id} className="rounded-md border border-border overflow-hidden">
        <button
          onClick={() => setExpandedConfig(isExpanded ? null : cfg.id)}
          className="flex w-full items-center justify-between p-3 text-left hover:bg-accent/50"
        >
          <div className="flex items-center gap-2 min-w-0">
            <span className="font-medium truncate">{cfg.name}</span>
            <span className={`rounded-full px-1.5 py-0.5 text-[0.6rem] font-medium ${sc.cls}`}>
              {sc.label}
            </span>
          </div>
          <div className="flex items-center gap-2 shrink-0">
            {cfg.updatesAvailable && (
              <span
                title={
                  cfg.updatesAvailable.kind === "base_migration"
                    ? `New base available: ${cfg.updatesAvailable.defaultBaseName} ${cfg.updatesAvailable.defaultBaseVersion} (current: ${cfg.updatesAvailable.currentBaseName} ${cfg.updatesAvailable.currentBaseVersion}). Package versions follow the Debian suite — re-save on the new base to migrate.`
                    : `Base update available: ${cfg.updatesAvailable.currentBaseName} ${cfg.updatesAvailable.latestBaseVersion} (current: ${cfg.updatesAvailable.currentBaseVersion}). Re-save to pick it up.`
                }
                className="rounded-full px-2 py-0.5 text-xs font-medium bg-amber-500/15 text-amber-600 dark:text-amber-400"
              >
                {cfg.updatesAvailable.kind === "base_migration"
                  ? `new base: ${cfg.updatesAvailable.defaultBaseName}`
                  : `base ${cfg.updatesAvailable.latestBaseVersion} available`}
              </span>
            )}
            <span className={`rounded-full px-2 py-0.5 text-xs font-medium ${statusPill(cfg.status)}`}>
              {cfg.status}
            </span>
          </div>
        </button>
        {isExpanded && (
          <div className="border-t border-border px-3 py-2 bg-muted/30">
            <div className="text-xs text-muted-foreground mb-2">
              Base: {cfg.baseName} · {cfg.selection.length} extensions · {cfg.baseVersion}
            </div>
            <div className="flex flex-wrap gap-1">
              {cfg.selection.map((ext) => (
                <span key={ext} className="rounded bg-secondary px-1.5 py-0.5 text-[0.65rem] text-secondary-foreground">
                  {ext}
                </span>
              ))}
            </div>
            {editable && cfg.status !== "building" && (
              <div className="mt-3 flex gap-2">
                {cfg.updatesAvailable && (
                  <button
                    onClick={() => handleRefreshPrefill(cfg)}
                    className="rounded-md bg-amber-600 px-3 py-1 text-xs font-medium text-white hover:bg-amber-700"
                  >
                    Refresh to {cfg.updatesAvailable.kind === "base_migration" ? cfg.updatesAvailable.defaultBaseName : `${cfg.baseName} ${cfg.updatesAvailable.latestBaseVersion}`}
                  </button>
                )}
                {renamingId === cfg.id ? (
                  <>
                    <input
                      type="text"
                      value={renameValue}
                      onChange={(e) => setRenameValue(e.target.value)}
                      onKeyDown={(e) => { if (e.key === "Enter") handleRename(cfg.hash); if (e.key === "Escape") setRenamingId(null); }}
                      className="h-7 w-40 rounded border border-border bg-background px-2 text-xs"
                      autoFocus
                    />
                    <button onClick={() => handleRename(cfg.hash)} className="rounded border border-border px-2 py-1 text-xs hover:bg-accent">Save</button>
                    <button onClick={() => { setRenamingId(null); setRenameValue(""); }} className="rounded border border-border px-2 py-1 text-xs hover:bg-accent">Cancel</button>
                  </>
                ) : (
                  <>
                    <button onClick={() => { setRenamingId(cfg.id); setRenameValue(cfg.name); }} className="rounded border border-border px-2 py-1 text-xs hover:bg-accent">Rename</button>
                    <button onClick={() => handleDelete(cfg.hash, cfg.name)} className="rounded border border-destructive/50 px-2 py-1 text-xs text-destructive hover:bg-destructive/10">Delete</button>
                  </>
                )}
              </div>
            )}
          </div>
        )}
      </div>
    );
  };

  const renderConfigBuilder = () => (
    <section>
      <h3 className="mb-3 text-sm font-semibold text-muted-foreground uppercase tracking-wide">
        Create {createScopeLabel} Image
      </h3>
      <div className="space-y-4 rounded-md border border-border p-4">
        {refreshSource && (
          <div className="flex items-start justify-between gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs">
            <div>
              <span className="font-medium">Refreshing “{refreshSource.name}”</span> — same extensions, new base.
              Saving creates a NEW config; the original stays untouched and launchable.
              {refreshSource.updatesAvailable?.kind === "base_migration" && (
                <div className="mt-1 text-muted-foreground">
                  Package versions follow the Debian suite: {refreshSource.baseName} → {refreshSource.updatesAvailable.defaultBaseName} may change system-package versions.
                </div>
              )}
            </div>
            <button
              onClick={() => {
                setRefreshSource(null);
                setName("");
                setSelected(new Set());
                // Restore the default base — the prefill re-targeted it;
                // leaving it would silently aim the next manual create
                // at the migration base (review round 2, C5).
                const def = catalog?.bases.find((b) => b.isDefault) ?? catalog?.bases[0];
                if (def) { setBaseName(def.name); setBaseVersion(def.version); }
              }}
              className="shrink-0 rounded px-2 py-0.5 text-muted-foreground hover:bg-accent"
              aria-label="Cancel refresh"
            >
              Cancel
            </button>
          </div>
        )}
        <div>
          <label className="block text-sm font-medium mb-1">Name</label>
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. ml-stack"
            className="w-full rounded-md border border-border px-3 py-1.5 text-sm"
          />
        </div>
        <div>
          <label className="block text-sm font-medium mb-1">Base Image</label>
          <select
            value={baseName ? `${baseName}/${baseVersion}` : ""}
            onChange={(e) => {
              const selectedValue = e.target.value;
              const [name, ver] = selectedValue.split("/");
              setBaseName(name ?? "");
              setBaseVersion(ver ?? "");
            }}
            className="w-full rounded-md border border-border bg-background px-3 py-1.5 text-sm text-foreground"
          >
            {catalog.bases.map((b) => (
              <option key={`${b.name}/${b.version}`} value={`${b.name}/${b.version}`}>
                {b.name} ({b.version})
              </option>
            ))}
          </select>
          {unsupportedOnBase.length > 0 && (
            <div className="mt-1 rounded border border-red-500/40 bg-red-500/10 px-2 py-1 text-xs text-red-600 dark:text-red-400">
              Not available on {baseName}: {unsupportedOnBase.join(", ")} — deselect them or pick a base that supports them. Saving with them selected will fail.
            </div>
          )}
        </div>
        <div>
          <label className="block text-sm font-medium mb-1">Extensions</label>
          {(() => {
            const groups: Record<string, Extension[]> = {
              "Language Packs": catalog.extensions.filter((e) => e.type === "mise"),
              "System Packages": catalog.extensions.filter((e) => e.type === "apt"),
              "Files": catalog.extensions.filter((e) => e.type === "file"),
            };
            const groupOrder = ["Language Packs", "System Packages", "Files"];
            return groupOrder
              .filter((g) => (groups[g] || []).length > 0)
              .map((groupName) => (
                <div key={groupName} className="mb-4">
                  <h4 className="mb-1 text-xs font-semibold uppercase text-muted-foreground">{groupName}</h4>
                  <div className="space-y-1">
                    {(groups[groupName] || []).map((ext) => (
                      <label key={ext.id} className="flex items-center gap-2 text-sm cursor-pointer">
                        <input type="checkbox" checked={selected.has(ext.id)} onChange={() => toggleExtension(ext.id)} />
                        <span className="font-mono">{ext.id}</span>
                        <span className="text-xs text-muted-foreground">
                          ({ext.type}: {ext.value.length > 50 ? ext.value.slice(0, 50) + "\u2026" : ext.value})
                        </span>
                        {ext.description && <span className="text-xs text-muted-foreground">— {ext.description}</span>}
                      </label>
                    ))}
                  </div>
                </div>
              ));
          })()}
        </div>
        <button
          onClick={handleSave}
          disabled={!name.trim() || selected.size === 0 || isCurrentSelectionBlocked()}
          className="rounded-md bg-blue-600 px-4 py-1.5 text-sm font-medium text-white disabled:opacity-50"
        >
          {isCurrentSelectionBlocked() ? "Combination blocked" : `Create ${createScopeLabel} Image & Build`}
        </button>
      </div>
    </section>
  );

  return (
    <div className="space-y-6">
      {/* Managed configs — what this tab creates */}
      {showManagedSection && (
        <section>
          <h3 className="mb-3 text-sm font-semibold text-muted-foreground uppercase tracking-wide">
            {managedHeading}
          </h3>
          <div className="space-y-2">
            {managedConfigs.length === 0 && <p className="text-sm text-muted-foreground">{managedEmptyMsg}</p>}
            {managedConfigs.map(renderConfigCard)}
          </div>
        </section>
      )}

      {/* Platform configs visible from org tab (read-only for org admins) */}
      {platformFromOrgTab && (
        <section>
          <h3 className="mb-3 text-sm font-semibold text-muted-foreground uppercase tracking-wide">
            Platform Images
          </h3>
          <div className="space-y-2">
            {platformConfigs.map(renderConfigCard)}
          </div>
        </section>
      )}

      {/* Org configs visible from platform tab (read-only even for platform admin) */}
      {showOrgSection && (
        <section>
          <h3 className="mb-3 text-sm font-semibold text-muted-foreground uppercase tracking-wide">
            Org Images
          </h3>
          <div className="space-y-2">
            {orgConfigs.map(renderConfigCard)}
          </div>
        </section>
      )}

      {/* Member configs — always separate and read-only from org/platform */}
      {showMemberSection && (
        <section>
          <h3 className="mb-3 text-sm font-semibold text-muted-foreground uppercase tracking-wide">
            Member Images
          </h3>
          <div className="space-y-2">
            {memberConfigs.map(renderConfigCard)}
          </div>
        </section>
      )}

      {/* User scope: show all personal configs in one list */}
      {!showManagedSection && !showMemberSection && (
        <section>
          <h3 className="mb-3 text-sm font-semibold text-muted-foreground uppercase tracking-wide">
            My Workspace Images
          </h3>
          <div className="space-y-2">
            {configs.length === 0 && <p className="text-sm text-muted-foreground">No saved images yet.</p>}
            {configs.map(renderConfigCard)}
          </div>
        </section>
      )}

      {renderConfigBuilder()}
      {confirmDialog}
    </div>
  );
}
