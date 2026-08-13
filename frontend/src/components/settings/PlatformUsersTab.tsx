import { useCallback, useEffect, useState } from "react";
import {
  adminPlatformApi,
  type AdminListResponse,
  type UserListEntry,
  type UserStatus,
} from "../../api/orgs";
import { ApiClientError } from "../../api/client";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { safeConfirm } from "../../lib/safeConfirm";

const PAGE_SIZE = 20;

const STATUS_FILTERS: { value: "" | UserStatus; label: string }[] = [
  { value: "", label: "All statuses" },
  { value: "active", label: "Active" },
  { value: "suspended", label: "Suspended" },
];

function statusVariant(status: UserStatus) {
  return status === "active" ? ("success" as const) : ("destructive" as const);
}

export function PlatformUsersTab() {
  const [users, setUsers] = useState<UserListEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [statusFilter, setStatusFilter] = useState<"" | UserStatus>("");
  const [offset, setOffset] = useState(0);
  const [total, setTotal] = useState(0);
  const [busyId, setBusyId] = useState<string | null>(null);

  const fetchUsers = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const data = await adminPlatformApi.listUsers({
        limit: PAGE_SIZE,
        offset,
        status: statusFilter || undefined,
      });
      const resp = data as AdminListResponse<UserListEntry>;
      setUsers(resp.items || []);
      setTotal(resp.pagination?.total ?? resp.items.length);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load users");
    } finally {
      setLoading(false);
    }
  }, [offset, statusFilter]);

  useEffect(() => {
    fetchUsers();
  }, [fetchUsers]);

  const applyStatusFilter = (next: "" | UserStatus) => {
    setStatusFilter(next);
    setOffset(0);
  };

  const handleSuspend = async (user: UserListEntry) => {
    const prompt = user.orgCount > 0
      ? `Suspend ${user.email}? They are in org "${user.orgName ?? user.orgId}".`
      : `Suspend ${user.email}?`;
    if (!safeConfirm(prompt)) return;
    setBusyId(user.id);
    try {
      await adminPlatformApi.suspendUser(user.id);
      await fetchUsers();
    } catch (e) {
      if (e instanceof ApiClientError && e.status === 409) {
        setError(e.body.error || "Cannot suspend the last admin of an organisation.");
      } else {
        setError(e instanceof Error ? e.message : "Failed to suspend user");
      }
    } finally {
      setBusyId(null);
    }
  };

  const handleUnsuspend = async (user: UserListEntry) => {
    setBusyId(user.id);
    try {
      await adminPlatformApi.unsuspendUser(user.id);
      await fetchUsers();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to unsuspend user");
    } finally {
      setBusyId(null);
    }
  };

  const canPrev = offset > 0 && !loading;
  const canNext = offset + PAGE_SIZE < total && !loading;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-semibold">Users</h3>
        <select
          value={statusFilter}
          onChange={(e) => applyStatusFilter(e.target.value as "" | UserStatus)}
          className="h-8 rounded border border-border bg-background px-2 text-sm focus:outline-none focus:ring-1 focus:ring-ring"
        >
          {STATUS_FILTERS.map((s) => (
            <option key={s.value} value={s.value}>
              {s.label}
            </option>
          ))}
        </select>
      </div>

      <p className="text-xs text-muted-foreground">
        Platform-wide view of every user account. Suspending a user blocks them
        across all organisations and personal workspaces.
      </p>

      {error && <p className="text-xs text-red-500">{error}</p>}

      {loading ? (
        <p className="text-sm text-muted-foreground">Loading...</p>
      ) : users.length === 0 ? (
        <p className="text-xs text-muted-foreground">No users found.</p>
      ) : (
        <div className="rounded border border-border overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="border-b border-border bg-muted/50">
              <tr>
                <th className="px-3 py-2 text-left font-medium">Email</th>
                <th className="px-3 py-2 text-left font-medium">Role</th>
                <th className="px-3 py-2 text-left font-medium">Status</th>
                <th className="px-3 py-2 text-left font-medium">Organisation</th>
                <th className="px-3 py-2 text-left font-medium">Actions</th>
              </tr>
            </thead>
            <tbody>
              {users.map((user) => (
                <tr key={user.id} className="border-b border-border last:border-0">
                  <td className="px-3 py-2">
                    <div className="font-medium">{user.email}</div>
                    <div className="font-mono text-xs text-muted-foreground">{user.id.slice(0, 8)}…</div>
                  </td>
                  <td className="px-3 py-2">
                    <Badge variant={user.role === "admin" ? "default" : "muted"}>{user.role}</Badge>
                  </td>
                  <td className="px-3 py-2">
                    <Badge variant={statusVariant(user.status)}>{user.status}</Badge>
                  </td>
                  <td className="px-3 py-2 text-muted-foreground">
                    {user.orgName ? (
                      <span title={user.orgId}>{user.orgName}</span>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="px-3 py-2">
                    {user.status === "suspended" ? (
                      <button
                        onClick={() => handleUnsuspend(user)}
                        disabled={busyId === user.id}
                        className="text-xs text-accent hover:underline disabled:opacity-50"
                      >
                        Unsuspend
                      </button>
                    ) : (
                      <button
                        onClick={() => handleSuspend(user)}
                        disabled={busyId === user.id}
                        className="text-xs text-yellow-600 hover:underline disabled:opacity-50 dark:text-yellow-400"
                      >
                        Suspend
                      </button>
                    )}
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
    </div>
  );
}
