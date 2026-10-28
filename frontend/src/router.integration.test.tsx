import { describe, expect, it, vi } from "vitest";
import { createMemoryRouter, Navigate, RouterProvider } from "react-router-dom";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { router as productionRouter } from "./router";

// Mock the auth provider so the routes are accessible without a real backend.
vi.mock("./providers/AuthProvider", () => ({
  useAuth: () => ({
    user: { id: "u-1", role: "member", email: "e@test.com" } as const,
    loading: false,
    login: vi.fn(),
    register: vi.fn(),
    logout: vi.fn(),
  }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

// Stub the invitation API so InvitationPage can load without a real server.
vi.mock("./api/orgs", () => ({
  orgsApi: {
    getInvitationByToken: () =>
      Promise.resolve({
        orgName: "Acme",
        orgSlug: "acme",
        inviterName: "admin",
        role: "member" as const,
        expiresAt: new Date(Date.now() + 86400000).toISOString(),
      }),
  },
}));

// Stub the auth API so the token-consuming pages (#1651) can load without a
// real server. (The auth-session state itself comes from the AuthProvider
// mock above — these pages never call me().)
vi.mock("./api/auth", () => ({
  authApi: {
    getConfig: () =>
      Promise.resolve({
        registrationEnabled: true,
        oidcEnabled: false,
        instanceName: "TestSpace",
      }),
    verifyEmail: () => Promise.resolve({ verified: true }),
    resendVerification: () => Promise.resolve(undefined),
    requestPasswordReset: () => Promise.resolve(undefined),
    confirmPasswordReset: () => Promise.resolve({ reset: true }),
  },
}));

function renderRoute(path: string) {
  const mem = createMemoryRouter(productionRouter.routes, {
    initialEntries: [path],
  });
  // The AppShell tree (board, chat) runs on react-query — give the
  // render a client so authenticated routes mount like production.
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={mem} />
    </QueryClientProvider>,
  );
}

describe("production router", () => {
  it("/invitations/:token is accessible without auth", async () => {
    renderRoute("/invitations/abc123");
    await waitFor(() => {
      expect(screen.getByText("Organisation Invitation")).toBeInTheDocument();
    });
    expect(screen.getByText("Acme")).toBeInTheDocument();
  });

  // #1651: emailed links target these routes — before the fix they fell
  // through to NotFoundPage, dead-ending verification + password reset.
  // The pages read the token from window.location.search (codebase
  // precedent: LoginPage/RegisterPage), so set the real URL too.
  it("/verify-email renders the verification page (not NotFound)", async () => {
    window.history.replaceState({}, "", "/verify-email?token=abc");
    renderRoute("/verify-email?token=abc");
    await waitFor(() => {
      expect(screen.getByText(/email verified/i)).toBeInTheDocument();
    });
  });

  it("/verify-email is reachable while logged in (outside GuestOnly)", async () => {
    // The AuthProvider mock above returns a logged-in user; GuestOnly would
    // bounce /verify-email to /chat if the route were inside its children.
    window.history.replaceState({}, "", "/verify-email?token=abc");
    renderRoute("/verify-email?token=abc");
    await waitFor(() => {
      expect(screen.getByText(/email verified/i)).toBeInTheDocument();
    });
  });

  it("/reset-password renders the reset request page (not NotFound)", async () => {
    window.history.replaceState({}, "", "/reset-password");
    renderRoute("/reset-password");
    await waitFor(() => {
      expect(screen.getByPlaceholderText("Email")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /send reset link/i })).toBeInTheDocument();
    });
  });

  it("/reset-password?token= renders the confirm form (not NotFound)", async () => {
    window.history.replaceState({}, "", "/reset-password?token=tok");
    renderRoute("/reset-password?token=tok");
    await waitFor(() => {
      expect(screen.getByPlaceholderText("New password")).toBeInTheDocument();
    });
  });

  // Board config relocated ON the board (owner ruling 2026-10-28): the
  // Settings tab is removed; its URL redirects to the board, carrying
  // ?config=defaults so muscle memory lands on the defaults dialog —
  // never a dead end. (Structural pin: driving the data-router
  // navigation itself trips jsdom's cross-realm AbortSignal in undici;
  // the "?config=defaults auto-opens the dialog" half of the contract
  // is pinned in BoardPage.test.tsx.)
  it("/settings/board-config redirects to the board's defaults dialog", () => {
    // The route tree nests (RequireAuth → AppShell → /settings): walk
    // it recursively for the settings branch.
    const findRoute = (routes: { path?: string; children?: unknown[] }[], path: string): { path?: string; children?: unknown[]; element?: unknown } | undefined => {
      for (const r of routes) {
        if (r.path === path) return r as { path?: string; children?: unknown[]; element?: unknown };
        if (r.children) {
          const hit = findRoute(r.children as { path?: string; children?: unknown[] }[], path);
          if (hit) return hit;
        }
      }
      return undefined;
    };
    const settings = findRoute(productionRouter.routes as { path?: string; children?: unknown[] }[], "/settings");
    const redirect = settings?.children?.find((r) => (r as { path?: string }).path === "board-config");
    expect(redirect).toBeTruthy();
    const el = (redirect as { element: { type: unknown; props: { to: string; replace: boolean } } }).element;
    expect(el.type).toBe(Navigate);
    expect(el.props.to).toBe("/board?config=defaults");
    expect(el.props.replace).toBe(true);
  });
});
