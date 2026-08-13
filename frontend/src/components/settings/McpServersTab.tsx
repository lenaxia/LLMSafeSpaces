import { useState, useCallback, useEffect, useMemo } from "react";
import { useOutletContext } from "react-router-dom";
import {
  adminMcpServersApi,
  orgMcpServersApi,
  userMcpServersApi,
} from "../../api/mcpServers";
import { secretsApi } from "../../api/secrets";
import type { OrgResponse } from "../../api/orgs";
import type {
  McpServerResponse,
  CreateMcpServerRequest,
  McpTransport,
} from "../../api/mcpServerTypes";
import type { SecretResponse } from "../../api/secrets";
import { safeConfirm } from "../../lib/safeConfirm";

type ApiClient = {
  list: () => Promise<McpServerResponse[]>;
  create: (req: CreateMcpServerRequest) => Promise<McpServerResponse>;
  update: (id: string, req: { enabled?: boolean }) => Promise<McpServerResponse>;
  delete: (id: string) => Promise<void>;
};

interface McpServersTabProps {
  scope: "admin" | "org" | "user";
}

export function McpServersTab({ scope }: McpServersTabProps) {
  // Org scope reads orgId from the OrgAdminLayout outlet context (mirrors
  // OrgAgentConfigTab). The router element at /orgs/:id/mcp-servers cannot
  // pass orgId as a prop without a wrapper, and the previous absence caused
  // every org-scope API call to fall through to the user endpoint.
  // Safe destructuring: the user-scope tab is mounted under SettingsPage
  // which provides no outlet context, so useOutletContext() may return
  // undefined — use optional chaining to avoid a crash.
  const outlet = useOutletContext<{ org?: OrgResponse; isAdmin?: boolean }>();
  const orgId = scope === "org" ? outlet?.org?.id : undefined;
  const [servers, setServers] = useState<McpServerResponse[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);

  const api: ApiClient = useMemo(
    () => ({
      list: () => (scope === "org" && orgId ? orgMcpServersApi.list(orgId) : scope === "admin" ? adminMcpServersApi.list() : userMcpServersApi.list()),
      create: (req) => scope === "org" && orgId ? orgMcpServersApi.create(orgId, req) : scope === "admin" ? adminMcpServersApi.create(req) : userMcpServersApi.create(req),
      update: (id, req) => scope === "org" && orgId ? orgMcpServersApi.update(orgId, id, req) : scope === "admin" ? adminMcpServersApi.update(id, req) : userMcpServersApi.update(id, req),
      delete: (id) => (scope === "org" && orgId ? orgMcpServersApi.delete(orgId, id) : scope === "admin" ? adminMcpServersApi.delete(id) : userMcpServersApi.delete(id)) as Promise<void>,
    }),
    [scope, orgId],
  );

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.list();
      setServers(data || []);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load MCP servers");
    } finally {
      setLoading(false);
    }
  }, [api]);

  useEffect(() => {
    void load();
  }, [load]);

  const handleDelete = async (id: string) => {
    if (!safeConfirm("Delete this MCP server? Bound workspaces will lose its tools.")) return;
    try {
      await api.delete(id);
      setServers(servers.filter((s) => s.id !== id));
    } catch (e) {
      setError(e instanceof Error ? e.message : "Delete failed");
    }
  };

  const handleToggle = async (server: McpServerResponse) => {
    try {
      await api.update(server.id, { enabled: !server.enabled });
      setServers(servers.map((s) => (s.id === server.id ? { ...s, enabled: !s.enabled } : s)));
    } catch (e) {
      setError(e instanceof Error ? e.message : "Toggle failed");
    }
  };

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">MCP Servers</h2>
          <p className="text-sm text-muted-foreground">
            Register external MCP servers whose tools your agents can use.
          </p>
        </div>
        <button
          onClick={() => setShowForm(!showForm)}
          className="rounded-md bg-primary px-3 py-1.5 text-sm text-primary-foreground hover:bg-primary/90"
        >
          {showForm ? "Cancel" : "Add Server"}
        </button>
      </div>

      {error && (
        <div className="rounded-md border border-destructive/50 bg-destructive/10 p-3 text-sm text-destructive">
          {error}
        </div>
      )}

      {showForm && (
        <McpServerForm
          scope={scope}
          orgId={orgId}
          onCreated={() => {
            setShowForm(false);
            void load();
          }}
          onCancel={() => setShowForm(false)}
        />
      )}

      {servers.length === 0 && !loading ? (
        <p className="text-sm text-muted-foreground py-8 text-center">
          No MCP servers configured. Click "Add Server" to register one.
        </p>
      ) : (
        <div className="space-y-2">
          {servers.map((s) => (
            <div
              key={s.id}
              className="flex items-center justify-between rounded-md border border-border p-3"
            >
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <span className="font-medium">{s.name}</span>
                  <span className="rounded bg-muted px-1.5 py-0.5 text-xs">{s.transport}</span>
                  {!s.enabled && (
                    <span className="rounded bg-muted-foreground/20 px-1.5 py-0.5 text-xs">disabled</span>
                  )}
                </div>
                <p className="truncate text-xs text-muted-foreground">
                  {s.transport === "stdio" ? s.command : s.url}
                </p>
              </div>
              <div className="flex items-center gap-2">
                <button
                  onClick={() => handleToggle(s)}
                  className="rounded px-2 py-1 text-xs hover:bg-accent"
                >
                  {s.enabled ? "Disable" : "Enable"}
                </button>
                <button
                  onClick={() => handleDelete(s.id)}
                  className="rounded px-2 py-1 text-xs text-destructive hover:bg-destructive/10"
                >
                  Delete
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function McpServerForm({
  scope,
  orgId,
  onCreated,
  onCancel,
}: {
  scope: "admin" | "org" | "user";
  orgId?: string;
  onCreated: () => void;
  onCancel: () => void;
}) {
  const [name, setName] = useState("");
  const [transport, setTransport] = useState<McpTransport>("http");
  const [url, setUrl] = useState("");
  const [command, setCommand] = useState("");
  const [args, setArgs] = useState("");
  const [headerKey, setHeaderKey] = useState("");
  const [headerVal, setHeaderVal] = useState("");
  const [envKey, setEnvKey] = useState("");
  const [envVal, setEnvVal] = useState("");
  const [autoApply, setAutoApply] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [envSecrets, setEnvSecrets] = useState<SecretResponse[]>([]);

  // Fetch the caller's env-secrets so the form can offer "reference existing
  // secret" as an alternative to pasting a literal value. Env-secrets are
  // already injected into the pod environment via /sandbox-runtime/secrets-env;
  // the MCP config stores {env:VAR_NAME} and opencode resolves it at runtime.
  useEffect(() => {
    secretsApi.list().then((data) => {
      setEnvSecrets((data.secrets || []).filter((s) => s.type === "env-secret"));
    }).catch(() => { /* non-fatal: user can still type literal values */ });
  }, []);

  const pickSecretRef = (setKey: (v: string) => void, setVal: (v: string) => void) => {
    if (envSecrets.length === 0) return;
    const names = envSecrets.map((s) => s.metadata?.var_name || s.name);
    const picked = prompt("Select an env-secret to reference:\n\n" + names.map((n, i) => `${i + 1}. ${n}`).join("\n"));
    if (!picked) return;
    const idx = parseInt(picked, 10) - 1;
    if (idx >= 0 && idx < envSecrets.length) {
      const secret = envSecrets[idx];
      if (!secret) return;
      const varName = secret.metadata?.var_name || secret.name;
      setKey(varName);
      setVal(`{env:${varName}}`);
    }
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setError(null);

    const headers: Record<string, string> = {};
    if (headerKey && headerVal) headers[headerKey] = headerVal;
    const env: Record<string, string> = {};
    if (envKey && envVal) env[envKey] = envVal;

    const req: CreateMcpServerRequest = {
      name,
      transport,
      ...(transport === "stdio"
        ? { command, args: args ? args.split(/\s+/) : [] }
        : { url }),
      ...(Object.keys(headers).length > 0 ? { headers } : {}),
      ...(Object.keys(env).length > 0 ? { env } : {}),
    };
    if (autoApply) {
      req.autoApply =
        scope === "admin"
          ? { targetType: "all" }
          : scope === "org"
            ? { targetType: "org", targetId: orgId }
            : { targetType: "user" };
    }

    try {
      if (scope === "admin") await adminMcpServersApi.create(req);
      else if (scope === "org" && orgId) await orgMcpServersApi.create(orgId, req);
      else await userMcpServersApi.create(req);
      onCreated();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to create MCP server");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <form onSubmit={handleSubmit} className="space-y-3 rounded-md border border-border p-4">
      {error && (
        <div className="rounded-md border border-destructive/50 bg-destructive/10 p-2 text-sm text-destructive">
          {error}
        </div>
      )}
      <div>
        <label className="text-sm font-medium">Name</label>
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. github-tools"
          className="mt-1 w-full rounded-md border border-border px-3 py-1.5 text-sm"
          required
        />
      </div>
      <div>
        <label className="text-sm font-medium">Transport</label>
        <select
          value={transport}
          onChange={(e) => setTransport(e.target.value as McpTransport)}
          className="mt-1 w-full rounded-md border border-border px-3 py-1.5 text-sm"
        >
          <option value="http">HTTP (remote)</option>
          <option value="sse">SSE (remote)</option>
          <option value="stdio">Stdio (local)</option>
        </select>
      </div>
      {transport === "http" || transport === "sse" ? (
        <div>
          <label className="text-sm font-medium">URL</label>
          <input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://example.com/mcp"
            className="mt-1 w-full rounded-md border border-border px-3 py-1.5 text-sm"
            required
          />
        </div>
      ) : (
        <>
          <div>
            <label className="text-sm font-medium">Command</label>
            <input
              value={command}
              onChange={(e) => setCommand(e.target.value)}
              placeholder="e.g. npx"
              className="mt-1 w-full rounded-md border border-border px-3 py-1.5 text-sm"
              required
            />
          </div>
          <div>
            <label className="text-sm font-medium">Arguments</label>
            <input
              value={args}
              onChange={(e) => setArgs(e.target.value)}
              placeholder="e.g. -y @modelcontextprotocol/server-github"
              className="mt-1 w-full rounded-md border border-border px-3 py-1.5 text-sm"
            />
          </div>
        </>
      )}
      <div className="grid grid-cols-[1fr_2fr] gap-2">
        <div>
          <label className="text-sm font-medium">Header Key</label>
          <input
            value={headerKey}
            onChange={(e) => setHeaderKey(e.target.value)}
            placeholder="Authorization"
            className="mt-1 w-full rounded-md border border-border px-3 py-1.5 text-sm"
          />
        </div>
        <div>
          <label className="text-sm font-medium">Header Value</label>
          <div className="mt-1 flex gap-1">
            <input
              value={headerVal}
              onChange={(e) => setHeaderVal(e.target.value)}
              placeholder="Bearer ..."
              type="password"
              autoComplete="new-password"
              className="flex-1 rounded-md border border-border px-3 py-1.5 text-sm"
            />
            {envSecrets.length > 0 && (
              <button
                type="button"
                onClick={() => pickSecretRef(setHeaderKey, setHeaderVal)}
                className="shrink-0 rounded-md border border-border px-2 py-1 text-xs hover:bg-accent"
                title="Reference an existing env-secret"
              >
                Ref
              </button>
            )}
          </div>
        </div>
      </div>
      <div className="grid grid-cols-[1fr_2fr] gap-2">
        <div>
          <label className="text-sm font-medium">Env Key</label>
          <input
            value={envKey}
            onChange={(e) => setEnvKey(e.target.value)}
            placeholder="GITHUB_TOKEN"
            className="mt-1 w-full rounded-md border border-border px-3 py-1.5 text-sm"
          />
        </div>
        <div>
          <label className="text-sm font-medium">Env Value</label>
          <div className="mt-1 flex gap-1">
            <input
              value={envVal}
              onChange={(e) => setEnvVal(e.target.value)}
              type="password"
              autoComplete="new-password"
              className="flex-1 rounded-md border border-border px-3 py-1.5 text-sm"
            />
            {envSecrets.length > 0 && (
              <button
                type="button"
                onClick={() => pickSecretRef(setEnvKey, setEnvVal)}
                className="shrink-0 rounded-md border border-border px-2 py-1 text-xs hover:bg-accent"
                title="Reference an existing env-secret"
              >
                Ref
              </button>
            )}
          </div>
        </div>
      </div>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={autoApply} onChange={(e) => setAutoApply(e.target.checked)} />
        Auto-apply to {scope === "admin" ? "all workspaces" : scope === "org" ? "this org's workspaces" : "my workspaces"}
      </label>
      <div className="flex justify-end gap-2">
        <button type="button" onClick={onCancel} className="rounded-md px-3 py-1.5 text-sm hover:bg-accent">
          Cancel
        </button>
        <button
          type="submit"
          disabled={submitting}
          className="rounded-md bg-primary px-3 py-1.5 text-sm text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
        >
          {submitting ? "Creating..." : "Create"}
        </button>
      </div>
    </form>
  );
}
