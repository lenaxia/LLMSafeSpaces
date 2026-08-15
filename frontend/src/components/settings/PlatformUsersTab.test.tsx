import { render, screen, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import userEvent from "@testing-library/user-event";
import { PlatformUsersTab } from "./PlatformUsersTab";
import type { UserListEntry } from "../../api/orgs";

const mockListUsers = vi.fn();
const mockSuspendUser = vi.fn();
const mockUnsuspendUser = vi.fn();

vi.mock("../../api/orgs", () => ({
  adminPlatformApi: {
    listUsers: (filters: unknown) => mockListUsers(filters),
    suspendUser: (id: string) => mockSuspendUser(id),
    unsuspendUser: (id: string) => mockUnsuspendUser(id),
  },
}));

const ACTIVE_USER: UserListEntry = {
  id: "user-1",
  email: "alice@example.com",
  role: "member",
  status: "active",
  createdAt: "2026-01-01T00:00:00Z",
  orgCount: 0,
};

function listResponse(items: UserListEntry[], total = items.length) {
  return {
    items,
    pagination: { total, start: 0, end: items.length, limit: 20, offset: 0 },
  };
}

describe("PlatformUsersTab — ConfirmDialog (#814)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListUsers.mockResolvedValue(listResponse([ACTIVE_USER]));
    mockSuspendUser.mockResolvedValue({});
  });

  it("opens the suspend dialog and suspends the user on confirm", async () => {
    const user = userEvent.setup();
    render(<PlatformUsersTab />);
    await waitFor(() => expect(screen.getByText("alice@example.com")).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "Suspend" }));

    // ConfirmDialog opens — title visible, and a second "Suspend" button (the
    // dialog's confirm) is now present. The last matching button is the dialog's.
    await waitFor(() => expect(screen.getByText("Suspend user?")).toBeInTheDocument());
    const dialogConfirm = screen.getAllByRole("button", { name: "Suspend" }).pop()!;
    await user.click(dialogConfirm);

    await waitFor(() => expect(mockSuspendUser).toHaveBeenCalledWith("user-1"));
  });

  it("does not suspend when the dialog is cancelled", async () => {
    const user = userEvent.setup();
    render(<PlatformUsersTab />);
    await waitFor(() => expect(screen.getByText("Suspend")).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "Suspend" }));
    const cancelBtn = await screen.findByRole("button", { name: "Cancel" });
    await user.click(cancelBtn);

    expect(mockSuspendUser).not.toHaveBeenCalled();
  });
});
