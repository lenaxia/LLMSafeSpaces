import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter, Navigate, Route, Routes } from "react-router-dom";
import { ThemeProvider } from "../providers/ThemeProvider";
import { ToastProvider } from "../providers/ToastProvider";
import { SettingsPage } from "./SettingsPage";
import { UserSettingsTab } from "../components/settings/UserSettingsTab";
import { UserProviderCredentialsTab } from "../components/settings/UserProviderCredentialsTab";
import { SecretsTab } from "../components/settings/SecretsTab";
import { ApiKeysTab } from "../components/settings/ApiKeysTab";
import { MyOrganisationTab } from "../components/settings/MyOrganisationTab";

vi.mock("../providers/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "1", role: "admin" }, loading: false }),
}));

vi.mock("../api/settings", () => ({
  settingsApi: {
    getUserSettings: () => Promise.resolve({ settings: {}, schemaVersion: 1 }),
    getUserSchema: () => Promise.resolve({ settings: [], schemaVersion: 1 }),
    getAdminSettings: () => Promise.resolve({ settings: { debug: false }, schemaVersion: 1 }),
    getAdminSchema: () => Promise.resolve({ settings: [], schemaVersion: 1 }),
    setUserSetting: vi.fn().mockResolvedValue({}),
    setAdminSetting: vi.fn().mockResolvedValue({}),
  },
}));

vi.mock("../api/providerCredentials", () => ({
  adminProviderCredentialsApi: { list: () => Promise.resolve([]) },
  userProviderCredentialsApi: { list: () => Promise.resolve([]) },
}));

function renderSettingsRoute(initialPath = "/settings/preferences") {
  return render(
    <ThemeProvider>
      <ToastProvider>
        <MemoryRouter initialEntries={[initialPath]}>
          <Routes>
            <Route path="/settings" element={<SettingsPage />}>
              <Route index element={<Navigate to="preferences" replace />} />
              <Route path="preferences" element={<UserSettingsTab />} />
              <Route path="provider-keys" element={<UserProviderCredentialsTab />} />
              <Route path="secrets" element={<SecretsTab />} />
              <Route path="api-keys" element={<ApiKeysTab />} />
              <Route path="my-organisation" element={<MyOrganisationTab />} />
            </Route>
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </ThemeProvider>,
  );
}

describe("SettingsPage", () => {
  it("renders settings heading", () => {
    renderSettingsRoute();
    expect(screen.getByText("Settings")).toBeInTheDocument();
  });

  it("renders personal tabs and not platform-admin tabs", () => {
    renderSettingsRoute();
    expect(screen.getByText("Preferences")).toBeInTheDocument();
    expect(screen.getByText("Provider Keys")).toBeInTheDocument();
    expect(screen.getByText("Secrets")).toBeInTheDocument();
    expect(screen.getByText("API Keys")).toBeInTheDocument();
    expect(screen.getByText("My Organisation")).toBeInTheDocument();
    // Platform-admin tabs migrated to the /admin portal
    expect(screen.queryByText("Platform Credentials")).not.toBeInTheDocument();
    expect(screen.queryByText("Platform Audit")).not.toBeInTheDocument();
    expect(screen.queryByText("Admin")).not.toBeInTheDocument();
  });

  it("redirects /settings to /settings/preferences", async () => {
    renderSettingsRoute("/settings");
    // After <Navigate to="preferences" replace />, the Preferences tab
    // should be active. Verify via bg-accent class on the NavLink.
    await waitFor(() => {
      const prefsLink = screen.getByText("Preferences");
      expect(prefsLink).toBeInTheDocument();
      expect(prefsLink.className).toContain("bg-accent");
    });
    expect(screen.getByText("Settings")).toBeInTheDocument();
  });

  it("navigates to API Keys tab on click", async () => {
    const user = userEvent.setup();
    renderSettingsRoute();
    await user.click(screen.getByText("API Keys"));
    expect(screen.getByText(/no api keys yet/i)).toBeInTheDocument();
  });

  it("content area has min-w-0 to allow proper shrinking on narrow screens", () => {
    const { container } = renderSettingsRoute();
    const contentArea = container.querySelector(".flex-1.min-w-0.overflow-y-auto");
    expect(contentArea).not.toBeNull();
  });
});
