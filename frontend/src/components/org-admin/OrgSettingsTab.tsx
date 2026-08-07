import { useCallback, useEffect, useState } from "react";
import { useOutletContext } from "react-router-dom";
import { policiesApi, type OrgPolicy } from "../../api/policies";
import { useToast } from "../../providers/ToastProvider";
import { Button, Card, CardContent, CardHeader, CardTitle } from "../ui";
import { Toggle } from "../ui/Toggle";
import { Spinner } from "../ui";
import type { OrgResponse } from "../../api/orgs";

export function OrgAdminSettingsTab() {
  const { org } = useOutletContext<{ org: OrgResponse; isAdmin: boolean }>();
  const { toast } = useToast();

  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  // Workspace limits
  const [maxWorkspaces, setMaxWorkspaces] = useState(0);
  const [maxActive, setMaxActive] = useState(0);

  // Model/provider restrictions
  const [restrictModels, setRestrictModels] = useState(false);
  const [modelsText, setModelsText] = useState("");
  const [restrictProviders, setRestrictProviders] = useState(false);
  const [providersText, setProvidersText] = useState("");

  // MCP cap + default runtime
  const [maxMcpPerWs, setMaxMcpPerWs] = useState(0);
  const [defaultRuntime, setDefaultRuntime] = useState("");

  // Image restriction (design/0047 D3)
  const [restrictImages, setRestrictImages] = useState(false);
  const [imageHashesText, setImageHashesText] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const policies = await policiesApi.listOrg(org.id).catch(() => [] as OrgPolicy[]);
      const get = (key: string): unknown | undefined =>
        policies.find((p) => p.key === key)?.value;
      setMaxWorkspaces(toInt(get("max_workspaces_per_member"), 0));
      setMaxActive(toInt(get("max_active_workspaces_per_member"), 0));

      const models = toStringArray(get("allowed_models"));
      setRestrictModels(models.length > 0);
      setModelsText(models.join("\n"));

      const providers = toStringArray(get("allowed_providers"));
      setRestrictProviders(providers.length > 0);
      setProvidersText(providers.join("\n"));

      setMaxMcpPerWs(toInt(get("max_mcp_servers_per_workspace"), 0));
      setDefaultRuntime((get("default_runtime") as string) ?? "");

      const imageHashes = toStringArray(get("allowed_image_configs"));
      setRestrictImages(imageHashes.length > 0);
      setImageHashesText(imageHashes.join("\n"));
    } catch {
      // keep defaults
    } finally {
      setLoading(false);
    }
  }, [org.id]);

  useEffect(() => {
    void load();
  }, [load]);

  async function savePolicy(key: string, value: unknown, successMsg: string) {
    setSaving(true);
    try {
      await policiesApi.setOrg(org.id, key, value);
      toast(successMsg);
    } catch {
      toast("Failed to save", "error");
    } finally {
      setSaving(false);
    }
  }

  async function saveWorkspaceLimits() {
    setSaving(true);
    try {
      await policiesApi.setOrg(org.id, "max_workspaces_per_member", maxWorkspaces);
      await policiesApi.setOrg(org.id, "max_active_workspaces_per_member", maxActive);
      toast("Workspace limits saved");
    } catch {
      toast("Failed to save", "error");
    } finally {
      setSaving(false);
    }
  }

  async function saveModelRestrictions() {
    const list = restrictModels
      ? modelsText.split("\n").map((s) => s.trim()).filter(Boolean)
      : [];
    await savePolicy("allowed_models", list, restrictModels ? "Model restrictions saved" : "Model restrictions cleared");
  }

  async function saveProviderRestrictions() {
    const list = restrictProviders
      ? providersText.split("\n").map((s) => s.trim()).filter(Boolean)
      : [];
    await savePolicy("allowed_providers", list, restrictProviders ? "Provider restrictions saved" : "Provider restrictions cleared");
  }

  async function saveMcpAndRuntime() {
    setSaving(true);
    try {
      await policiesApi.setOrg(org.id, "max_mcp_servers_per_workspace", maxMcpPerWs);
      await policiesApi.setOrg(org.id, "default_runtime", defaultRuntime);
      toast("MCP and image settings saved");
    } catch {
      toast("Failed to save", "error");
    } finally {
      setSaving(false);
    }
  }

  async function saveImageRestrictions() {
    const list = restrictImages
      ? imageHashesText.split("\n").map((s) => s.trim()).filter(Boolean)
      : [];
    await savePolicy("allowed_image_configs", list, restrictImages ? "Image restrictions saved" : "Image restrictions cleared");
  }

  if (loading) {
    return (
      <div className="flex items-center justify-center py-12">
        <Spinner />
      </div>
    );
  }

  return (
    <div className="space-y-6">
      {/* Workspace limits */}
      <Card>
        <CardHeader>
          <CardTitle>Workspace Limits</CardTitle>
          <p className="text-sm text-muted-foreground">
            Control how many workspaces members can create. Set to 0 for unlimited.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between">
            <div>
              <span className="font-medium">Max workspaces per member</span>
              <p className="text-xs text-muted-foreground mt-1">
                Total workspaces a member can own (0 = unlimited)
              </p>
            </div>
            <input
              type="number"
              min={0}
              value={maxWorkspaces}
              onChange={(e) => setMaxWorkspaces(Math.max(0, Number(e.target.value)))}
              className="w-24 rounded-md border border-border bg-background px-3 py-1.5 text-sm"
            />
          </div>
          <div className="flex items-center justify-between">
            <div>
              <span className="font-medium">Max active workspaces per member</span>
              <p className="text-xs text-muted-foreground mt-1">
                Workspaces a member can run simultaneously (0 = unlimited)
              </p>
            </div>
            <input
              type="number"
              min={0}
              value={maxActive}
              onChange={(e) => setMaxActive(Math.max(0, Number(e.target.value)))}
              className="w-24 rounded-md border border-border bg-background px-3 py-1.5 text-sm"
            />
          </div>
          <div className="flex justify-end">
            <Button onClick={saveWorkspaceLimits} disabled={saving}>
              {saving ? "Saving..." : "Save Limits"}
            </Button>
          </div>
        </CardContent>
      </Card>

      {/* Model & provider restrictions */}
      <Card>
        <CardHeader>
          <CardTitle>Model & Provider Restrictions</CardTitle>
          <p className="text-sm text-muted-foreground">
            Restrict which AI models and providers members can use. Leave disabled for unrestricted access.
          </p>
        </CardHeader>
        <CardContent className="space-y-6">
          <div className="space-y-3">
            <div className="flex items-center justify-between">
              <div>
                <span className="font-medium">Restrict allowed models</span>
                <p className="text-xs text-muted-foreground mt-1">
                  {restrictModels ? "Only listed models are available to members" : "All models available (unrestricted)"}
                </p>
              </div>
              <Toggle checked={restrictModels} onCheckedChange={setRestrictModels} />
            </div>
            {restrictModels && (
              <textarea
                className="w-full min-h-[100px] rounded-md border border-border bg-background px-3 py-2 text-sm font-mono"
                placeholder={"gpt-4o\nclaude-sonnet-4-20250514\nglm-5.2"}
                value={modelsText}
                onChange={(e) => setModelsText(e.target.value)}
              />
            )}
            <div className="flex justify-end">
              <Button onClick={saveModelRestrictions} disabled={saving} variant="outline">
                Save Models
              </Button>
            </div>
          </div>

          <div className="space-y-3">
            <div className="flex items-center justify-between">
              <div>
                <span className="font-medium">Restrict allowed providers</span>
                <p className="text-xs text-muted-foreground mt-1">
                  {restrictProviders ? "Only listed providers are available to members" : "All providers available (unrestricted)"}
                </p>
              </div>
              <Toggle checked={restrictProviders} onCheckedChange={setRestrictProviders} />
            </div>
            {restrictProviders && (
              <textarea
                className="w-full min-h-[100px] rounded-md border border-border bg-background px-3 py-2 text-sm font-mono"
                placeholder={"openai\nanthropic\nthekaocloud"}
                value={providersText}
                onChange={(e) => setProvidersText(e.target.value)}
              />
            )}
            <div className="flex justify-end">
              <Button onClick={saveProviderRestrictions} disabled={saving} variant="outline">
                Save Providers
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* MCP cap + default runtime */}
      <Card>
        <CardHeader>
          <CardTitle>MCP & Image Defaults</CardTitle>
          <p className="text-sm text-muted-foreground">
            Control MCP server density per workspace and set a default workspace image.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between">
            <div>
              <span className="font-medium">Max MCP servers per workspace</span>
              <p className="text-xs text-muted-foreground mt-1">
                Maximum MCP servers bound to a single workspace (0 = unlimited)
              </p>
            </div>
            <input
              type="number"
              min={0}
              value={maxMcpPerWs}
              onChange={(e) => setMaxMcpPerWs(Math.max(0, Number(e.target.value)))}
              className="w-24 rounded-md border border-border bg-background px-3 py-1.5 text-sm"
            />
          </div>
          <div className="flex items-center justify-between">
            <div>
              <span className="font-medium">Default workspace image</span>
              <p className="text-xs text-muted-foreground mt-1">
                Image config name or hash for new workspaces (empty = platform default)
              </p>
            </div>
            <input
              type="text"
              value={defaultRuntime}
              onChange={(e) => setDefaultRuntime(e.target.value)}
              placeholder="e.g. python-3.12-node-22"
              className="w-64 rounded-md border border-border bg-background px-3 py-1.5 text-sm"
            />
          </div>
          <div className="flex justify-end">
            <Button onClick={saveMcpAndRuntime} disabled={saving}>
              {saving ? "Saving..." : "Save Settings"}
            </Button>
          </div>
        </CardContent>
      </Card>

      {/* Allowed image configs restriction (design/0047 D3) */}
      <Card>
        <CardHeader>
          <CardTitle>Image Restrictions</CardTitle>
          <p className="text-sm text-muted-foreground">
            Restrict which org/platform images members can launch. Leave empty for unrestricted access.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-3">
            <div className="flex items-center justify-between">
              <div>
                <span className="font-medium">Restrict allowed images</span>
                <p className="text-xs text-muted-foreground mt-1">
                  {restrictImages ? "Only listed image hashes are launchable by members" : "All visible images are launchable (unrestricted)"}
                </p>
              </div>
              <Toggle checked={restrictImages} onCheckedChange={setRestrictImages} />
            </div>
            {restrictImages && (
              <textarea
                className="w-full min-h-[100px] rounded-md border border-border bg-background px-3 py-2 text-sm font-mono"
                placeholder={"s-abc123def456\ns-789abcdef012"}
                value={imageHashesText}
                onChange={(e) => setImageHashesText(e.target.value)}
              />
            )}
            <div className="flex justify-end">
              <Button onClick={saveImageRestrictions} disabled={saving} variant="outline">
                Save Image Restrictions
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

function toInt(v: unknown, fallback: number): number {
  if (typeof v === "number") return v;
  return fallback;
}

function toStringArray(v: unknown): string[] {
  if (Array.isArray(v)) return v.filter((x): x is string => typeof x === "string");
  return [];
}
