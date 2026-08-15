import { render, screen, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import userEvent from "@testing-library/user-event";
import { OrgSSOTab } from "./OrgSSOTab";
import type { OrgResponse } from "../../api/orgs";
import type { OrgSSOConfig } from "../../api/sso";

const ORG: OrgResponse = {
  id: "org-1",
  name: "Acme",
  slug: "acme",
  createdBy: "u-1",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
  status: "active",
  planId: "team",
  subscriptionStatus: "active",
  memberCount: 2,
};

const mockOutlet = vi.fn();
const mockGetConfig = vi.fn();
const mockRemove = vi.fn();
const mockRotateToken = vi.fn();

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useOutletContext: () => mockOutlet() };
});

vi.mock("../../api/sso", () => ({
  ssoApi: {
    getConfig: (id: string) => mockGetConfig(id),
    upsert: vi.fn(),
    remove: (id: string) => mockRemove(id),
    verifyDomain: vi.fn(),
    rotateToken: (id: string) => mockRotateToken(id),
    domains: vi.fn(),
  },
}));

// A configured SSO (hasSecret: true) so the "Remove" button renders. Empty
// claimedDomains keeps the DomainVerification sub-component from rendering,
// isolating the test to the parent remove flow.
const SSO_CONFIG: OrgSSOConfig = {
  orgId: "org-1",
  discoveryUrl: "https://idp.example.com",
  clientId: "client-1",
  hasSecret: true,
  claimedDomains: [],
  verifiedDomains: [],
  verificationToken: "",
  autoProvision: true,
  groupRoleMapping: {},
  updatedAt: "2026-01-01T00:00:00Z",
};

// Variant with at least one claimed domain so the DomainVerification
// sub-component (and its rotate-token button) renders.
const SSO_CONFIG_WITH_DOMAIN: OrgSSOConfig = {
  ...SSO_CONFIG,
  claimedDomains: ["acme.com"],
  verificationToken: "existing-token",
};

function renderTab() {
  mockOutlet.mockReturnValue({ org: ORG, isAdmin: true });
  return render(<OrgSSOTab />);
}

describe("OrgSSOTab — ConfirmDialog (#814)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGetConfig.mockResolvedValue(SSO_CONFIG);
    mockRemove.mockResolvedValue(undefined);
  });

  it("opens the remove dialog and deletes the SSO config on confirm", async () => {
    const user = userEvent.setup();
    renderTab();
    await waitFor(() => expect(screen.getByText("Single Sign-On (OIDC)")).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "Remove" }));

    // ConfirmDialog opens — title visible, and a second "Remove" button (the
    // dialog's confirm) is now present. The last matching button is the dialog's.
    await waitFor(() => expect(screen.getByText("Remove SSO?")).toBeInTheDocument());
    const dialogConfirm = screen.getAllByRole("button", { name: "Remove" }).pop()!;
    await user.click(dialogConfirm);

    await waitFor(() => expect(mockRemove).toHaveBeenCalledWith("org-1"));
  });

  it("does not remove SSO when the dialog is cancelled", async () => {
    const user = userEvent.setup();
    renderTab();
    await waitFor(() => expect(screen.getByRole("button", { name: "Remove" })).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "Remove" }));
    const cancelBtn = await screen.findByRole("button", { name: "Cancel" });
    await user.click(cancelBtn);

    expect(mockRemove).not.toHaveBeenCalled();
  });

  it("rotates the verification token on confirm (#814)", async () => {
    mockGetConfig.mockResolvedValue(SSO_CONFIG_WITH_DOMAIN);
    mockRotateToken.mockResolvedValue({ verificationToken: "new-token-123" });
    const user = userEvent.setup();
    renderTab();
    await waitFor(() => expect(screen.getByText("Domain Verification")).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "Rotate Token" }));

    // ConfirmDialog opens — click "Rotate"
    await waitFor(() => expect(screen.getByText("Rotate verification token?")).toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: "Rotate" }));

    await waitFor(() => expect(mockRotateToken).toHaveBeenCalledWith("org-1"));
  });

  it("does not rotate the token when the dialog is cancelled (#814)", async () => {
    mockGetConfig.mockResolvedValue(SSO_CONFIG_WITH_DOMAIN);
    const user = userEvent.setup();
    renderTab();
    await waitFor(() => expect(screen.getByRole("button", { name: "Rotate Token" })).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "Rotate Token" }));
    const cancelBtn = await screen.findByRole("button", { name: "Cancel" });
    await user.click(cancelBtn);

    expect(mockRotateToken).not.toHaveBeenCalled();
  });
});
