import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { PasskeySettings } from "./PasskeySettings";
import { passkeyApi } from "../../api/passkey";

vi.mock("../../api/passkey");
vi.mock("react-router-dom", () => ({
  useSearchParams: () => [new URLSearchParams(), vi.fn()],
}));

describe("PasskeySettings", () => {
  it("renders loading then passkeys", async () => {
    vi.mocked(passkeyApi.listPasskeys).mockResolvedValueOnce({
      passkeys: [{ id: "pk-1", name: "YubiKey", createdAt: "2026-01-01T00:00:00Z" }],
    });
    render(<PasskeySettings />);
    await waitFor(() => {
      expect(screen.getByText("YubiKey")).toBeInTheDocument();
    });
  });

  it("renders empty state when no passkeys", async () => {
    vi.mocked(passkeyApi.listPasskeys).mockResolvedValueOnce({ passkeys: [] });
    render(<PasskeySettings />);
    await waitFor(() => {
      expect(screen.getByText(/No passkeys registered/i)).toBeInTheDocument();
    });
  });
});

it("shows error when listPasskeys fails", async () => {
  vi.mocked(passkeyApi.listPasskeys).mockRejectedValueOnce(new Error("network"));
  render(<PasskeySettings />);
  await waitFor(() => {
    expect(screen.getByText(/Failed to load passkeys/i)).toBeInTheDocument();
  });
});

it("shows delete button for each passkey", async () => {
  vi.mocked(passkeyApi.listPasskeys).mockResolvedValueOnce({
    passkeys: [{ id: "pk-1", name: "YubiKey", createdAt: "2026-01-01T00:00:00Z" }],
  });
  render(<PasskeySettings />);
  await waitFor(() => {
    expect(screen.getByText("Remove")).toBeInTheDocument();
  });
});

it("calls deletePasskey when Remove clicked", async () => {
  vi.mocked(passkeyApi.listPasskeys).mockResolvedValueOnce({
    passkeys: [{ id: "pk-1", name: "YubiKey", createdAt: "2026-01-01T00:00:00Z" }],
  });
  vi.mocked(passkeyApi.deletePasskey).mockResolvedValueOnce({ deleted: true });
  vi.mocked(passkeyApi.listPasskeys).mockResolvedValueOnce({ passkeys: [] });
  const { fireEvent, waitFor } = await import("@testing-library/react");
  render(<PasskeySettings />);
  await waitFor(() => expect(screen.getByText("YubiKey")).toBeInTheDocument());
  fireEvent.click(screen.getByText("Remove"));
  await waitFor(() => expect(passkeyApi.deletePasskey).toHaveBeenCalledWith("pk-1"));
});

it("shows regenerate button and calls API", async () => {
  vi.mocked(passkeyApi.listPasskeys).mockResolvedValueOnce({ passkeys: [] });
  vi.mocked(passkeyApi.regenerateRecoveryCodes).mockResolvedValueOnce({ recoveryCodes: ["CODE1"] });
  const { fireEvent, waitFor } = await import("@testing-library/react");
  render(<PasskeySettings />);
  await waitFor(() => expect(screen.getByText("Regenerate recovery codes")).toBeInTheDocument());
  fireEvent.click(screen.getByText("Regenerate recovery codes"));
  await waitFor(() => expect(passkeyApi.regenerateRecoveryCodes).toHaveBeenCalled());
});

it("shows conflict error when deleting last passkey", async () => {
  vi.mocked(passkeyApi.listPasskeys).mockResolvedValueOnce({
    passkeys: [{ id: "pk-1", name: "Only Key", createdAt: "2026-01-01T00:00:00Z" }],
  });
  const { ApiClientError } = await import("../../api/client");
  vi.mocked(passkeyApi.deletePasskey).mockRejectedValueOnce(
    new ApiClientError(409, { error: "cannot delete your last passkey" }),
  );
  const { fireEvent, waitFor } = await import("@testing-library/react");
  render(<PasskeySettings />);
  await waitFor(() => expect(screen.getByText("Only Key")).toBeInTheDocument());
  fireEvent.click(screen.getByText("Remove"));
  await waitFor(() => {
    expect(screen.getByText(/Cannot delete your last passkey/i)).toBeInTheDocument();
  });
});
