import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { PermissionPrompt } from "./PermissionPrompt";
import type { PermissionRequest } from "../../api/types";

vi.mock("../../api/input", () => ({
  inputApi: {
    permissionReply: vi.fn().mockResolvedValue(true),
  },
}));

import { inputApi } from "../../api/input";
const mockReply = vi.mocked(inputApi.permissionReply);

const shellPermission: PermissionRequest = {
  id: "per_1",
  session_id: "ses_1",
  permission: "shell",
  patterns: ["rm -rf /workspace/node_modules"],
};

const writePermission: PermissionRequest = {
  id: "per_2",
  session_id: "ses_1",
  permission: "write",
  patterns: ["/workspace/src/main.go", "/workspace/go.mod"],
};

describe("PermissionPrompt", () => {
  const onResolved = vi.fn();

  beforeEach(() => { vi.clearAllMocks(); });

  it("displays permission type correctly", () => {
    render(<PermissionPrompt workspaceId="ws-1" request={shellPermission} onResolved={onResolved} />);
    expect(screen.getByText("Run shell command")).toBeInTheDocument();
  });

  it("displays patterns in monospace", () => {
    render(<PermissionPrompt workspaceId="ws-1" request={shellPermission} onResolved={onResolved} />);
    expect(screen.getByText("rm -rf /workspace/node_modules")).toBeInTheDocument();
  });

  it("Allow once calls API with reply:'once'", async () => {
    render(<PermissionPrompt workspaceId="ws-1" request={shellPermission} onResolved={onResolved} />);
    fireEvent.click(screen.getByText("Allow once"));
    await waitFor(() => expect(mockReply).toHaveBeenCalledWith("ws-1", "per_1", "once", undefined));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
  });

  it("Allow always calls API with reply:'always'", async () => {
    render(<PermissionPrompt workspaceId="ws-1" request={shellPermission} onResolved={onResolved} />);
    fireEvent.click(screen.getByText("Allow always"));
    await waitFor(() => expect(mockReply).toHaveBeenCalledWith("ws-1", "per_1", "always", undefined));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
  });

  it("Deny without message: first click shows feedback, second confirms", async () => {
    render(<PermissionPrompt workspaceId="ws-1" request={shellPermission} onResolved={onResolved} />);
    fireEvent.click(screen.getByText("Deny"));
    // Feedback input appears
    expect(screen.getByLabelText("Feedback")).toBeInTheDocument();
    // Confirm deny
    fireEvent.click(screen.getByText("Confirm deny"));
    await waitFor(() => expect(mockReply).toHaveBeenCalledWith("ws-1", "per_1", "reject", undefined));
  });

  it("Deny with message includes message", async () => {
    render(<PermissionPrompt workspaceId="ws-1" request={shellPermission} onResolved={onResolved} />);
    fireEvent.click(screen.getByText("Deny"));
    fireEvent.change(screen.getByLabelText("Feedback"), { target: { value: "too dangerous" } });
    fireEvent.click(screen.getByText("Confirm deny"));
    await waitFor(() => expect(mockReply).toHaveBeenCalledWith("ws-1", "per_1", "reject", "too dangerous"));
  });

  it("multiple patterns displayed", () => {
    render(<PermissionPrompt workspaceId="ws-1" request={writePermission} onResolved={onResolved} />);
    expect(screen.getByText("/workspace/src/main.go")).toBeInTheDocument();
    expect(screen.getByText("/workspace/go.mod")).toBeInTheDocument();
  });

  it("loading state disables buttons", async () => {
    mockReply.mockImplementation(() => new Promise(() => {})); // never resolves
    render(<PermissionPrompt workspaceId="ws-1" request={shellPermission} onResolved={onResolved} />);
    fireEvent.click(screen.getByText("Allow once"));
    await waitFor(() => {
      expect(screen.getByText("Allow once")).toBeDisabled();
      expect(screen.getByText("Allow always")).toBeDisabled();
    });
    // vi.clearAllMocks does not drop implementations — restore the
    // resolving default so later tests are not poisoned by the
    // never-resolving stub.
    mockReply.mockReset().mockResolvedValue(true);
  });

  describe("whileAway variant (#1313)", () => {
    const awayPermission: PermissionRequest = { ...shellPermission, whileAway: true };

    it("renders the while-you-were-away title and the guidance hint", () => {
      render(<PermissionPrompt workspaceId="ws-1" request={awayPermission} onResolved={onResolved} />);
      expect(screen.getByText(/while you were away/i)).toBeInTheDocument();
      expect(screen.getByText(/already ended without permission/i)).toBeInTheDocument();
    });

    it("deny still routes through permissionReply (late decision is server-side)", async () => {
      render(<PermissionPrompt workspaceId="ws-1" request={awayPermission} onResolved={onResolved} />);
      fireEvent.click(screen.getByText("Deny"));
      await waitFor(() => expect(screen.getByText("Confirm deny")).toBeInTheDocument());
      fireEvent.click(screen.getByText("Confirm deny"));
      await waitFor(() => expect(mockReply).toHaveBeenCalledWith("ws-1", "per_1", "reject", undefined));
      await waitFor(() => expect(onResolved).toHaveBeenCalled());
    });

    it("live prompt shows no whileAway chrome", () => {
      render(<PermissionPrompt workspaceId="ws-1" request={shellPermission} onResolved={onResolved} />);
      expect(screen.queryByText(/while you were away/i)).not.toBeInTheDocument();
      expect(screen.queryByText(/already ended without permission/i)).not.toBeInTheDocument();
    });
  });
});
