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

  it("renders the compact banner at >=95% (critical tier)", () => {
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    expect(screen.getByTestId("disk-recovery-strip")).toBeInTheDocument();
    expect(screen.getByText(/97%/)).toBeInTheDocument();
    expect(screen.getByTestId("disk-recover-review")).toBeInTheDocument();
  });

  // #1686 (owner dogfooding): the strip renders ONLY at the critical tier.
  // The pre-#1686 behavior — a permanent "Free up disk space" on-demand
  // line whenever disk metrics were known — trained users to ignore the
  // safety banner (it showed at 5% as at 97%), occupied a dedicated line
  // under the metrics row, and on mobile sat right below the Hide/Metrics
  // toggle so "hide" never cleared it. This pin was deliberately flipped
  // from "below 95% renders only the subtle on-demand action": below the
  // threshold the component must render NOTHING.
  it("below 95% renders nothing at all (#1686 — no on-demand line)", () => {
    const { container } = render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={7000} diskTotalBytes={10000} />);
    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByTestId("disk-recovery-strip")).not.toBeInTheDocument();
    expect(screen.queryByTestId("disk-recover-on-demand")).not.toBeInTheDocument();
  });

  it("threshold boundary: 94% renders nothing, 95% renders the banner", () => {
    const below = render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9400} diskTotalBytes={10000} />);
    expect(below.container).toBeEmptyDOMElement();
    below.unmount();
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9500} diskTotalBytes={10000} />);
    expect(screen.getByTestId("disk-recovery-strip")).toBeInTheDocument();
  });

  // Review r1: the threshold compares the RAW ratio (systemnotices mirror)
  // — a rounded comparison would surface 94.5-94.99% as "95%" and
  // over-alert vs the backend's floored critical tier.
  it("sub-integer boundary: 94.5% renders nothing (raw ratio, floored display)", () => {
    const { container } = render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9450} diskTotalBytes={10000} />);
    expect(container).toBeEmptyDOMElement();
  });

  // Review r1: strip state is workspace-scoped. ChatPage's Outlet is not
  // keyed, so the instance survives workspace switches — a dismissal (or
  // in-flight report) from one workspace must not silence another.
  it("a dismissal does not leak across a workspace switch (review r1)", async () => {
    const user = userEvent.setup();
    const view = render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recovery-dismiss"));
    expect(view.container).toBeEmptyDOMElement();

    // Same instance, NEW workspace at 96% — the banner must be back.
    view.rerender(<DiskRecoveryStrip workspaceId="ws-2" diskUsedBytes={9600} diskTotalBytes={10000} />);
    expect(screen.getByTestId("disk-recovery-strip")).toBeInTheDocument();
    expect(screen.getByText(/96%/)).toBeInTheDocument();
  });

  it("an in-flight report does not survive a workspace switch (no wrong-workspace execute)", async () => {
    const user = userEvent.setup();
    mocked.mockResolvedValueOnce(report());
    const view = render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));
    expect(await screen.findByText("Go build cache")).toBeInTheDocument();

    view.rerender(<DiskRecoveryStrip workspaceId="ws-2" diskUsedBytes={9600} diskTotalBytes={10000} />);
    // ws-2 is critical but the stale ws-1 report is gone; a fresh banner.
    expect(screen.queryByText("Go build cache")).not.toBeInTheDocument();
    expect(screen.getByTestId("disk-recover-review")).toBeInTheDocument();
  });

  // Review r1: a terminal error must not keep the strip mounted below the
  // threshold ("renders ONLY at >=95%" — error retention falsified it).
  it("a failed recovery leaves no residue once usage drops below the threshold", async () => {
    const user = userEvent.setup();
    mocked.mockRejectedValueOnce(new Error("pod unreachable"));
    const view = render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));
    expect(await screen.findByText(/pod unreachable/)).toBeInTheDocument(); // visible while critical

    view.rerender(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={7000} diskTotalBytes={10000} />);
    expect(view.container).toBeEmptyDOMElement();
  });

  it("retry re-runs the dry-run (not just clears the error)", async () => {
    const user = userEvent.setup();
    mocked.mockRejectedValueOnce(new Error("pod unreachable"));
    mocked.mockResolvedValueOnce(report());
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));
    await user.click(await screen.findByText("retry"));
    await waitFor(() => expect(mocked).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("Go build cache")).toBeInTheDocument();
  });

  // Review r2 race pins: react-query dispatches mutate-time callbacks
  // irrespective of current props — a response started on ws-1 that
  // settles after the switch to ws-2 must be DROPPED, or ws-A's report
  // (whose "Free N now" would sweep ws-B with ws-A's numbers) or error
  // resurrects on the wrong workspace.
  it("a dry-run response arriving after a workspace switch is dropped (no stale report, no wrong-workspace sweep)", async () => {
    const user = userEvent.setup();
    let resolveDryRun: (r: DiskRecoveryReport) => void = () => {};
    mocked.mockImplementationOnce(() => new Promise<DiskRecoveryReport>((res) => { resolveDryRun = res; }));
    const view = render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));

    // Switch BEFORE the response lands.
    view.rerender(<DiskRecoveryStrip workspaceId="ws-2" diskUsedBytes={9600} diskTotalBytes={10000} />);
    resolveDryRun(report());

    // The stale report must never render: ws-2 shows its fresh banner only.
    await waitFor(() => expect(screen.getByTestId("disk-recovery-strip")).toBeInTheDocument());
    expect(screen.queryByText("Go build cache")).not.toBeInTheDocument();
    expect(screen.queryByTestId("disk-recover-free-now")).not.toBeInTheDocument();
    expect(screen.getByTestId("disk-recover-review")).toBeInTheDocument();
  });

  it("a late error arriving after a workspace switch plants nothing", async () => {
    const user = userEvent.setup();
    let rejectRun: (e: Error) => void = () => {};
    mocked.mockImplementationOnce(() => new Promise<DiskRecoveryReport>((_, rej) => { rejectRun = rej; }));
    const view = render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));

    view.rerender(<DiskRecoveryStrip workspaceId="ws-2" diskUsedBytes={9600} diskTotalBytes={10000} />);
    rejectRun(new Error("ws-1 pod unreachable"));

    // ws-2's banner stays clean — no ws-1 error text, no retry planting.
    await waitFor(() => expect(screen.getByTestId("disk-recovery-strip")).toBeInTheDocument());
    expect(screen.queryByText(/ws-1 pod unreachable/)).not.toBeInTheDocument();
    expect(screen.getByTestId("disk-recover-review")).toBeInTheDocument();
  });

  // Review r2: a terminal error must not survive into the NEXT critical
  // episode on the same workspace — the episode reset clears it.
  it("an error does not resurrect on a later critical episode of the same workspace", async () => {
    const user = userEvent.setup();
    mocked.mockRejectedValueOnce(new Error("pod unreachable"));
    const view = render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));
    expect(await screen.findByText(/pod unreachable/)).toBeInTheDocument();

    // Dip below the threshold (episode ends, error cleared) and re-cross.
    view.rerender(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9000} diskTotalBytes={10000} />);
    view.rerender(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9600} diskTotalBytes={10000} />);
    await waitFor(() => expect(screen.getByTestId("disk-recovery-strip")).toBeInTheDocument());
    expect(screen.queryByText(/pod unreachable/)).not.toBeInTheDocument();
    expect(screen.getByTestId("disk-recover-review")).toBeInTheDocument();
  });

  // #1686: dismissal is per critical episode — quieting the banner must
  // not permanently silence the safety alert. Drop below the threshold
  // (episode ends, dismissal resets) and climb back: the banner re-arms.
  it("Dismiss hides the banner; re-crossing the threshold re-arms it", async () => {
    const user = userEvent.setup();
    const view = render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recovery-dismiss"));
    expect(view.container).toBeEmptyDOMElement();

    // Still critical, still dismissed (no nagging within the episode).
    view.rerender(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9800} diskTotalBytes={10000} />);
    expect(view.container).toBeEmptyDOMElement();

    // Drop below the threshold → episode ends.
    view.rerender(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={7000} diskTotalBytes={10000} />);
    expect(view.container).toBeEmptyDOMElement();

    // Re-cross → the banner is back.
    view.rerender(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9600} diskTotalBytes={10000} />);
    expect(screen.getByTestId("disk-recovery-strip")).toBeInTheDocument();
    expect(screen.getByText(/96%/)).toBeInTheDocument();
  });

  // The compact placement (#1686 item 2): collapsed, the banner is ONE
  // line — headline, inline action and Dismiss all live in the same flex
  // row (py-1 text-[11px], not the old py-2 multi-row strip).
  it("collapsed banner is a single compact line with inline actions", () => {
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    const strip = screen.getByTestId("disk-recovery-strip");
    expect(strip.className).toContain("py-1");
    expect(strip.className).toContain("text-[11px]");
    const review = screen.getByTestId("disk-recover-review");
    const dismiss = screen.getByTestId("disk-recovery-dismiss");
    // All three affordances share the banner's first (collapsed) row.
    const row = review.closest("div");
    expect(row).toContainElement(dismiss);
    expect(row).toContainElement(screen.getByText(/critically low/i));
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

describe("DiskRecoveryStrip — #1744 classes and in_use rendering", () => {
  // #1744: classes pinned by open handles (or a hot mtime / size floor)
  // report in_use — measured bytes stay visible, nothing is claimed,
  // and they never count into the reclaimable total.
  it("renders an in_use class with its pinned bytes, reason, and zero reclaimable", async () => {
    const user = userEvent.setup();
    mocked.mockResolvedValueOnce(
      report({
        classes: [
          { class: "tmp-opencode-gocache", path: "/tmp/opencode/gocache", bytes: 2400, bytesFreed: 0, entries: 900, status: "in_use", reason: "open file handles under the cache tree" },
          { class: "go-build-cache", path: "/home/sandbox/.cache/go-build", bytes: 1000, bytesFreed: 0, entries: 4, status: "would_free" },
        ],
      })
    );
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));

    expect(await screen.findByText(/in use \(open file handles/)).toBeInTheDocument();
    expect(screen.getByText("Session Go build cache")).toBeInTheDocument();
    expect(screen.getByText("2 KB")).toBeInTheDocument(); // the pinned 2400 B, toFixed(0)
    // Only the would_free class counts toward Free-now (1000 B → "1 KB").
    expect(screen.getByTestId("disk-recover-free-now")).toHaveTextContent(/1 KB/);
  });

  it("friendly labels for the #1744 regenerable classes", async () => {
    const user = userEvent.setup();
    mocked.mockResolvedValueOnce(
      report({
        classes: [
          { class: "playwright-browsers", path: "/home/sandbox/.cache/ms-playwright", bytes: 641000000, bytesFreed: 0, entries: 4, status: "would_free" },
          { class: "gotmp-residue", path: "/home/sandbox/.cache/gotmp", bytes: 0, bytesFreed: 0, entries: 0, status: "skipped_target_met" },
          { class: "kubebuilder-envtest", path: "/workspace/.local/kubebuilder-envtest", bytes: 0, bytesFreed: 0, entries: 0, status: "skipped_target_met" },
          { class: "npm-npx", path: "/home/sandbox/.npm/_npx", bytes: 0, bytesFreed: 0, entries: 0, status: "skipped_target_met" },
          { class: "golangci-lint-cache", path: "/home/sandbox/.cache/golangci-lint", bytes: 0, bytesFreed: 0, entries: 0, status: "skipped_target_met" },
        ],
      })
    );
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));

    expect(await screen.findByText("Playwright browsers")).toBeInTheDocument();
    expect(screen.getByText("Stale Go temp dirs")).toBeInTheDocument();
    expect(screen.getByText("envtest assets")).toBeInTheDocument();
    expect(screen.getByText("npx package cache")).toBeInTheDocument();
    expect(screen.getByText("golangci-lint cache")).toBeInTheDocument();
    // Raw class slugs never leak into the render.
    expect(screen.queryByText(/ms-playwright/)).not.toBeInTheDocument();
  });
});

describe("DiskRecoveryStrip — partial-sweep honesty (review r5)", () => {
  it("renders bytesFreed (not measured bytes) and the partial reason for a partially-freed class", async () => {
    const user = userEvent.setup();
    mocked.mockResolvedValueOnce(
      report({
        dryRun: false,
        classes: [
          {
            class: "go-build-cache",
            path: "/home/sandbox/.cache/go-build",
            bytes: 5000,
            bytesFreed: 3000, // partial: externally removed mid-sweep
            entries: 7,
            status: "freed",
            reason: "partial: 3000 of 5000 measured bytes were still present at delete time",
          },
        ],
      })
    );
    render(<DiskRecoveryStrip workspaceId="ws-1" diskUsedBytes={9700} diskTotalBytes={10000} />);
    await user.click(screen.getByTestId("disk-recover-review"));

    // The Freed column must show the HONEST figure (3000 B → "3 KB"),
    // never the measured 5000 ("5 KB"), and the partial reason rides along.
    expect(await screen.findByText(/freed \(partial/)).toBeInTheDocument();
    expect(screen.queryByText("5 KB")).not.toBeInTheDocument();
    expect(screen.getByText("3 KB")).toBeInTheDocument();
  });
});
