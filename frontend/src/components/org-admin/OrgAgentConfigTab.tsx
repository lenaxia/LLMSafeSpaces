import { useCallback, useEffect, useState } from "react";
import { useOutletContext } from "react-router-dom";
import { promptsApi } from "../../api/prompts";
import { policiesApi } from "../../api/policies";
import {
  agentRolesApi,
  type AgentRole,
} from "../../api/agentRoles";
import { ApiClientError } from "../../api/client";
import { useToast } from "../../providers/ToastProvider";
import { Button, Card, CardContent, CardHeader, CardTitle, Badge, Input } from "../ui";
import { Toggle } from "../ui/Toggle";
import { Spinner } from "../ui";
import type { OrgResponse } from "../../api/orgs";

export function OrgAgentConfigTab() {
  const { org } = useOutletContext<{ org: OrgResponse; isAdmin: boolean }>();
  const { toast } = useToast();

  const [prompt, setPrompt] = useState("");
  const [allowUser, setAllowUser] = useState(false);
  const [allowMemberMcp, setAllowMemberMcp] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  const [roles, setRoles] = useState<AgentRole[]>([]);
  const [platformRoles, setPlatformRoles] = useState<AgentRole[]>([]);
  const [loadingRoles, setLoadingRoles] = useState(true);
  const [showCreate, setShowCreate] = useState(false);

  const refreshPrompt = useCallback(async () => {
    setLoading(true);
    try {
      const [promptData, policies] = await Promise.all([
        promptsApi.getOrg(org.id),
        policiesApi.listOrg(org.id).catch(() => [] as { key: string; value: unknown }[]),
      ]);
      setPrompt(promptData.prompt);
      setAllowUser(promptData.allowUserPrompt);
      const mcpPolicy = policies.find((p) => p.key === "allow_user_mcp_servers");
      setAllowMemberMcp(mcpPolicy ? mcpPolicy.value === true : false);
    } catch {
      setPrompt("");
      setAllowUser(false);
      setAllowMemberMcp(false);
    } finally {
      setLoading(false);
    }
  }, [org.id]);

  const refreshRoles = useCallback(async () => {
    setLoadingRoles(true);
    try {
      const [orgRoles, platRoles] = await Promise.all([
        agentRolesApi.listOrg(org.id),
        agentRolesApi.listPlatform(),
      ]);
      setRoles(orgRoles);
      setPlatformRoles(platRoles);
    } catch {
      setRoles([]);
    } finally {
      setLoadingRoles(false);
    }
  }, [org.id]);

  useEffect(() => {
    refreshPrompt();
    refreshRoles();
  }, [refreshPrompt, refreshRoles]);

  const save = async () => {
    setSaving(true);
    try {
      await promptsApi.setOrg(org.id, { prompt });
      toast("Organization instructions saved");
    } catch (e) {
      toast(e instanceof Error ? e.message : "Failed to save", "error");
    } finally {
      setSaving(false);
    }
  };

  const toggleAllowUser = async (checked: boolean) => {
    setAllowUser(checked);
    try {
      await promptsApi.setOrg(org.id, { allowUserPrompt: checked });
      toast(checked ? "Member customization enabled" : "Member customization disabled");
    } catch {
      setAllowUser(!checked);
      toast("Failed to toggle", "error");
    }
  };

  const toggleAllowMemberMcp = async (checked: boolean) => {
    setAllowMemberMcp(checked);
    try {
      await policiesApi.setOrg(org.id, "allow_user_mcp_servers", checked);
      toast(checked ? "Member MCP servers enabled" : "Member MCP servers disabled");
    } catch {
      setAllowMemberMcp(!checked);
      toast("Failed to toggle", "error");
    }
  };

  if (loading && loadingRoles) {
    return (
      <div className="flex items-center justify-center py-12">
        <Spinner />
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>Member Customization</CardTitle>
          <p className="text-sm text-muted-foreground">
            When enabled, members can customize the agent's instructions in their workspaces. When disabled, members get a uniform agent.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between">
            <div>
              <span className="font-medium">Allow member prompt customization</span>
              <p className="text-xs text-muted-foreground mt-1">
                {allowUser ? "Members can customize" : "Members get the organization's default agent"}
              </p>
            </div>
            <Toggle checked={allowUser} onCheckedChange={toggleAllowUser} />
          </div>
          <div className="flex items-center justify-between">
            <div>
              <span className="font-medium">Allow member MCP servers</span>
              <p className="text-xs text-muted-foreground mt-1">
                {allowMemberMcp ? "Members can register personal MCP servers" : "Only org admins can add MCP servers"}
              </p>
            </div>
            <Toggle checked={allowMemberMcp} onCheckedChange={toggleAllowMemberMcp} />
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Organization Agent Prompt Customization</CardTitle>
          <p className="text-sm text-muted-foreground">
            Members will follow these instructions in addition to anything they configure themselves. Useful for coding standards, review checklists, or domain context.
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <textarea
            className="w-full min-h-[150px] rounded-md border border-border bg-background px-3 py-2 text-sm font-mono"
            placeholder="Org-specific instructions..."
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            maxLength={10000}
          />
          <div className="flex items-center justify-between">
            <span className="text-xs text-muted-foreground">{prompt.length} / 10,000 chars</span>
            <Button onClick={save} disabled={saving}>
              {saving ? "Saving..." : "Save Instructions"}
            </Button>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <div>
              <CardTitle>Org Agent Roles</CardTitle>
              <p className="text-sm text-muted-foreground">
                Custom roles for your org. Can inherit from platform roles.
              </p>
            </div>
            <Button size="sm" variant="outline" onClick={() => setShowCreate(!showCreate)}>
              {showCreate ? "Cancel" : "Create Role"}
            </Button>
          </div>
        </CardHeader>
        <CardContent>
          {showCreate && (
            <OrgRoleCreateForm
              orgId={org.id}
              platformRoles={platformRoles}
              onDone={() => {
                setShowCreate(false);
                refreshRoles();
              }}
              onError={(msg) => toast(msg, "error")}
            />
          )}
          {loadingRoles ? (
            <div className="py-8 text-center">
              <Spinner />
            </div>
          ) : roles.length === 0 ? (
            <p className="py-8 text-center text-sm text-muted-foreground">
              No org-specific roles. Platform roles are available to all members.
            </p>
          ) : (
            <div className="rounded border border-border overflow-x-auto">
              <table className="w-full text-sm">
                <thead className="border-b border-border bg-muted/50">
                  <tr>
                    <th className="px-3 py-2 text-left font-medium">Name</th>
                    <th className="px-3 py-2 text-left font-medium">Default</th>
                    <th className="px-3 py-2 text-left font-medium">Inherits</th>
                    <th className="px-3 py-2 text-right font-medium">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {roles.map((role) => (
                    <tr key={role.id} className="border-b border-border last:border-0">
                      <td className="px-3 py-2 font-medium">{role.name}</td>
                      <td className="px-3 py-2">
                        {role.isDefault && <Badge variant="success">Default</Badge>}
                      </td>
                      <td className="px-3 py-2">
                        {role.extends ? <Badge variant="muted">{role.extends}</Badge> : "—"}
                      </td>
                      <td className="px-3 py-2 text-right">
                        <Button
                          size="sm"
                          variant="destructive"
                          onClick={async () => {
                            try {
                              await agentRolesApi.deleteOrg(org.id, role.id);
                              toast("Role deleted");
                              refreshRoles();
                            } catch (e) {
                              const msg =
                                e instanceof ApiClientError
                                  ? e.body?.error || "Failed to delete"
                                  : "Failed to delete";
                              toast(msg, "error");
                            }
                          }}
                        >
                          Delete
                        </Button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function OrgRoleCreateForm({
  orgId,
  platformRoles,
  onDone,
  onError,
}: {
  orgId: string;
  platformRoles: AgentRole[];
  onDone: () => void;
  onError: (msg: string) => void;
}) {
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [extendsId, setExtendsId] = useState("");
  const [system, setSystem] = useState("");
  const [isDefault, setIsDefault] = useState(false);
  const [busy, setBusy] = useState(false);

  const handleSubmit = async () => {
    if (!name || !slug) {
      onError("Name and slug are required");
      return;
    }
    setBusy(true);
    try {
      await agentRolesApi.createOrg(orgId, {
        name,
        slug,
        extends: extendsId || undefined,
        isDefault,
        config: system ? { version: 1, system } : undefined,
      });
      onDone();
    } catch (e) {
      onError(e instanceof ApiClientError ? e.body?.error || "Failed to create" : "Failed to create");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mb-4 rounded-md border border-border p-4 space-y-3 bg-muted/30">
      <div className="grid grid-cols-2 gap-3">
        <div>
          <label className="text-xs font-medium text-muted-foreground">Name</label>
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Security Reviewer" />
        </div>
        <div>
          <label className="text-xs font-medium text-muted-foreground">Slug</label>
          <Input value={slug} onChange={(e) => setSlug(e.target.value)} placeholder="security-reviewer" />
        </div>
      </div>
      <div>
        <label className="text-xs font-medium text-muted-foreground">Inherits from (optional)</label>
        <select
          className="w-full rounded-md border border-border bg-background px-3 py-1.5 text-sm"
          value={extendsId}
          onChange={(e) => setExtendsId(e.target.value)}
        >
          <option value="">— None —</option>
          {platformRoles.map((r) => (
            <option key={r.id} value={r.id}>
              {r.name} ({r.slug})
            </option>
          ))}
        </select>
      </div>
      <div>
        <label className="text-xs font-medium text-muted-foreground">System Prompt (additional)</label>
        <textarea
          className="w-full min-h-[80px] rounded-md border border-border bg-background px-3 py-2 text-sm font-mono"
          placeholder="Additional instructions specific to this role..."
          value={system}
          onChange={(e) => setSystem(e.target.value)}
        />
      </div>
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={isDefault}
          onChange={(e) => setIsDefault(e.target.checked)}
          className="rounded border-border"
        />
        Set as org default role
      </label>
      <Button size="sm" onClick={handleSubmit} disabled={busy}>
        {busy ? "Creating..." : "Create Role"}
      </Button>
    </div>
  );
}
