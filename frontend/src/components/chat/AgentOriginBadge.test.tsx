import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { AgentOriginBadge } from "./AgentOriginBadge";

describe("AgentOriginBadge", () => {
  it("renders the origin session as a return address", () => {
    const { getByTestId } = render(<AgentOriginBadge origin={{ fromSession: "ses_f4965f03dffetQIEh2NbI526fv" }} />);
    expect(getByTestId("agent-origin-badge").textContent).toContain(
      "message from session ses_f4965f03dffetQIEh2NbI526fv",
    );
  });

  it("renders the workspace when present (future cross-workspace origin)", () => {
    const { getByTestId } = render(
      <AgentOriginBadge origin={{ fromSession: "ses_a", workspace: "d8bed486-2eec-4db6-a8ff-e11ba404a055" }} />,
    );
    expect(getByTestId("agent-origin-badge").textContent).toContain("workspace d8bed486");
  });

  it("marks self-declared origins (degraded-pod fallback)", () => {
    const { getByTestId } = render(<AgentOriginBadge origin={{ fromSession: "ses_a", mode: "self-declared" }} />);
    expect(getByTestId("agent-origin-badge").textContent).toContain("self-declared origin");
  });

  it("does not mark injected origins", () => {
    const { getByTestId } = render(<AgentOriginBadge origin={{ fromSession: "ses_a", mode: "injected" }} />);
    expect(getByTestId("agent-origin-badge").textContent).not.toContain("self-declared");
  });

  it("omits the workspace clause for local senders", () => {
    const { getByTestId } = render(<AgentOriginBadge origin={{ fromSession: "ses_a" }} />);
    expect(getByTestId("agent-origin-badge").textContent).not.toContain("workspace");
  });

  // #1465 owner report: the badge TRUNCATED the origin session ID
  // (nowrap + ellipsis via `truncate` on the label span) instead of
  // wrapping, hiding the return address. Pinned here: the ID renders
  // in full inside its own break-all element (house convention for
  // monospace IDs — TriggersPage/ApiKeysTab), carries the full ID as
  // title for hover-copy, and NO element in the badge subtree carries
  // truncation/nowrap utilities — the historical bug was on the OUTER
  // label span, so scanning only the inner ID span would miss exactly
  // that reintroduction (r1 review finding).
  it("renders the full session ID, wrapping, with a hover title, and no truncation anywhere in the badge", () => {
    const fullId = "ses_f4990c383ffe6Jr3rKx1nyKwtx"; // realistic 30-char platform ID
    const { getByTestId } = render(<AgentOriginBadge origin={{ fromSession: fullId }} />);
    const idEl = getByTestId("agent-origin-session-id");
    expect(idEl.textContent).toBe(fullId);
    expect(idEl.getAttribute("title")).toBe(fullId);
    expect(idEl.className).toContain("break-all");
    expect(idEl.className).not.toContain("truncate");
    // The full ID must be present in the rendered badge text — no
    // ellipsis substitution at the DOM level.
    expect(getByTestId("agent-origin-badge").textContent).toContain(fullId);
    // No truncation/nowrap semantics anywhere in the badge subtree —
    // re-adding `truncate` or `whitespace-nowrap` to ANY span (the
    // outer label span is the historical location) fails here.
    const badge = getByTestId("agent-origin-badge");
    for (const el of [badge, ...Array.from(badge.querySelectorAll("*"))]) {
      // getAttribute, not el.className: SVG elements expose
      // SVGAnimatedString, on which toContain passes unconditionally
      // (r2). One bare "nowrap" substring catches every utility form —
      // whitespace-nowrap, text-nowrap, and the Tailwind arbitrary
      // property [white-space:nowrap] (computed-identical to truncate;
      // invisible to "whitespace-nowrap" scans). No style scan: jsdom
      // never applies the stylesheet, so el.style can only ever see
      // inline declarations — a class-driven mutation is invisible to
      // it (r3 finding; the r2 style scan was dead code and its
      // claimed mutation verification never occurred).
      const cls = el.getAttribute("class") ?? "";
      expect(cls).not.toContain("truncate");
      expect(cls).not.toContain("nowrap");
    }
  });
});
