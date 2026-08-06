import { NavLink, Outlet } from "react-router-dom";
import { cn } from "../lib/utils";

const allTabs = [
  { id: "preferences", label: "Preferences" },
  { id: "provider-keys", label: "Provider Keys" },
  { id: "mcp-servers", label: "MCP Servers" },
  { id: "secrets", label: "Secrets" },
  { id: "api-keys", label: "API Keys" },
  { id: "passkeys", label: "Passkeys" },
  { id: "workspace-images", label: "Workspace Images" },
  { id: "my-organisation", label: "My Organisation" },
] as const;

export function SettingsPage() {
  return (
    <div className="flex h-full flex-col md:flex-row">
      {/* Mobile: horizontal tab bar. Desktop: vertical sidebar */}
      <nav className="max-w-full overflow-x-auto border-b border-border p-2 md:border-b-0 md:border-r md:w-52 md:overflow-visible md:p-4 md:shrink-0">
        <h2 className="hidden md:block mb-4 text-sm font-semibold">Settings</h2>
        <ul className="flex gap-1 touch-manipulation md:flex-col">
          {allTabs.map((tab) => (
            <li key={tab.id} className="shrink-0">
              <NavLink
                to={`/settings/${tab.id}`}
                replace
                className={({ isActive }) =>
                  cn(
                    "whitespace-nowrap rounded-md px-3 py-1.5 text-left text-sm transition-colors w-full block",
                    isActive ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
                  )
                }
              >
                {tab.label}
              </NavLink>
            </li>
          ))}
        </ul>
      </nav>
      <div className="flex-1 min-w-0 overflow-y-auto p-4 md:p-6">
        <Outlet />
      </div>
    </div>
  );
}
