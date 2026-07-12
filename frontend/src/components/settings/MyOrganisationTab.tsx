import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { orgsApi, type OrgResponse } from "../../api/orgs";
import { Spinner } from "../ui/Spinner";

export function MyOrganisationTab() {
  const [org, setOrg] = useState<OrgResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const fetchOrg = useCallback(async () => {
    try {
      const orgs = await orgsApi.list();
      setOrg(orgs && orgs.length > 0 ? orgs[0]! : null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load organisation");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchOrg();
  }, [fetchOrg]);

  if (loading) {
    return (
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Spinner size="sm" /> Loading...
      </div>
    );
  }

  if (error) {
    return <p className="text-xs text-red-500">{error}</p>;
  }

  if (!org) {
    return (
      <div className="space-y-4">
        <h3 className="text-sm font-semibold">My Organisation</h3>
        <p className="text-xs text-muted-foreground">
          You are not a member of any organisation.
        </p>
        <p className="text-xs text-muted-foreground">
          If you have been invited to join an organisation, check your
          email for the invitation link, or ask your organisation
          administrator to resend it.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <h3 className="text-sm font-semibold">My Organisation</h3>
      <div className="rounded border border-border p-4 space-y-2">
        <div className="flex items-center justify-between">
          <span className="text-sm font-medium">{org.name}</span>
          <span className="text-xs text-muted-foreground">{org.slug}</span>
        </div>
        <div className="text-xs text-muted-foreground">
          <span className="rounded-full bg-accent px-2 py-0.5">{org.userRole}</span>
          <span className="ml-2">{org.memberCount} member{org.memberCount !== 1 ? "s" : ""}</span>
        </div>
        <div className="text-xs text-muted-foreground">
          Plan: <span className="font-medium">{org.planId}</span>
          {" · "}
          Status: <span className="font-medium">{org.status}</span>
        </div>
      </div>
      {org.userRole === "admin" && (
        <Link
          to={`/orgs/${org.id}`}
          className="text-xs text-accent hover:underline"
        >
          Manage organisation →
        </Link>
      )}
    </div>
  );
}
