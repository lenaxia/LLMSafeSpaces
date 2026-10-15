import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { DiskUsageBar } from "./DiskUsageBar";

// DiskUsageBar renders both a desktop and mobile layout simultaneously.
// JSDOM does not apply CSS breakpoints, so both the `hidden sm:flex` (desktop)
// and `sm:hidden` (mobile) containers are visible. Use getAllBy* when a label
// or value is expected to appear in both layouts.

describe("DiskUsageBar — context unknown (contextTotal=0)", () => {
  // ── Happy path — known limit ─────────────────────────────────────────────

  it("renders context progress bar when contextTotal > 0", () => {
    render(<DiskUsageBar contextUsed={50000} contextTotal={200000} />);
    expect(screen.getAllByText(/context/i).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/50K/).length).toBeGreaterThan(0);
    expect(screen.queryByText(/unknown/i)).not.toBeInTheDocument();
  });

  it("always renders context bar with Unknown when no metrics are provided", () => {
    render(<DiskUsageBar />);
    expect(screen.getAllByText(/Context/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/Unknown/).length).toBeGreaterThan(0);
  });

  // ── Unknown context limit ────────────────────────────────────────────────

  it("shows 'Unknown' badge instead of progress bar when contextTotal=0", () => {
    render(<DiskUsageBar contextUsed={150000} contextTotal={0} />);
    expect(screen.getAllByText(/unknown/i).length).toBeGreaterThan(0);
  });

  it("shows 'Unknown' badge when contextTotal is undefined", () => {
    render(<DiskUsageBar contextUsed={150000} />);
    expect(screen.getAllByText(/unknown/i).length).toBeGreaterThan(0);
  });

  it("shows used token count even when limit is unknown", () => {
    render(<DiskUsageBar contextUsed={150000} contextTotal={0} />);
    expect(screen.getAllByText(/150K/).length).toBeGreaterThan(0);
  });

  it("does not render a progress bar when contextTotal=0", () => {
    const { container } = render(<DiskUsageBar contextUsed={150000} contextTotal={0} />);
    const progressBars = container.querySelectorAll(".bg-primary\\/40, .bg-orange-500");
    expect(progressBars).toHaveLength(0);
  });

  it("does not show a percentage when contextTotal=0", () => {
    render(<DiskUsageBar contextUsed={150000} contextTotal={0} />);
    expect(screen.queryByText(/%/)).not.toBeInTheDocument();
  });

  // ── Tooltip on unknown (Radix UI — async hover with delayDuration=0) ──────

  it("shows tooltip on hover of Unknown badge explaining auto-compaction is disabled", async () => {
    const user = userEvent.setup();
    render(<DiskUsageBar contextUsed={150000} contextTotal={0} />);
    const badge = screen.getAllByRole("button", { name: /context limit unknown/i })[0]!;
    await user.hover(badge);
    await waitFor(() => {
      expect(screen.getAllByText(/auto-compaction is disabled/i).length).toBeGreaterThan(0);
    });
  });

  it("hides tooltip when mouse leaves Unknown badge", async () => {
    const user = userEvent.setup();
    render(<DiskUsageBar contextUsed={150000} contextTotal={0} />);
    const badge = screen.getAllByRole("button", { name: /context limit unknown/i })[0]!;
    await user.hover(badge);
    await waitFor(() => {
      expect(screen.getAllByText(/auto-compaction is disabled/i).length).toBeGreaterThan(0);
    });
    await user.keyboard("{Escape}");
    await waitFor(() => {
      expect(screen.queryByText(/auto-compaction is disabled/i)).not.toBeInTheDocument();
    });
  });

  it("tooltip mentions max_input_tokens as the fix", async () => {
    const user = userEvent.setup();
    render(<DiskUsageBar contextUsed={150000} contextTotal={0} />);
    const badge = screen.getAllByRole("button", { name: /context limit unknown/i })[0]!;
    await user.hover(badge);
    await waitFor(() => {
      expect(screen.getAllByText(/max_input_tokens/i).length).toBeGreaterThan(0);
    });
  });

  // ── Edge cases ────────────────────────────────────────────────────────────

  it("still shows disk and memory metrics alongside unknown context", () => {
    render(<DiskUsageBar
      contextUsed={150000}
      contextTotal={0}
      diskUsedBytes={1024 * 1024 * 500}
      diskTotalBytes={1024 * 1024 * 1024 * 10}
    />);
    expect(screen.getAllByText(/unknown/i).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/disk/i).length).toBeGreaterThan(0);
  });

  it("does not count unknown context as a critical metric", () => {
    render(<DiskUsageBar contextUsed={999999} contextTotal={0} />);
    expect(screen.queryByText(/critical/i)).not.toBeInTheDocument();
  });
});

describe("DiskUsageBar — mobile drawer hide semantics (#1686 item 3)", () => {
  // Review r1 root cause: while a metric is critical the mobile drawer
  // auto-opens — but an EXPLICIT close must stick. The pre-#1686
  // toggleDrawer cleared mobileAutoOpened, so the auto-open effect
  // immediately re-opened the drawer: the Hide control could not hide
  // exactly when it mattered (the owner's mobile repro).
  it("explicit Hide sticks while a metric is critical", async () => {
    const user = userEvent.setup();
    // Disk 97% — critical (threshold 85). Both layouts render in JSDOM;
    // the mobile toggle is the Hide/Metrics button.
    render(<DiskUsageBar diskUsedBytes={9700} diskTotalBytes={10000} contextUsed={10} contextTotal={100} />);

    // Auto-opened on mount: the toggle reads "Hide".
    const hide = await screen.findByRole("button", { name: /^Hide$/ });
    await user.click(hide);

    // The drawer must STAY closed — the toggle reads "Metrics", no Hide
    // remains (a re-open would flip it back immediately), and the drawer
    // container itself is collapsed (review r2: pin the collapsed state,
    // not just the label).
    expect(screen.getByRole("button", { name: "Metrics" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Hide$/ })).not.toBeInTheDocument();
    const drawer = document.querySelector(".overflow-hidden.transition-all");
    expect(drawer).not.toBeNull();
    expect(drawer!.className).toContain("max-h-0");
    expect(drawer!.className).toContain("opacity-0");
  });
});
