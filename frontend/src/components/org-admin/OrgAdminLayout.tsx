import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { orgsApi, type OrgResponse } from "../../api/orgs";
import { ApiClientError } from "../../api/client";
import { Badge } from "../ui/Badge";
import { Spinner } from "../ui/Spinner";
import { PortalLayout } from "../layout/PortalLayout";

export function OrgAdminLayout() {
  const { id } = useParams<{ id: string }>();
  const [org, setOrg] = useState<OrgResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!id) return;
    setLoading(true);
    setError("");
    orgsApi
      .get(id)
      .then((data) => setOrg(data))
      .catch((e) => {
        if (e instanceof ApiClientError && (e.status === 403 || e.status === 404)) {
          setError("You do not have access to this organization.");
        } else {
          setError(e instanceof Error ? e.message : "Failed to load organization");
        }
      })
      .finally(() => setLoading(false));
  }, [id]);

  if (loading)
    return (
      <div className="flex h-screen items-center justify-center">
        <Spinner size="lg" />
      </div>
    );

  if (error || !org)
    return (
      <div className="flex h-screen flex-col items-center justify-center gap-4">
        <p className="text-sm text-red-500">{error || "Organization not found"}</p>
        <Link to="/chat" className="text-sm text-accent hover:underline">
          ← Back to Chat
        </Link>
      </div>
    );

  const isAdmin = org.userRole === "admin";

  const navItems = [
    { to: "overview", label: "Overview", adminOnly: false },
    { to: "members", label: "Members", adminOnly: true },
    { to: "credentials", label: "Credentials", adminOnly: true },
    { to: "mcp-servers", label: "MCP Servers", adminOnly: true },
    { to: "images", label: "Image Factory", adminOnly: true },
    { to: "workspaces", label: "Workspaces", adminOnly: false },
    { to: "audit", label: "Audit", adminOnly: true },
    { to: "billing", label: "Billing", adminOnly: true },
    { to: "sso", label: "SSO", adminOnly: true },
    { to: "agent-config", label: "Agent Config", adminOnly: true },
    { to: "settings", label: "Settings", adminOnly: true },
  ]
    .filter((item) => !item.adminOnly || isAdmin)
    .map(({ to, label }) => ({ to, label }));

  const badges = (
    <>
      {org.status === "pending_activation" && (
        <Badge variant="warning">Pending Activation</Badge>
      )}
      {org.status === "suspended" && (
        <Badge variant="destructive">Suspended</Badge>
      )}
      <Badge variant="default">{org.planId}</Badge>
    </>
  );

  const meta = (
    <span>
      {org.memberCount} member{org.memberCount !== 1 ? "s" : ""}
    </span>
  );

  return (
    <PortalLayout
      title={org.name}
      backLink="/chat"
      badges={badges}
      meta={meta}
      navItems={navItems}
      context={{ org, isAdmin }}
    />
  );
}
