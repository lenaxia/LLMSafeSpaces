import { describe, expect, it, vi } from "vitest";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { render, screen, waitFor } from "@testing-library/react";
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

function renderRoute(path: string) {
  const mem = createMemoryRouter(productionRouter.routes, {
    initialEntries: [path],
  });
  return render(<RouterProvider router={mem} />);
}

describe("production router", () => {
  it("/invitations/:token is accessible without auth", async () => {
    renderRoute("/invitations/abc123");
    await waitFor(() => {
      expect(screen.getByText("Organisation Invitation")).toBeInTheDocument();
    });
    expect(screen.getByText("Acme")).toBeInTheDocument();
  });
});
