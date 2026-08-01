import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { PasskeySettings } from "./PasskeySettings";
import { passkeyApi } from "../../api/passkey";

vi.mock("../../api/passkey");
vi.mock("@simplewebauthn/browser", () => ({
  startRegistration: vi.fn().mockResolvedValue({} as never),
  browserSupportsWebAuthn: () => true,
}));
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

it("shows Add passkey button", async () => {
  vi.mocked(passkeyApi.listPasskeys).mockResolvedValueOnce({ passkeys: [] });
  render(<PasskeySettings />);
  await waitFor(() => {
    expect(screen.getByText("Add passkey")).toBeInTheDocument();
  });
});

it("shows error when enroll fails", async () => {
  vi.mocked(passkeyApi.listPasskeys).mockResolvedValueOnce({ passkeys: [] });
  vi.mocked(passkeyApi.enrollBegin).mockResolvedValueOnce({ options: {}, sessionToken: "tok" });
  const { ApiClientError } = await import("../../api/client");
  vi.mocked(passkeyApi.enrollFinish).mockRejectedValueOnce(
    new ApiClientError(500, { error: "failed" }),
  );
  const { fireEvent, waitFor } = await import("@testing-library/react");
  render(<PasskeySettings />);
  await waitFor(() => expect(screen.getByText("Add passkey")).toBeInTheDocument());
  fireEvent.click(screen.getByText("Add passkey"));
  // startRegistration will fail because the mock options don't have the
  // required fields. The error will be caught and displayed.
  await waitFor(() => {
    const errEl = screen.queryByText(/Failed to add passkey|cancelled/i);
    expect(errEl).toBeTruthy();
  }, { timeout: 5000 });
});

it("shows session-expired message on enroll failure", async () => {
  vi.mocked(passkeyApi.listPasskeys).mockResolvedValueOnce({ passkeys: [] });
  vi.mocked(passkeyApi.enrollBegin).mockResolvedValueOnce({ options: {}, sessionToken: "tok" });
  const { ApiClientError } = await import("../../api/client");
  vi.mocked(passkeyApi.enrollFinish).mockRejectedValueOnce(
    new ApiClientError(400, { error: "passkey enrollment failed" }),
  );
  const { fireEvent, waitFor } = await import("@testing-library/react");
  render(<PasskeySettings />);
  await waitFor(() => expect(screen.getByText("Add passkey")).toBeInTheDocument());
  fireEvent.click(screen.getByText("Add passkey"));
  await waitFor(() => {
    expect(screen.getByText(/session expired/i)).toBeInTheDocument();
  });
});
