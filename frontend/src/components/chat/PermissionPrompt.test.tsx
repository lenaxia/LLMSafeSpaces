import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { PermissionPrompt } from "./PermissionPrompt";
import type { InputRequest } from "../../api/types";

vi.mock("../../api/input", () => ({
  inputApi: {
    permissionReply: vi.fn().mockResolvedValue(undefined),
  },
}));

import { inputApi } from "../../api/input";
const mockReply = vi.mocked(inputApi.permissionReply);

const shellPermission: InputRequest = {
  id: "per_1",
  sessionId: "ses_1",
  kind: "permission",
  permission: "shell",
  patterns: ["rm -rf /workspace/node_modules"],
};

const writePermission: InputRequest = {
  id: "per_2",
  sessionId: "ses_1",
  kind: "permission",
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
    mockReply.mockReset().mockResolvedValue(undefined);
  });

  describe("whileAway variant (#1313)", () => {
    const awayPermission: InputRequest & { whileAway?: boolean } = { ...shellPermission, whileAway: true };

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

// #1365 regression set: the pill must stay clickable when the instance
// survives a completed reply, and re-clickable after an API failure.
describe("PermissionPrompt clickability (#1365)", () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it("re-enables buttons after a SUCCESSFUL reply on a surviving instance (root cause)", async () => {
    // The incident shape: onResolved's removal MISSES (store race, whileAway
    // re-presentation, fold lag) so the component stays mounted. Pre-fix,
    // submitting was only reset on error — the pill was button-dead forever.
    const onResolved = vi.fn(); // does NOT unmount the component
    render(<PermissionPrompt workspaceId="ws-1" request={shellPermission} onResolved={onResolved} />);
    const always = screen.getByRole("button", { name: /always/i });
    fireEvent.click(always);
    await waitFor(() => expect(mockReply).toHaveBeenCalledTimes(1));
    // Instance survived: buttons must be enabled again, not disabled
    await waitFor(() => expect(always).not.toBeDisabled());
    // And a second click issues a second API call (re-clickable, not dead)
    fireEvent.click(always);
    await waitFor(() => expect(mockReply).toHaveBeenCalledTimes(2));
    expect(onResolved).toHaveBeenCalledTimes(2);
  });

  it("renders the error AND re-enables buttons when the reply API rejects", async () => {
    mockReply.mockRejectedValueOnce(new Error("boom"));
    const onResolved = vi.fn();
    render(<PermissionPrompt workspaceId="ws-1" request={shellPermission} onResolved={onResolved} />);
    const once = screen.getByRole("button", { name: /once/i });
    fireEvent.click(once);
    await waitFor(() => expect(screen.getByText(/boom/i)).toBeInTheDocument());
    expect(once).not.toBeDisabled();
    // Retry works
    fireEvent.click(once);
    await waitFor(() => expect(mockReply).toHaveBeenCalledTimes(2));
  });
});
