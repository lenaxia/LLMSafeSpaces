import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { InvitationPage } from "./InvitationPage";
import type { InvitationDetail, OrgMember } from "../api/orgs";

// --- Mocks ---
const mockGetInvitationByToken = vi.fn();
const mockAcceptInvitation = vi.fn();
const mockDeclineInvitation = vi.fn();

vi.mock("../api/orgs", () => ({
  orgsApi: {
    getInvitationByToken: (token: string) => mockGetInvitationByToken(token),
    acceptInvitation: (token: string) => mockAcceptInvitation(token),
    declineInvitation: (token: string) => mockDeclineInvitation(token),
  },
}));

vi.mock("../providers/AuthProvider", () => ({
  useAuth: () => ({
    user: { id: "u-1", role: "member", email: "invitee@example.com" },
    loading: false,
    login: vi.fn(),
    register: vi.fn(),
    logout: vi.fn(),
  }),
}));

// --- Fixtures ---
const INVITATION_DETAIL: InvitationDetail = {
  orgName: "Acme Corp",
  orgSlug: "acme-corp",
  inviterName: "admin",
  role: "member",
  expiresAt: new Date(Date.now() + 7 * 86400000).toISOString(),
};

const MEMBERSHIP: OrgMember = {
  orgId: "org-1",
  userId: "u-1",
  username: "invitee",
  email: "invitee@example.com",
  role: "member",
  emailVerified: true,
  createdAt: new Date().toISOString(),
};

