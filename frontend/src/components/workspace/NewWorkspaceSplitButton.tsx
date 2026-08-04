import { useState, useRef, useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { workspacesApi } from "../../api/workspaces";
import { imageFactoryApi } from "../../api/imageFactory";
import { Spinner } from "../ui/Spinner";
import { generateWorkspaceName } from "../../lib/names";
import { Plus, ChevronDown, Package } from "lucide-react";

/**
 * NewWorkspaceSplitButton is a segmented control: the main `[+]` button
 * launches a workspace with the default image (user → org → platform
 * hierarchy), and the attached `[▼]` button opens a popup menu of Ready
 * image-factory configs for one-off launches.
 *
 * Design: the + box is the primary action (always available, one click).
 * The ▼ is a skinnier attached box that toggles a dropdown.
 */
export function NewWorkspaceSplitButton({ onCreated }: { onCreated: (wsId: string) => void }) {
  const [showPopup, setShowPopup] = useState(false);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const popupRef = useRef<HTMLDivElement>(null);

  const { data: configsData } = useQuery({
    queryKey: ["image-factory-configs"],
    queryFn: () => imageFactoryApi.listConfigs(),
    staleTime: 30_000,
  });

  const readyConfigs = (configsData?.configs ?? [])
    .filter((c) => c.status === "ready")
    .sort((a, b) => a.name.localeCompare(b.name));

  const buildingConfigs = (configsData?.configs ?? [])
    .filter((c) => c.status === "building")
    .sort((a, b) => a.name.localeCompare(b.name));

  const hasContent = readyConfigs.length > 0 || buildingConfigs.length > 0;

  // Close popup on outside click.
  useEffect(() => {
    if (!showPopup) return;
    function handleClick(e: MouseEvent) {
      if (popupRef.current && !popupRef.current.contains(e.target as Node)) {
        setShowPopup(false);
      }
    }
    document.addEventListener("mousedown", handleClick);
    return () => document.removeEventListener("mousedown", handleClick);
  }, [showPopup]);

  async function launchDefault() {
    setCreating(true);
    setError(null);
    try {
      const ws = await workspacesApi.create({ name: generateWorkspaceName() });
      onCreated(ws.id);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to create workspace");
    } finally {
      setCreating(false);
    }
  }

  async function launchWithConfig(hash: string) {
    setShowPopup(false);
    setCreating(true);
    setError(null);
    try {
      const ws = await workspacesApi.create({
        name: generateWorkspaceName(),
        imageConfigHash: hash,
      });
      onCreated(ws.id);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to create workspace");
    } finally {
      setCreating(false);
    }
  }

  return (
    <div className="relative flex items-stretch" ref={popupRef}>
      {/* Primary + button — launches default */}
      <button
        onClick={launchDefault}
        disabled={creating}
        className="flex items-center justify-center rounded-l-md border border-r-0 border-border px-2 py-1 hover:bg-accent disabled:opacity-50"
        aria-label="New workspace (default image)"
        title="New workspace"
      >
        {creating ? <Spinner size="sm" /> : <Plus className="h-4 w-4" />}
      </button>

      {/* Attached dropdown arrow — opens popup */}
      <button
        onClick={() => setShowPopup((v) => !v)}
        disabled={creating || !hasContent}
        aria-label="Select workspace image"
        title={hasContent ? "Choose an image" : "No custom images available"}
      >
        <ChevronDown className="h-3 w-3" />
      </button>

      {/* Popup menu */}
      {showPopup && hasContent && (
        <div className="absolute right-0 top-full z-50 mt-1 w-60 rounded-md border border-border bg-popover p-1 shadow-lg">
          {readyConfigs.length > 0 && (
            <>
              <div className="px-2 py-1 text-[0.65rem] font-medium uppercase text-muted-foreground">
                Custom images
              </div>
              {readyConfigs.map((cfg) => (
                <button
                  key={cfg.id}
                  onClick={() => launchWithConfig(cfg.hash)}
                  className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-sm hover:bg-accent"
                >
                  <Package className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                  <span className="truncate flex-1">{cfg.name}</span>
                  <span className="rounded-full bg-green-500/15 px-1.5 py-0.5 text-[0.6rem] font-medium text-green-600 dark:text-green-400">
                    Ready
                  </span>
                </button>
              ))}
            </>
          )}
          {buildingConfigs.length > 0 && (
            <>
              <div className="px-2 py-1 mt-1 border-t border-border text-[0.65rem] font-medium uppercase text-muted-foreground">
                Building
              </div>
              {buildingConfigs.map((cfg) => (
                <div
                  key={cfg.id}
                  className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-sm opacity-60 cursor-not-allowed"
                  title="Image is still building"
                >
                  <Package className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                  <span className="truncate flex-1">{cfg.name}</span>
                  <span className="rounded-full bg-yellow-500/15 px-1.5 py-0.5 text-[0.6rem] font-medium text-yellow-600 dark:text-yellow-400">
                    Building
                  </span>
                </div>
              ))}
            </>
          )}
        </div>
      )}

      {error && (
        <div className="absolute right-0 top-full z-50 mt-1 w-56 rounded-md border border-destructive/50 bg-destructive/10 p-2 text-xs text-destructive">
          {error}
        </div>
      )}
    </div>
  );
}
