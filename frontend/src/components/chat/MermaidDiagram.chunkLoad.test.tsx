// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// #2013 pins (the 2026-11-10 v0.44.12 incident): a stale open tab after a
// deploy holds the OLD index.html asset manifest, so the lazy
// `import("mermaid")` asks for a hashed chunk that no longer exists and
// rejects with `TypeError: Failed to fetch dynamically imported module:
// …/assets/mermaid.core-CGvHfpkw.js`. The diagram was VALID — but every
// failure funneled into the "invalid mermaid syntax" box, and the incident's
// agent burned two rewrite rounds "fixing" correct mermaid before pivoting
// the user to PlantUML.
//
// This file pins the LOAD class: a rejecting dynamic import renders the
// reload-class error (`mermaid-load-error` — "The app updated in the
// background — reload the tab…") and NEVER the syntax claim. The mirror pin
// (a genuine parse error KEEPS the syntax box) lives in
// MessagePart.markdownMermaid.test.tsx pin 4, where the mermaid mock
// resolves and `render` rejects — the two directions pinned in their natural
// homes.
//
// Mock posture: the mermaid PACKAGE mock's factory REJECTS, so
// `import("mermaid")` itself rejects (vitest wraps the factory rejection;
// the original TypeError rides as `cause` — the component classifies at the
// import BOUNDARY, so any rejection shape exercises the same path a real
// browser 404 takes). initialize/render mocks exist only to prove they are
// NEVER reached — the module never loads.

import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { MessagePart } from "./MessagePart";

vi.mock("../../lib/shiki", () => ({
  highlight: vi.fn().mockResolvedValue(null),
}));

const mockInitialize = vi.hoisted(() => vi.fn());
const mockRender = vi.hoisted(() => vi.fn());
vi.mock("mermaid", () =>
  Promise.reject(
    // The incident's exact failure class, verbatim from issue #2013: the
    // stale tab's manifest names a chunk the new deploy no longer serves.
    new TypeError(
      "Failed to fetch dynamically imported module: https://chat.safespaces.dev/assets/mermaid.core-CGvHfpkw.js",
    ),
  ),
);

const FLOWCHART = "flowchart TD\nA-->B";
const FLOWCHART_FENCE = "```mermaid\nflowchart TD\nA-->B\n```";

describe("#2013 pins — lazy-chunk load failure is a reload-class error, never a syntax claim", () => {
  beforeEach(() => {
    mockInitialize.mockReset();
    mockRender.mockReset();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("L1: rejecting dynamic import renders the reload-class box, never the syntax box (the incident, verbatim shape)", async () => {
    render(<MessagePart part={{ type: "text", text: FLOWCHART_FENCE }} isUser={false} />);

    const box = await screen.findByTestId("mermaid-load-error");
    // the actionable copy — the incident's user needed exactly this
    expect(box.textContent).toMatch(/app updated in the background/i);
    expect(box.textContent).toMatch(/reload the tab/i);
    // NEVER the syntax claim — the incident's misleading verdict, anywhere
    expect(box.textContent).not.toMatch(/invalid mermaid syntax/i);
    expect(screen.queryByTestId("mermaid-error")).not.toBeInTheDocument();
    // the diagram container never appeared
    expect(screen.queryByTestId("mermaid-diagram")).not.toBeInTheDocument();
    // the module never loaded: no initialize, no render attempt (r1 style:
    // plain absence assertions — nothing async left to wait for)
    expect(mockRender).not.toHaveBeenCalled();
    expect(mockInitialize).not.toHaveBeenCalled();
  });

  it("L2: the diagnostic is reframed as a load error — no 'parse error' summary over a fetch failure", async () => {
    render(<MessagePart part={{ type: "text", text: FLOWCHART_FENCE }} isUser={false} />);

    const box = await screen.findByTestId("mermaid-load-error");
    // the raw failure stays visible for diagnosis (details), but under a
    // load-error label — the incident showed a fetch 404 under "parse error"
    const summary = box.querySelector("summary");
    expect(summary?.textContent).toMatch(/load error/i);
    expect(summary?.textContent).not.toMatch(/parse error/i);
    // the diagram source survives inline (it is valid content, not garbage)
    expect(box.textContent).toContain(FLOWCHART);
  });

  it("L3: the box carries a working reload affordance", async () => {
    const reloadSpy = vi.fn();
    Object.defineProperty(window, "location", {
      value: { ...window.location, reload: reloadSpy },
      writable: true,
    });

    render(<MessagePart part={{ type: "text", text: FLOWCHART_FENCE }} isUser={false} />);

    const box = await screen.findByTestId("mermaid-load-error");
    const button = box.querySelector("button");
    expect(button).toBeInTheDocument();
    expect(button?.textContent).toMatch(/reload tab/i);
    await userEvent.click(button!);
    expect(reloadSpy).toHaveBeenCalledTimes(1);
  });
});