// --- Helpers ---
function renderInvitationPage(token = "abc123") {
  return render(
    <MemoryRouter initialEntries={[`/invitations/${token}`]}>
      <Routes>
        <Route path="/invitations/:token" element={<InvitationPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

// React Router wraps state updates in startTransition starting v7.
// The warning is harmless — suppress it to keep test output clean.
let realWarn: typeof console.warn;
beforeEach(() => {
  realWarn = console.warn;
  console.warn = (...args: unknown[]) => {
    const msg = typeof args[0] === "string" ? args[0] : "";
    if (msg.includes("startTransition")) return;
    realWarn(...args);
  };
});

afterEach(() => {
  console.warn = realWarn;
  vi.restoreAllMocks();
});

describe("InvitationPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGetInvitationByToken.mockResolvedValue(INVITATION_DETAIL);
    mockAcceptInvitation.mockResolvedValue({ membership: MEMBERSHIP });
    mockDeclineInvitation.mockResolvedValue({ status: "declined" });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  // --- Happy paths ---

  it("shows spinner while loading", () => {
    // Never resolve — stay in loading state.
    mockGetInvitationByToken.mockReturnValue(new Promise(() => {}));
    const { container } = renderInvitationPage();
    // Spinner renders an SVG with role=status (or an aria-label).
    // Check that there's no text content yet.
    expect(screen.queryByText("Organisation Invitation")).not.toBeInTheDocument();
    // The spinner should be present. Spinner component renders an SVG.
    const svg = container.querySelector("svg");
    expect(svg).not.toBeNull();
  });

  it("renders invitation details after loading", async () => {
    renderInvitationPage();
    await waitFor(() => {
      expect(screen.getByText("Organisation Invitation")).toBeInTheDocument();
    });
    expect(screen.getByText("Acme Corp")).toBeInTheDocument();
    expect(screen.getByText("admin")).toBeInTheDocument();
    expect(screen.getByText("Member")).toBeInTheDocument();
  });

  it("accepts invitation successfully", async () => {
    const user = userEvent.setup();
    renderInvitationPage();
    await waitFor(() => {
      expect(screen.getByText("Accept")).toBeInTheDocument();
    });
    await user.click(screen.getByText("Accept"));
    await waitFor(() => {
      expect(screen.getByText("Invitation accepted!")).toBeInTheDocument();
    });
    expect(screen.getByText(/Acme Corp/)).toBeInTheDocument();
    expect(mockAcceptInvitation).toHaveBeenCalledWith("abc123");
  });

  it("declines invitation successfully", async () => {
    const user = userEvent.setup();
    renderInvitationPage();
    await waitFor(() => {
      expect(screen.getByText("Decline")).toBeInTheDocument();
    });
    await user.click(screen.getByText("Decline"));
    await waitFor(() => {
      expect(screen.getByText("Invitation declined")).toBeInTheDocument();
    });
    expect(mockDeclineInvitation).toHaveBeenCalledWith("abc123");
  });

  // --- Unhappy paths ---

  it("shows 404 error when invitation not found", async () => {
    const { ApiClientError } = await import("../api/client");
    mockGetInvitationByToken.mockRejectedValue(
      new ApiClientError(404, { error: "not found" }),
    );
    renderInvitationPage();
    await waitFor(() => {
      expect(
        screen.getByText("Invitation not found or has expired."),
      ).toBeInTheDocument();
    });
  });

  it("shows 410 error on expired invitation accept", async () => {
    const { ApiClientError } = await import("../api/client");
    mockAcceptInvitation.mockRejectedValue(
      new ApiClientError(410, { error: "expired" }),
    );
    const user = userEvent.setup();
    renderInvitationPage();
    await waitFor(() => {
      expect(screen.getByText("Accept")).toBeInTheDocument();
    });
    await user.click(screen.getByText("Accept"));
    await waitFor(() => {
      expect(
        screen.getByText("This invitation has expired."),
      ).toBeInTheDocument();
    });
  });

  it("shows 409 already-handled on accept", async () => {
    const { ApiClientError } = await import("../api/client");
    mockAcceptInvitation.mockRejectedValue(
      new ApiClientError(409, {
        error: "You have already accepted this invitation.",
      }),
    );
    const user = userEvent.setup();
    renderInvitationPage();
    await waitFor(() => {
      expect(screen.getByText("Accept")).toBeInTheDocument();
    });
    await user.click(screen.getByText("Accept"));
    await waitFor(() => {
      expect(
        screen.getByText("You have already accepted this invitation."),
      ).toBeInTheDocument();
    });
  });

  it("shows inline 403 error and keeps Accept/Decline visible", async () => {
    const { ApiClientError } = await import("../api/client");
    mockAcceptInvitation.mockRejectedValue(
      new ApiClientError(403, { error: "wrong email" }),
    );
    const user = userEvent.setup();
    renderInvitationPage();
    await waitFor(() => {
      expect(screen.getByText("Accept")).toBeInTheDocument();
    });
    await user.click(screen.getByText("Accept"));
    await waitFor(() => {
      expect(
        screen.getByText(/This invitation was sent to a different email/),
      ).toBeInTheDocument();
    });
    // Buttons should still be present (state is back to detail).
    expect(screen.getByText("Accept")).toBeInTheDocument();
    expect(screen.getByText("Decline")).toBeInTheDocument();
  });

  it("shows generic error on network failure during accept", async () => {
    mockAcceptInvitation.mockRejectedValue(new Error("network error"));
    const user = userEvent.setup();
    renderInvitationPage();
    await waitFor(() => {
      expect(screen.getByText("Accept")).toBeInTheDocument();
    });
    await user.click(screen.getByText("Accept"));
    await waitFor(() => {
      expect(
        screen.getByText("Something went wrong. Please try again."),
      ).toBeInTheDocument();
    });
  });

  it("shows inline error on decline failure", async () => {
    const { ApiClientError } = await import("../api/client");
    mockDeclineInvitation.mockRejectedValue(
      new ApiClientError(500, { error: "internal" }),
    );
    const user = userEvent.setup();
    renderInvitationPage();
    await waitFor(() => {
      expect(screen.getByText("Decline")).toBeInTheDocument();
    });
    await user.click(screen.getByText("Decline"));
    await waitFor(() => {
      expect(screen.getByText("internal")).toBeInTheDocument();
    });
  });

  // --- Edge cases ---

  it("shows error when token is missing from URL params", async () => {
    render(
      <MemoryRouter initialEntries={["/invitations/"]}>
        <Routes>
          <Route path="/invitations/:token?" element={<InvitationPage />} />
        </Routes>
      </MemoryRouter>,
    );
    await waitFor(() => {
      expect(
        screen.getByText("Missing invitation token."),
      ).toBeInTheDocument();
    });
  });

  it("Go to chat link is present on error and terminal states", async () => {
    const { ApiClientError } = await import("../api/client");
    mockGetInvitationByToken.mockRejectedValue(
      new ApiClientError(404, { error: "not found" }),
    );
    renderInvitationPage();
    await waitFor(() => {
      expect(screen.getByText("Go to chat")).toBeInTheDocument();
    });
  });

  it("disables buttons while accepting/declining", async () => {
    // Never resolve — keep it in accepting state.
    mockAcceptInvitation.mockReturnValue(new Promise(() => {}));
    const user = userEvent.setup();
    renderInvitationPage();
    await waitFor(() => {
      expect(screen.getByText("Accept")).toBeInTheDocument();
    });
    await user.click(screen.getByText("Accept"));
    await waitFor(() => {
      const acceptBtn = screen.getByText("Accept");
      expect(acceptBtn).toBeDisabled();
    });
  });
});
