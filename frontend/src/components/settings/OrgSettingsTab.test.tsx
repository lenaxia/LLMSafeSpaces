import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { OrgSettingsTab } from "./OrgSettingsTab";
import type { OrgSummary } from "../../api/orgs";

const mockListOrgs = vi.fn();
const mockCreate = vi.fn();
const mockSuspendOrg = vi.fn();
const mockUnsuspendOrg = vi.fn();

vi.mock("../../api/orgs", () => ({
  orgsApi: {
    create: (req: unknown) => mockCreate(req),
    delete: vi.fn(),
  },
  adminPlatformApi: {
    listOrgs: (filters: unknown) => mockListOrgs(filters),
    suspendOrg: (id: string) => mockSuspendOrg(id),
    unsuspendOrg: (id: string) => mockUnsuspendOrg(id),
  },
}));

vi.mock("../../providers/AuthProvider", () => ({
  useAuth: () => ({ user: { role: "admin" }, loading: false }),
}));

import { orgsApi } from "../../api/orgs";

const ORG_ACTIVE: OrgSummary = {
  id: "org-1",
  name: "Acme",
  slug: "acme",
  createdBy: "admin-1",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
  status: "active",
  planId: "enterprise",
  subscriptionStatus: "active",
  memberCount: 3,
  workspaceCount: 5,
};

const ORG_SUSPENDED: OrgSummary = {
  ...ORG_ACTIVE,
  id: "org-2",
  name: "Globex",
  slug: "globex",
  status: "suspended",
  planId: "team",
  memberCount: 1,
  workspaceCount: 2,
};

function listResponse(items: OrgSummary[], total = items.length) {
  return {
    items,
    pagination: { total, start: 0, end: items.length, limit: 20, offset: 0 },
  };
}

function renderTab() {
  return render(
    <MemoryRouter>
      <OrgSettingsTab />
    </MemoryRouter>,
  );
}

