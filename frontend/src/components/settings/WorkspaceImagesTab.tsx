import { useState, useEffect, useCallback } from "react";
import { imageFactoryApi, type Catalog, type Config } from "../../api/imageFactory";
import { Spinner } from "../ui/Spinner";
import { useToast } from "../../providers/ToastProvider";

export function WorkspaceImagesTab() {
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [configs, setConfigs] = useState<Config[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const { toast } = useToast();

  // Config builder state
  const [name, setName] = useState("");
  const [baseName, setBaseName] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [cat, cfgs] = await Promise.all([
        imageFactoryApi.getCatalog(),
        imageFactoryApi.listConfigs(),
      ]);
      setCatalog(cat);
      setConfigs(cfgs.configs);
      if (cat.bases.length > 0 && !baseName) {
        const def = cat.bases.find((b) => b.isDefault) ?? cat.bases[0];
        setBaseName(def.name);
      }
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to load image factory data");
    } finally {
      setLoading(false);
    }
  }, [baseName]);

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
      const cfg = await imageFactoryApi.createConfig({
        name: name.trim(),
        selection: Array.from(selected).sort(),
        baseName,
      });
      setConfigs((prev) => [...prev, cfg]);
      setName("");
      setSelected(new Set());
      toast({ title: "Image config created", description: `${cfg.name} is building` });
    } catch (e: unknown) {
      toast({
        title: "Failed to create config",
        description: e instanceof Error ? e.message : "Unknown error",
        variant: "destructive",
      });
    }
  };

  if (loading) return <Spinner />;
  if (error) return <p className="text-destructive">{error}</p>;
  if (!catalog) return <p>No catalog data</p>;

  // Check if the current selection + base combination is a known failure.
  // The backend blocks by (selection_hash, base_name) — a combination-level
  // check, not per-extension. We can't compute the hash client-side (it's
  // SHA-256 over the sorted selection), so we check if ANY known failure
  // has the same selection set + base name.
  const currentSelection = Array.from(selected).sort();
  const isCurrentSelectionBlocked = (): boolean => {
    if (currentSelection.length === 0) return false;
    return catalog.knownFailures.some((kf) => {
      if (kf.baseName !== baseName || kf.retriable) return false;
      const kfSorted = [...kf.selection].sort();
      return kfSorted.length === currentSelection.length &&
        kfSorted.every((v, i) => v === currentSelection[i]);
    });
  };

  const statusPill = (status: Config["status"]): string => {
    switch (status) {
      case "ready": return "bg-green-100 text-green-800";
      case "building": return "bg-yellow-100 text-yellow-800";
      case "rejected": return "bg-red-100 text-red-800";
    }
  };

  return (
    <div className="space-y-6">
      {/* Saved configs */}
      <section>
        <h3 className="mb-3 text-sm font-semibold text-muted-foreground uppercase tracking-wide">
          My Workspace Images
        </h3>
        <div className="space-y-2">
          {configs.length === 0 && (
            <p className="text-sm text-muted-foreground">No saved images yet.</p>
          )}
          {configs.map((cfg) => (
            <div key={cfg.id} className="flex items-center justify-between rounded-md border border-border p-3">
              <div>
                <span className="font-medium">{cfg.name}</span>
                <span className="ml-2 text-xs text-muted-foreground">
                  {cfg.selection.length} extensions · {cfg.baseName}
                </span>
              </div>
              <span className={`rounded-full px-2 py-0.5 text-xs font-medium ${statusPill(cfg.status)}`}>
                {cfg.status}
              </span>
            </div>
          ))}
        </div>
      </section>

      {/* Config builder */}
      <section>
        <h3 className="mb-3 text-sm font-semibold text-muted-foreground uppercase tracking-wide">
          Create New Image
        </h3>
        <div className="space-y-4 rounded-md border border-border p-4">
          {/* Name */}
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

          {/* Base selector */}
          <div>
            <label className="block text-sm font-medium mb-1">Base Image</label>
            <select
              value={baseName}
              onChange={(e) => setBaseName(e.target.value)}
              className="w-full rounded-md border border-border px-3 py-1.5 text-sm"
            >
              {catalog.bases.map((b) => (
                <option key={`${b.name}/${b.version}`} value={b.name}>
                  {b.name} ({b.version})
                </option>
              ))}
            </select>
          </div>

          {/* Extension checkboxes */}
          <div>
            <label className="block text-sm font-medium mb-1">Extensions</label>
            <div className="space-y-1">
              {catalog.extensions.map((ext) => {
                return (
                  <label
                    key={ext.id}
                    className="flex items-center gap-2 text-sm cursor-pointer"
                  >
                    <input
                      type="checkbox"
                      checked={selected.has(ext.id)}
                      onChange={() => toggleExtension(ext.id)}
                    />
                    <span className="font-mono">{ext.id}</span>
                    <span className="text-xs text-muted-foreground">
                      ({ext.type}: {ext.value})
                    </span>
                    {ext.description && (
                      <span className="text-xs text-muted-foreground">— {ext.description}</span>
                    )}
                  </label>
                );
              })}
            </div>
          </div>

          {/* Save button */}
          <button
            onClick={handleSave}
            disabled={!name.trim() || selected.size === 0 || isCurrentSelectionBlocked()}
            className="rounded-md bg-blue-600 px-4 py-1.5 text-sm font-medium text-white disabled:opacity-50"
          >
            {isCurrentSelectionBlocked() ? "Combination blocked" : "Create & Build"}
          </button>
        </div>
      </section>
    </div>
  );
}
