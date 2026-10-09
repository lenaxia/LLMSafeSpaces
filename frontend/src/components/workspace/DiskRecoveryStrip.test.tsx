import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { DiskRecoveryStrip } from "./DiskRecoveryStrip";
import { workspacesApi } from "../../api/workspaces";
import type { DiskRecoveryReport } from "../../api/types";

// #1601 — the disk-recovery surface pins. The api module is mocked at
// the client seam (no route-level queries needed for the isolated
// strip; the workspace-status invalidation is asserted via the query
// client's cache, which the shared render helper owns).
vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    recoverDisk: vi.fn(),
  },
}));

const mocked = vi.mocked(workspacesApi.recoverDisk);

function report(overrides: Partial<DiskRecoveryReport> = {}): DiskRecoveryReport {
  return {
    dryRun: true,
    alreadyBelowTarget: false,
    beforeUsedBytes: 9600,
    beforeTotalBytes: 10000,
    afterUsedBytes: 9600,
    afterTotalBytes: 10000,
    targetRatio: 0.85,
    beforeRatio: 0.96,
    afterRatio: 0.96,
    bytesFreed: 0,
    stoppedEarly: false,
    runtimeBase: "opencode",
    classes: [
      { class: "go-build-cache", path: "/home/sandbox/.cache/go-build", bytes: 3000, bytesFreed: 0, entries: 7, status: "would_free" },
      { class: "npm-cache", path: "/home/sandbox/.npm", bytes: 1000, bytesFreed: 0, entries: 3, status: "would_free" },
      { class: "pip-cache", path: "/home/sandbox/.cache/pip", bytes: 0, bytesFreed: 0, entries: 0, status: "not_present" },
    ],
    ...overrides,
  };
}

beforeEach(() => {
  mocked.mockReset();
});

describe("DiskRecoveryStrip — surfacing rules", () => {
  it("renders nothing when disk usage is unknown", () => {
    const { container } = render(<DiskRecoveryStrip workspaceId="ws-1" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the prominent banner at >=95% (critical tier)", () => {
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    expect(screen.getByTestId("disk-recovery-strip")).toBeInTheDocument();
    expect(screen.getByText(/97%/)).toBeInTheDocument();
    expect(screen.getByTestId("disk-recover-review")).toBeInTheDocument();
  });

  it("below 95% renders only the subtle on-demand action, no banner", () => {
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={7000} diskTotalBytes={10000} />);
    expect(screen.queryByTestId("disk-recovery-strip")).not.toBeInTheDocument();
    expect(screen.getByTestId("disk-recover-on-demand")).toBeInTheDocument();
  });
});

describe("DiskRecoveryStrip — dry-run first render", () => {
  it("first click runs dryRun=true and renders the class report, deleting nothing", async () => {
    const user = userEvent.setup();
    mocked.mockResolvedValueOnce(report());
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);

    await user.click(screen.getByTestId("disk-recover-review"));

    await waitFor(() => expect(mocked).toHaveBeenCalledWith("ws-1", true));
    expect(screen.getByText("Go build cache")).toBeInTheDocument();
    expect(screen.getByText(/3 KB/)).toBeInTheDocument(); // 3000 B, toFixed(0)
    expect(screen.getAllByText("reclaimable").length).toBe(2); // both would_free classes
    // The execute button labels the measured reclaimable total.
    expect(screen.getByTestId("disk-recover-free-now")).toHaveTextContent(/4 KB/); // 3000+1000
  });

  it("Free-now executes dryRun=false and renders the freed result", async () => {
    const user = userEvent.setup();
    mocked.mockResolvedValueOnce(report());
    mocked.mockResolvedValueOnce(
      report({
        dryRun: false,
        afterUsedBytes: 6000,
        afterRatio: 0.6,
        bytesFreed: 3600,
        stoppedEarly: true,
        classes: [
          { class: "go-build-cache", path: "/home/sandbox/.cache/go-build", bytes: 3000, bytesFreed: 3000, entries: 7, status: "freed" },
          { class: "npm-cache", path: "/home/sandbox/.npm", bytes: 1000, bytesFreed: 0, entries: 3, status: "skipped_target_met" },
        ],
      })
    );
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);

    await user.click(screen.getByTestId("disk-recover-review"));
    await user.click(await screen.findByTestId("disk-recover-free-now"));

    await waitFor(() => expect(mocked).toHaveBeenCalledWith("ws-1", false));
    expect(await screen.findByText(/Freed 4 KB/)).toBeInTheDocument();
    expect(screen.getByText(/60%/)).toBeInTheDocument();
    expect(screen.getByText("skipped — target met")).toBeInTheDocument();
  });

  it("already-below-target dry-run renders the no-op message and disables Free-now", async () => {
    const user = userEvent.setup();
    mocked.mockResolvedValueOnce(report({ alreadyBelowTarget: true, classes: [] }));
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9600} diskTotalBytes={10000} />);

    await user.click(screen.getByTestId("disk-recover-review"));

    expect(await screen.findByText(/already below the 85% target/)).toBeInTheDocument();
    expect(screen.queryByTestId("disk-recover-free-now")).not.toBeInTheDocument();
  });
});

