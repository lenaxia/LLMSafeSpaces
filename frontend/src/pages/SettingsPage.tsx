import { NavLink, Outlet } from "react-router-dom";
import { cn } from "../lib/utils";

const allTabs = [
  { id: "preferences", label: "Preferences" },
  { id: "provider-keys", label: "Provider Keys" },
  { id: "secrets", label: "Secrets" },
  { id: "api-keys", label: "API Keys" },
  { id: "my-organisation", label: "My Organisation" },
] as const;

export function SettingsPage() {
  return (
    <div className="flex h-full flex-col md:flex-row">
      {/* Mobile: horizontal tab bar. Desktop: vertical sidebar */}
      <nav className="border-b border-border p-2 md:border-b-0 md:border-r md:w-52 md:p-4 md:shrink-0">
        <h2 className="hidden md:block mb-4 text-sm font-semibold">Settings</h2>
        <ul className="flex gap-1 overflow-x-auto touch-manipulation md:flex-col">
          {allTabs.map((tab) => (
            <li key={tab.id}>
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
