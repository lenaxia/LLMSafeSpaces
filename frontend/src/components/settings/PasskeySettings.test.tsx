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
