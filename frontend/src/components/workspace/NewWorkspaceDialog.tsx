import { useState, useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "../ui/Button";
import { Spinner } from "../ui/Spinner";
import { generateWorkspaceName } from "../../lib/names";
import { orgsApi, type OrgResponse } from "../../api/orgs";
import { imageFactoryApi, type Config } from "../../api/imageFactory";
import { Package, Check, Box } from "lucide-react";
import { cn } from "../../lib/utils";

export interface NewWorkspaceParams {
  name: string;
  orgId?: string;
  imageConfigHash?: string;
}

interface Props {
  onCreate: (params: NewWorkspaceParams) => void;
  onCancel: () => void;
  loading?: boolean;
}

/**
 * NewWorkspaceDialog presents the workspace creation flow with an optional
 * image-factory config picker (design/0046). Ready configs are selectable;
 * building/rejected configs show a status pill but are disabled.
 */
export function NewWorkspaceDialog({ onCreate, onCancel, loading }: Props) {
  const [orgs, setOrgs] = useState<OrgResponse[]>([]);
  const [selectedOrg, setSelectedOrg] = useState<string>("");
  const [selectedHash, setSelectedHash] = useState<string | null>(null);

  useEffect(() => {
    orgsApi.list().then((data) => setOrgs(data || [])).catch(() => {});
  }, []);

  const { data: configsData, isLoading: configsLoading } = useQuery({
    queryKey: ["image-factory-configs"],
    queryFn: () => imageFactoryApi.listConfigs(),
    staleTime: 30_000,
  });

  const configs = (configsData?.configs ?? []).sort((a, b) => {
    if (a.status === "ready" && b.status !== "ready") return -1;
    if (a.status !== "ready" && b.status === "ready") return 1;
    return a.name.localeCompare(b.name);
  });
  const hasReadyConfigs = configs.some((c) => c.status === "ready");

  return (
    <div className="flex flex-col gap-3 p-4 max-w-sm">
      <h3 className="text-sm font-semibold">New Workspace</h3>
      <p className="text-xs text-muted-foreground">A workspace will be created and ready to chat.</p>

      {orgs.length > 0 && (
        <select
          value={selectedOrg}
          onChange={(e) => setSelectedOrg(e.target.value)}
          className="h-8 rounded border border-border bg-background px-2 text-sm focus:outline-none focus:ring-1 focus:ring-ring"
        >
          <option value="">Personal workspace</option>
          {orgs.map((org) => (
            <option key={org.id} value={org.id}>
              {org.name}
            </option>
          ))}
        </select>
      )}

      <button
        onClick={() => setSelectedHash(null)}
        className={cn(
          "flex w-full items-center gap-3 rounded-md border p-3 text-left transition-colors hover:bg-accent",
          selectedHash === null && "border-primary ring-1 ring-primary",
        )}
      >
        <Box className="h-4 w-4 shrink-0 text-muted-foreground" />
        <div className="flex-1 min-w-0">
          <div className="text-sm font-medium">Default image</div>
          <div className="text-xs text-muted-foreground">Standard runtime environment</div>
        </div>
        {selectedHash === null && <Check className="h-4 w-4 text-primary" />}
      </button>

      {configsLoading && (
        <div className="flex items-center justify-center py-2">
          <Spinner size="sm" />
        </div>
      )}

      {!configsLoading && hasReadyConfigs && (
        <>
          <div className="flex items-center gap-2 px-1 pt-1">
            <Package className="h-3.5 w-3.5 text-muted-foreground" />
            <span className="text-xs font-medium text-muted-foreground">Custom images</span>
          </div>
          {configs.map((cfg) => (
            <ConfigRow
              key={cfg.id}
              config={cfg}
              selected={selectedHash === cfg.hash}
              onSelect={() => cfg.status === "ready" && setSelectedHash(cfg.hash)}
            />
          ))}
        </>
      )}

      <div className="flex justify-end gap-2 pt-1">
        <Button type="button" variant="ghost" size="sm" onClick={onCancel}>Cancel</Button>
        <Button
          size="sm"
          disabled={loading}
          onClick={() =>
            onCreate({
              name: generateWorkspaceName(),
              orgId: selectedOrg || undefined,
              imageConfigHash: selectedHash ?? undefined,
            })
          }
        >
          {loading ? "Creating..." : "Create"}
        </Button>
      </div>
    </div>
  );
}

function ConfigRow({
  config,
  selected,
  onSelect,
}: {
  config: Config;
  selected: boolean;
  onSelect: () => void;
}) {
  const isReady = config.status === "ready";

  return (
    <button
      onClick={onSelect}
      disabled={!isReady}
      className={cn(
        "flex w-full items-center gap-3 rounded-md border p-3 text-left transition-colors",
        isReady && "hover:bg-accent",
        !isReady && "opacity-60 cursor-not-allowed",
        selected && "border-primary ring-1 ring-primary",
      )}
    >
      <Package className="h-4 w-4 shrink-0 text-muted-foreground" />
      <div className="flex-1 min-w-0">
        <div className="text-sm font-medium truncate">{config.name}</div>
        <div className="text-xs text-muted-foreground truncate">
          {config.selection.length} packages
        </div>
      </div>
      <StatusPill status={config.status} />
      {selected && <Check className="h-4 w-4 text-primary" />}
    </button>
  );
}

function StatusPill({ status }: { status: Config["status"] }) {
  if (status === "ready") {
    return (
      <span className="rounded-full bg-green-500/15 px-2 py-0.5 text-[0.65rem] font-medium text-green-600 dark:text-green-400">
        Ready
      </span>
    );
  }
  if (status === "building") {
    return (
      <span className="rounded-full bg-yellow-500/15 px-2 py-0.5 text-[0.65rem] font-medium text-yellow-600 dark:text-yellow-400">
        Building
      </span>
    );
  }
  if (status === "rejected") {
    return (
      <span className="rounded-full bg-red-500/15 px-2 py-0.5 text-[0.65rem] font-medium text-red-600 dark:text-red-400">
        Rejected
      </span>
    );
  }
  return null;
}