describe("OrgSettingsTab", () => {
  beforeEach(() => {
    mockListOrgs.mockReset();
    mockCreate.mockReset();
    mockSuspendOrg.mockReset();
    mockUnsuspendOrg.mockReset();
    (orgsApi.delete as ReturnType<typeof vi.fn>).mockReset();
  });

  it("lists organisations with member + workspace counts", async () => {
    mockListOrgs.mockResolvedValue(listResponse([ORG_ACTIVE, ORG_SUSPENDED]));
    renderTab();
    await waitFor(() => expect(screen.getByText("Acme")).toBeInTheDocument());
    expect(screen.getByText("Globex")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    expect(screen.getByText("5")).toBeInTheDocument();
  });

  it("renders status badges for each org", async () => {
    mockListOrgs.mockResolvedValue(listResponse([ORG_ACTIVE, ORG_SUSPENDED]));
    renderTab();
    await waitFor(() => expect(screen.getByText("Acme")).toBeInTheDocument());
    expect(screen.getByText("active")).toBeInTheDocument();
    expect(screen.getByText("suspended")).toBeInTheDocument();
  });

  it("shows a Suspend action for active orgs and Unsuspend for suspended", async () => {
    mockListOrgs.mockResolvedValue(listResponse([ORG_ACTIVE, ORG_SUSPENDED]));
    renderTab();
    await waitFor(() => expect(screen.getByText("Acme")).toBeInTheDocument());
    const suspendButtons = screen.getAllByText("Suspend");
    const unsuspendButtons = screen.getAllByText("Unsuspend");
    expect(suspendButtons).toHaveLength(1);
    expect(unsuspendButtons).toHaveLength(1);
  });

  it("calls suspendOrg then refreshes on confirm (#814)", async () => {
    mockListOrgs
      .mockResolvedValueOnce(listResponse([ORG_ACTIVE]))
      .mockResolvedValueOnce(
        listResponse([{ ...ORG_ACTIVE, status: "suspended" }]),
      );
    mockSuspendOrg.mockResolvedValue({ status: "suspended" });
    const user = userEvent.setup();
    renderTab();
    await waitFor(() => expect(screen.getByText("Suspend")).toBeInTheDocument());
    await user.click(screen.getByText("Suspend"));

    // ConfirmDialog opens — wait for dialog title, then click confirm
    await waitFor(() => {
      expect(screen.getByText("Suspend organisation?")).toBeInTheDocument();
    });
    const dialogBtn = screen.getAllByRole("button", { name: "Suspend" }).pop()!;
    await user.click(dialogBtn);

    await waitFor(() => expect(mockSuspendOrg).toHaveBeenCalledWith("org-1"));
    await waitFor(() => expect(mockListOrgs).toHaveBeenCalledTimes(2));
  });

  it("does not call suspendOrg when the confirm is cancelled (#814)", async () => {
    mockListOrgs.mockResolvedValue(listResponse([ORG_ACTIVE]));
    const user = userEvent.setup();
    renderTab();
    await waitFor(() => expect(screen.getByText("Suspend")).toBeInTheDocument());
    await user.click(screen.getByText("Suspend"));

    // ConfirmDialog opens — click Cancel
    const cancelBtn = await screen.findByRole("button", { name: "Cancel" });
    await user.click(cancelBtn);

    expect(mockSuspendOrg).not.toHaveBeenCalled();
  });

  it("calls orgsApi.delete when the confirm dialog is confirmed (#814)", async () => {
    mockListOrgs
      .mockResolvedValueOnce(listResponse([ORG_ACTIVE]))
      .mockResolvedValueOnce(listResponse([]));
    (orgsApi.delete as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderTab();
    await waitFor(() => expect(screen.getByText("Acme")).toBeInTheDocument());
    await user.click(screen.getByText("Delete"));

    // ConfirmDialog opens — click the dialog's "Delete" button (the last one,
    // since the table row still shows its own Delete button).
    await waitFor(() => expect(screen.getByText("Delete organisation?")).toBeInTheDocument());
    const dialogConfirm = screen.getAllByRole("button", { name: "Delete" }).pop()!;
    await user.click(dialogConfirm);

    await waitFor(() => expect(orgsApi.delete).toHaveBeenCalledWith("org-1"));
  });

  it("does not call orgsApi.delete when the confirm is cancelled (#814)", async () => {
    mockListOrgs.mockResolvedValue(listResponse([ORG_ACTIVE]));
    const user = userEvent.setup();
    renderTab();
    await waitFor(() => expect(screen.getByText("Acme")).toBeInTheDocument());
    await user.click(screen.getByText("Delete"));

    const cancelBtn = await screen.findByRole("button", { name: "Cancel" });
    await user.click(cancelBtn);

    expect(orgsApi.delete).not.toHaveBeenCalled();
  });

  it("calls unsuspendOrg for a suspended org", async () => {
    mockListOrgs
      .mockResolvedValueOnce(listResponse([ORG_SUSPENDED]))
      .mockResolvedValueOnce(listResponse([{ ...ORG_SUSPENDED, status: "active" }]));
    mockUnsuspendOrg.mockResolvedValue({ status: "active" });
    const user = userEvent.setup();
    renderTab();
    await waitFor(() => expect(screen.getByText("Unsuspend")).toBeInTheDocument());
    await user.click(screen.getByText("Unsuspend"));
    await waitFor(() => expect(mockUnsuspendOrg).toHaveBeenCalledWith("org-2"));
  });

  it("opens the create form and posts owner email + plan", async () => {
    mockListOrgs.mockResolvedValue(listResponse([]));
    mockCreate.mockResolvedValue({});
    const user = userEvent.setup();
    renderTab();
    await waitFor(() =>
      expect(screen.getByText("New Organisation")).toBeInTheDocument(),
    );
    await user.click(screen.getByText("New Organisation"));
    await user.type(
      screen.getByPlaceholderText(/owner email/i),
      "owner@example.com",
    );
    await user.type(
      screen.getByPlaceholderText(/organisation name/i),
      "Acme",
    );
    await user.click(screen.getByText("Create"));
    await waitFor(() => expect(mockCreate).toHaveBeenCalledTimes(1));
    const req = mockCreate.mock.calls[0]![0] as {
      name: string;
      slug: string;
      ownerEmail: string;
      planId: string;
    };
    expect(req.ownerEmail).toBe("owner@example.com");
    expect(req.name).toBe("Acme");
    expect(req.slug).toBeTruthy();
    expect(req.planId).toBe("enterprise");
  });

  // slugify() must produce slugs that the backend's "slug" validator accepts:
  // ^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$ (no consecutive hyphens). Names that mix
  // spaces and hyphens (e.g. "My - Org") would previously have generated
  // "my---org", which the backend rejects.
  it("slugify collapses consecutive hyphens so the generated slug is backend-valid", async () => {
    mockListOrgs.mockResolvedValue(listResponse([]));
    mockCreate.mockResolvedValue({});
    const user = userEvent.setup();
    renderTab();
    await waitFor(() =>
      expect(screen.getByText("New Organisation")).toBeInTheDocument(),
    );
    await user.click(screen.getByText("New Organisation"));
    await user.type(
      screen.getByPlaceholderText(/owner email/i),
      "owner@example.com",
    );
    await user.type(
      screen.getByPlaceholderText(/organisation name/i),
      "My - Org",
    );
    await user.click(screen.getByText("Create"));
    await waitFor(() => expect(mockCreate).toHaveBeenCalledTimes(1));
    const req = mockCreate.mock.calls[0]![0] as { slug: string };
    expect(req.slug).toBe("my-org");
    // Defence-in-depth: assert the slug satisfies the backend's regex too,
    // so any future tightening of either rule re-trips this test.
    expect(req.slug).toMatch(/^[a-z0-9]+(-[a-z0-9]+)*$/);
  });

  it("surfaces a 'no user' message when the backend returns 404", async () => {
    mockListOrgs.mockResolvedValue(listResponse([]));
    const { ApiClientError } = await import("../../api/client");
    mockCreate.mockRejectedValue(new ApiClientError(404, { error: "owner not found" }));
    const user = userEvent.setup();
    renderTab();
    await waitFor(() =>
      expect(screen.getByText("New Organisation")).toBeInTheDocument(),
    );
    await user.click(screen.getByText("New Organisation"));
    await user.type(
      screen.getByPlaceholderText(/owner email/i),
      "nobody@example.com",
    );
    await user.type(
      screen.getByPlaceholderText(/organisation name/i),
      "Acme",
    );
    await user.click(screen.getByText("Create"));
    await waitFor(() =>
      expect(screen.getByText(/no user found with that owner email/i)).toBeInTheDocument(),
    );
  });

  it("forwards the status filter to the list call", async () => {
    mockListOrgs.mockResolvedValue(listResponse([]));
    const user = userEvent.setup();
    renderTab();
    await waitFor(() => expect(mockListOrgs).toHaveBeenCalled());
    await user.selectOptions(screen.getByDisplayValue("All statuses"), "suspended");
    await waitFor(() => {
      const calls = mockListOrgs.mock.calls;
      const lastCall = calls[calls.length - 1]![0] as { status?: string };
      expect(lastCall.status).toBe("suspended");
    });
  });

  it("surfaces backend per-field validation details on 400 with a friendly label", async () => {
    mockListOrgs.mockResolvedValue(listResponse([]));
    const { ApiClientError } = await import("../../api/client");
    mockCreate.mockRejectedValue(
      new ApiClientError(400, {
        error: "validation failed",
        details: {
          slug: "Must be letters, digits, and single hyphens between segments (e.g. \"my-org\")",
        },
      }),
    );
    const user = userEvent.setup();
    renderTab();
    await waitFor(() =>
      expect(screen.getByText("New Organisation")).toBeInTheDocument(),
    );
    await user.click(screen.getByText("New Organisation"));
    await user.type(
      screen.getByPlaceholderText(/owner email/i),
      "owner@example.com",
    );
    await user.type(
      screen.getByPlaceholderText(/organisation name/i),
      "Acme",
    );
    await user.click(screen.getByText("Create"));
    await waitFor(() =>
      expect(screen.getByText(/^Slug:.*letters, digits/i)).toBeInTheDocument(),
    );
  });
});
