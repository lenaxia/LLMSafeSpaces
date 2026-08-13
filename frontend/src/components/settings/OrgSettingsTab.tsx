import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  adminPlatformApi,
  orgsApi,
  type AdminListResponse,
  type OrgPlan,
  type OrgStatus,
  type OrgSummary,
} from "../../api/orgs";
import { ApiClientError } from "../../api/client";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { useConfirmDialog } from "../../hooks/useConfirmDialog";

const PAGE_SIZE = 20;

const STATUS_FILTERS: { value: "" | OrgStatus; label: string }[] = [
  { value: "", label: "All statuses" },
  { value: "active", label: "Active" },
  { value: "suspended", label: "Suspended" },
  { value: "pending_activation", label: "Pending" },
];

function slugify(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/-+/g, "-") // collapse consecutive hyphens (e.g. "my - org" -> "my-org")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48);
}

function statusVariant(status: OrgStatus) {
  if (status === "active") return "success" as const;
  if (status === "suspended") return "destructive" as const;
  return "warning" as const;
}

function CreateOrgForm({
  onCreated,
  onCancel,
}: {
  onCreated: () => void;
  onCancel: () => void;
}) {
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [ownerEmail, setOwnerEmail] = useState("");
  const [planId, setPlanId] = useState<OrgPlan>("enterprise");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const handleSubmit = async () => {
    setError("");
    if (!name.trim()) {
      setError("Name is required");
      return;
    }
    if (!ownerEmail.trim()) {
      setError("Owner email is required");
      return;
    }
    setLoading(true);
    try {
      const finalSlug = slug.trim() || slugify(name);
      await orgsApi.create({
        name: name.trim(),
        slug: finalSlug,
        ownerEmail: ownerEmail.trim(),
        planId,
      });
      onCreated();
    } catch (e) {
      if (e instanceof ApiClientError && e.status === 409) {
        setError("An organisation with this slug already exists");
      } else if (e instanceof ApiClientError && e.status === 404) {
        setError("No user found with that owner email");
      } else if (e instanceof ApiClientError && e.status === 403) {
        setError("Only platform admins can create organisations");
      } else if (e instanceof ApiClientError && e.status === 400 && e.body.details) {
        // Surface per-field validation messages from the backend
        // (e.g. bad slug, malformed email) instead of the opaque top-level
        // "validation failed" string. Keys are JSON field names.
        const details = e.body.details;
        const field = Object.keys(details)[0];
        if (field) {
          const msg = details[field];
          const labels: Record<string, string> = {
            slug: "Slug",
            name: "Name",
            ownerEmail: "Owner email",
          };
          setError(`${labels[field] ?? field}: ${msg}`);
        } else {
          setError("Validation failed");
        }
      } else {
        setError(e instanceof Error ? e.message : "Failed to create organisation");
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="rounded border border-border bg-background p-4 space-y-3">
      <p className="text-sm font-medium">New Organisation</p>
      {error && <p className="text-xs text-red-500">{error}</p>}
      <div className="space-y-2">
        <input
          type="email"
          value={ownerEmail}
          onChange={(e) => setOwnerEmail(e.target.value)}
          placeholder="Owner email (must be an existing user)"
          className="h-8 w-full rounded border border-border bg-background px-2 text-sm focus:outline-none focus:ring-1 focus:ring-ring"
        />
        <input
          type="text"
          value={name}
          onChange={(e) => {
            setName(e.target.value);
            if (!slug || slug === slugify(name)) {
              setSlug(slugify(e.target.value));
            }
          }}
          placeholder="Organisation name"
          className="h-8 w-full rounded border border-border bg-background px-2 text-sm focus:outline-none focus:ring-1 focus:ring-ring"
        />
        <input
          type="text"
          value={slug}
          onChange={(e) => setSlug(e.target.value)}
          placeholder="slug (auto-generated from name)"
          className="h-8 w-full rounded border border-border bg-background px-2 text-sm focus:outline-none focus:ring-1 focus:ring-ring"
        />
        <select
          value={planId}
          onChange={(e) => setPlanId(e.target.value as OrgPlan)}
          className="h-8 w-full rounded border border-border bg-background px-2 text-sm focus:outline-none focus:ring-1 focus:ring-ring"
        >
          <option value="free">Free</option>
          <option value="team">Team</option>
          <option value="business">Business</option>
          <option value="enterprise">Enterprise</option>
        </select>
      </div>
      <div className="flex gap-2">
        <Button size="sm" disabled={loading} onClick={handleSubmit}>
          {loading ? "Creating..." : "Create"}
        </Button>
        <Button size="sm" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </div>
  );
}

export function OrgSettingsTab() {
  const [orgs, setOrgs] = useState<OrgSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [showCreate, setShowCreate] = useState(false);
  const [statusFilter, setStatusFilter] = useState<"" | OrgStatus>("");
  const [offset, setOffset] = useState(0);
  const [total, setTotal] = useState(0);
  const [busyId, setBusyId] = useState<string | null>(null);

  const { confirm: confirmAction, dialog: confirmDialog } = useConfirmDialog();

  const fetchOrgs = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const data = await adminPlatformApi.listOrgs({
        limit: PAGE_SIZE,
        offset,
        status: statusFilter || undefined,
      });
      const resp = data as AdminListResponse<OrgSummary>;
      setOrgs(resp.items || []);
      setTotal(resp.pagination?.total ?? resp.items.length);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load organisations");
    } finally {
      setLoading(false);
    }
  }, [offset, statusFilter]);

  useEffect(() => {
    fetchOrgs();
  }, [fetchOrgs]);

  const applyStatusFilter = (next: "" | OrgStatus) => {
    setStatusFilter(next);
    setOffset(0);
  };

  const handleSuspend = (orgId: string) => {
    confirmAction({
      title: "Suspend organisation?",
      description: "Suspend this organisation? All its workspaces will be suspended.",
      confirmLabel: "Suspend",
      destructive: true,
      onConfirm: async () => {
        setBusyId(orgId);
        try {
          await adminPlatformApi.suspendOrg(orgId);
          await fetchOrgs();
        } catch (e) {
          setError(e instanceof Error ? e.message : "Failed to suspend organisation");
        } finally {
          setBusyId(null);
        }
      },
    });
  };

  const handleUnsuspend = async (orgId: string) => {
    setBusyId(orgId);
    try {
      await adminPlatformApi.unsuspendOrg(orgId);
      await fetchOrgs();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to unsuspend organisation");
    } finally {
      setBusyId(null);
    }
  };

  const handleDelete = (org: OrgSummary) => {
    confirmAction({
      title: "Delete organisation?",
      description: `Delete "${org.name}"? This cannot be undone.`,
      confirmLabel: "Delete",
      destructive: true,
      onConfirm: async () => {
        setBusyId(org.id);
        try {
          await orgsApi.delete(org.id);
          await fetchOrgs();
        } catch (e) {
          setError(e instanceof Error ? e.message : "Failed to delete organisation");
        } finally {
          setBusyId(null);
        }
      },
    });
  };

  const canPrev = offset > 0 && !loading;
  const canNext = offset + PAGE_SIZE < total && !loading;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-semibold">Organisations</h3>
        <div className="flex items-center gap-2">
          <select
            value={statusFilter}
            onChange={(e) => applyStatusFilter(e.target.value as "" | OrgStatus)}
            className="h-8 rounded border border-border bg-background px-2 text-sm focus:outline-none focus:ring-1 focus:ring-ring"
          >
            {STATUS_FILTERS.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </select>
          <Button size="sm" onClick={() => setShowCreate(true)}>
            New Organisation
          </Button>
        </div>
      </div>

      <p className="text-xs text-muted-foreground">
        Platform-wide view of every organisation. Suspend to halt operations;
        workspaces are preserved and can be resumed after unsuspending.
      </p>

      {error && <p className="text-xs text-red-500">{error}</p>}

      {showCreate && (
        <CreateOrgForm
          onCreated={() => {
            setShowCreate(false);
            setOffset(0);
            fetchOrgs();
          }}
          onCancel={() => setShowCreate(false)}
        />
      )}

      {loading ? (
        <p className="text-sm text-muted-foreground">Loading...</p>
      ) : orgs.length === 0 ? (
        <p className="text-xs text-muted-foreground">No organisations found.</p>
      ) : (
        <div className="rounded border border-border overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="border-b border-border bg-muted/50">
              <tr>
                <th className="px-3 py-2 text-left font-medium">Name</th>
                <th className="px-3 py-2 text-left font-medium">Status</th>
                <th className="px-3 py-2 text-left font-medium">Plan</th>
                <th className="px-3 py-2 text-right font-medium">Members</th>
                <th className="px-3 py-2 text-right font-medium">Workspaces</th>
                <th className="px-3 py-2 text-left font-medium">Actions</th>
              </tr>
            </thead>
            <tbody>
              {orgs.map((org) => (
                <tr key={org.id} className="border-b border-border last:border-0">
                  <td className="px-3 py-2">
                    <div className="font-medium">{org.name}</div>
                    <div className="text-xs text-muted-foreground">{org.slug}</div>
                  </td>
                  <td className="px-3 py-2">
                    <Badge variant={statusVariant(org.status)}>{org.status}</Badge>
                  </td>
                  <td className="px-3 py-2 text-muted-foreground">{org.planId}</td>
                  <td className="px-3 py-2 text-right text-muted-foreground">
                    {org.memberCount}
                  </td>
                  <td className="px-3 py-2 text-right text-muted-foreground">
                    {org.workspaceCount}
                  </td>
                  <td className="px-3 py-2">
                    <div className="flex flex-wrap gap-2">
                      <Link
                        to={`/orgs/${org.id}`}
                        className="text-xs text-accent hover:underline"
                      >
                        Manage
                      </Link>
                      {org.status === "suspended" ? (
                        <button
                          onClick={() => handleUnsuspend(org.id)}
                          disabled={busyId === org.id}
                          className="text-xs text-accent hover:underline disabled:opacity-50"
                        >
                          Unsuspend
                        </button>
                      ) : (
                        <button
                          onClick={() => handleSuspend(org.id)}
                          disabled={busyId === org.id}
                          className="text-xs text-yellow-600 hover:underline disabled:opacity-50 dark:text-yellow-400"
                        >
                          Suspend
                        </button>
                      )}
                      <button
                        onClick={() => handleDelete(org)}
                        disabled={busyId === org.id}
                        className="text-xs text-red-500 hover:underline disabled:opacity-50"
                      >
                        Delete
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {!loading && total > PAGE_SIZE && (
        <div className="flex items-center gap-2">
          <Button size="sm" variant="ghost" disabled={!canPrev} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>
            Previous
          </Button>
          <span className="text-xs text-muted-foreground">
            {offset + 1}–{Math.min(offset + PAGE_SIZE, total)} of {total}
          </span>
          <Button size="sm" variant="ghost" disabled={!canNext} onClick={() => setOffset(offset + PAGE_SIZE)}>
            Next
          </Button>
        </div>
      )}
      {confirmDialog}
    </div>
  );
}