describe("DiskRecoveryStrip — refused classes stay loud", () => {
  it("renders a refused class with its reason (never silent)", async () => {
    const user = userEvent.setup();
    mocked.mockResolvedValueOnce(
      report({
        classes: [
          { class: "go-build-cache", path: "/x", bytes: 3000, bytesFreed: 0, entries: 1, status: "refused", reason: "entry is or contains a protected path" },
        ],
      })
    );
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));

    expect(await screen.findByText(/protected path/)).toBeInTheDocument();
    // Nothing reclaimable → the execute button is present but disabled.
    expect(screen.getByTestId("disk-recover-free-now")).toBeDisabled();
  });
});

describe("DiskRecoveryStrip — error path", () => {
  it("surfaces the failure with a retry affordance", async () => {
    const user = userEvent.setup();
    mocked.mockRejectedValueOnce(new Error("workspace pod is not reachable"));
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);

    await user.click(screen.getByTestId("disk-recover-review"));

    expect(await screen.findByText(/not reachable/)).toBeInTheDocument();
    expect(screen.getByText("retry")).toBeInTheDocument();
  });
});

describe("DiskRecoveryStrip — production wire shapes (review r1 F2)", () => {
  // The engine initializes classes on every path, but the strip must
  // survive a literal "classes":null on the wire (both former crash
  // sites: the reclaimable filter and the length check).
  it("renders the no-op message for a below-target report with classes:null", async () => {
    const user = userEvent.setup();
    const nullClasses = {
      ...report(),
      alreadyBelowTarget: true,
      classes: null as unknown as DiskRecoveryReport["classes"],
    };
    mocked.mockResolvedValueOnce(nullClasses);
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9600} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));
    expect(await screen.findByText(/already below the 85% target/)).toBeInTheDocument();
  });

  it("renders an above-target report with classes:null without crashing", async () => {
    const user = userEvent.setup();
    const nullClasses = {
      ...report(),
      alreadyBelowTarget: false,
      classes: null as unknown as DiskRecoveryReport["classes"],
    };
    mocked.mockResolvedValueOnce(nullClasses);
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));
    // POSITIVE assertions first (review r2): a crash unmounts the whole
    // tree, so the strip container and the still-known banner text must
    // REMAIN — absence-only assertions pass against a crashed render.
    await waitFor(() => expect(screen.getByTestId("disk-recovery-strip")).toBeInTheDocument());
    expect(screen.getByText(/97%/)).toBeInTheDocument();
    // And the degraded shape: no class table, no execute button.
    expect(screen.queryByText("Go build cache")).not.toBeInTheDocument();
    expect(screen.queryByTestId("disk-recover-free-now")).not.toBeInTheDocument();
  });
});
