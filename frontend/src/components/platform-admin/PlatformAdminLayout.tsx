import { Link } from "react-router-dom";
import { PortalLayout, type NavItem } from "../layout/PortalLayout";
import { useAuth } from "../../providers/AuthProvider";

const NAV_ITEMS: NavItem[] = [
  { to: "users", label: "Users" },
  { to: "organisations", label: "Organisations" },
  { to: "credentials", label: "Credentials" },
  { to: "relay", label: "Relay" },
  { to: "settings", label: "Settings" },
  { to: "versions", label: "Versions" },
  { to: "agent-config", label: "Agent Config" },
  { to: "audit", label: "Audit" },
];

export function PlatformAdminLayout() {
  const { user } = useAuth();

  if (!user || user.role !== "admin") {
    return (
      <div className="flex h-screen flex-col items-center justify-center gap-4">
        <p className="text-sm text-red-500">Platform administrator access required.</p>
        <Link to="/chat" className="text-sm text-accent hover:underline">
          ← Back to Chat
        </Link>
      </div>
    );
  }

  return (
    <PortalLayout
      title="Platform Administration"
      backLink="/chat"
      navItems={NAV_ITEMS}
      context={{ isAdmin: true }}
    />
  );
}
