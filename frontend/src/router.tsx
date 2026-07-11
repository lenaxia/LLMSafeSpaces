import { lazy, Suspense } from "react";
import { Navigate, Outlet, createBrowserRouter } from "react-router-dom";
import { useAuth } from "./providers/AuthProvider";
import { LoginPage } from "./pages/LoginPage";
import { RegisterPage } from "./pages/RegisterPage";
import { InvitationPage } from "./pages/InvitationPage";
import { ChatPage } from "./pages/ChatPage";
import { SettingsPage } from "./pages/SettingsPage";
import { NotFoundPage } from "./pages/NotFoundPage";
import { AppShell } from "./components/layout/AppShell";
import { Spinner } from "./components/ui/Spinner";
import { UserSettingsTab } from "./components/settings/UserSettingsTab";
import { UserProviderCredentialsTab } from "./components/settings/UserProviderCredentialsTab";
import { SecretsTab } from "./components/settings/SecretsTab";
import { ApiKeysTab } from "./components/settings/ApiKeysTab";
import { MyOrganisationTab } from "./components/settings/MyOrganisationTab";
import { OrgAdminLayout } from "./components/org-admin/OrgAdminLayout";
import { OrgOverviewTab } from "./components/org-admin/OrgOverviewTab";
import { OrgMembersTab } from "./components/org-admin/OrgMembersTab";
import { OrgCredentialsTab } from "./components/org-admin/OrgCredentialsTab";
import { OrgWorkspacesTab } from "./components/org-admin/OrgWorkspacesTab";
import { OrgAuditTab } from "./components/org-admin/OrgAuditTab";
import { OrgBillingTab } from "./components/org-admin/OrgBillingTab";
import { OrgSSOTab } from "./components/org-admin/OrgSSOTab";

// The /admin portal is code-split: its layout and section components are
// lazy-loaded so non-admin users (and users who never open the portal)
// never download the admin bundles. Each section is its own chunk, so an
// admin visiting /admin/users doesn't pull the relay dashboard or audit
// table. PortalLayout wraps its <Outlet> in <Suspense> for the sections;
// the route-level Suspense below covers the layout chunk itself.
const PlatformAdminLayout = lazy(() =>
  import("./components/platform-admin/PlatformAdminLayout").then((m) => ({ default: m.PlatformAdminLayout })),
);
const PlatformUsersTab = lazy(() =>
  import("./components/settings/PlatformUsersTab").then((m) => ({ default: m.PlatformUsersTab })),
);
const OrgSettingsTab = lazy(() =>
  import("./components/settings/OrgSettingsTab").then((m) => ({ default: m.OrgSettingsTab })),
);
const AdminProviderCredentialsTab = lazy(() =>
  import("./components/settings/AdminProviderCredentialsTab").then((m) => ({ default: m.AdminProviderCredentialsTab })),
);
const RelayTab = lazy(() => import("./components/settings/RelayTab").then((m) => ({ default: m.RelayTab })));
const AdminSettingsPage = lazy(() =>
  import("./pages/AdminSettingsPage").then((m) => ({ default: m.AdminSettingsPage })),
);
const PlatformAgentConfigTab = lazy(() =>
  import("./components/settings/PlatformAgentConfigTab").then((m) => ({ default: m.PlatformAgentConfigTab })),
);
const OrgAgentConfigTab = lazy(() =>
  import("./components/org-admin/OrgAgentConfigTab").then((m) => ({ default: m.OrgAgentConfigTab })),
);
const PlatformAuditTab = lazy(() =>
  import("./components/settings/PlatformAuditTab").then((m) => ({ default: m.PlatformAuditTab })),
);

const portalFallback = (
  <div className="flex h-screen items-center justify-center">
    <Spinner size="lg" />
  </div>
);

function RequireAuth() {
  const { user, loading } = useAuth();
  if (loading) return <div className="flex h-screen items-center justify-center"><Spinner size="lg" /></div>;
  if (!user) return <Navigate to="/login" replace />;
  return <Outlet />;
}

function GuestOnly() {
  const { user, loading } = useAuth();
  if (loading) return <div className="flex h-screen items-center justify-center"><Spinner size="lg" /></div>;
  if (user) return <Navigate to="/chat" replace />;
  return <Outlet />;
}

export const router = createBrowserRouter([
  {
    element: <GuestOnly />,
    children: [
      { path: "/login", element: <LoginPage /> },
      { path: "/register", element: <RegisterPage /> },
    ],
  },
  {
    element: <RequireAuth />,
    children: [
      {
        element: <AppShell />,
        children: [
          { path: "/chat", element: <ChatPage /> },
          { path: "/chat/:workspaceId", element: <ChatPage /> },
          { path: "/chat/:workspaceId/:sessionId", element: <ChatPage /> },
          {
            path: "/settings",
            element: <SettingsPage />,
            children: [
              { index: true, element: <Navigate to="preferences" replace /> },
              { path: "preferences", element: <UserSettingsTab /> },
              { path: "provider-keys", element: <UserProviderCredentialsTab /> },
              { path: "secrets", element: <SecretsTab /> },
              { path: "api-keys", element: <ApiKeysTab /> },
              { path: "my-organisation", element: <MyOrganisationTab /> },
            ],
          },
        ],
      },
      {
        path: "/orgs/:id",
        element: <OrgAdminLayout />,
        children: [
          { index: true, element: <Navigate to="overview" replace /> },
          { path: "overview", element: <OrgOverviewTab /> },
          { path: "members", element: <OrgMembersTab /> },
          { path: "credentials", element: <OrgCredentialsTab /> },
          { path: "workspaces", element: <OrgWorkspacesTab /> },
          { path: "audit", element: <OrgAuditTab /> },
          { path: "billing", element: <OrgBillingTab /> },
          { path: "sso", element: <OrgSSOTab /> },
          { path: "agent-config", element: <OrgAgentConfigTab /> },
        ],
      },
      {
        path: "/admin",
        element: (
          <Suspense fallback={portalFallback}>
            <PlatformAdminLayout />
          </Suspense>
        ),
        children: [
          { index: true, element: <Navigate to="users" replace /> },
          { path: "users", element: <PlatformUsersTab /> },
          { path: "organisations", element: <OrgSettingsTab /> },
          { path: "credentials", element: <AdminProviderCredentialsTab /> },
          { path: "relay", element: <RelayTab /> },
          { path: "settings", element: <AdminSettingsPage /> },
          { path: "agent-config", element: <PlatformAgentConfigTab /> },
          { path: "audit", element: <PlatformAuditTab /> },
        ],
      },
    ],
  },
  { path: "/", element: <Navigate to="/chat" replace /> },
  { path: "/invitations/:token", element: <InvitationPage /> },
  { path: "*", element: <NotFoundPage /> },
]);
