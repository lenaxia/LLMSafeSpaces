// MessagePart.entityLinks.test.tsx — born-red pins for entity
// auto-linking through the shared markdown pipeline
// (tsk-vibrant-cadet-pearls):
//   • a seeded tsk- id renders EXACTLY ONE link to the right target,
//     with the type+title accessible label
//   • an unresolvable 3-word kebab stays plain text (false-positive
//     pin) — pattern-matching alone never links
//   • code spans are never linkified
//   • rerender stability: identical message re-renders produce the
//     same single link, no churn/duplication
//   • lazy resolution lands asynchronously and then stabilizes

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { render } from "../../test/utils";
import { MessagePart } from "./MessagePart";

vi.mock("../../api/board", () => ({
  boardApi: {
    getTask: vi.fn().mockRejectedValue(new Error("no net")),
    getEpic: vi.fn().mockRejectedValue(new Error("no net")),
    getProject: vi.fn().mockRejectedValue(new Error("no net")),
  },
}));
vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    getSession: vi.fn().mockRejectedValue(new Error("no net")),
  },
}));

import { boardApi } from "../../api/board";
import {
  publishEntities,
  resetEntityLinkState,
  setEntityResolveDelayForTests,
} from "../../lib/entityLinks";

const mockedGetTask = vi.mocked(boardApi.getTask);

beforeEach(() => {
  resetEntityLinkState();
  vi.clearAllMocks();
  setEntityResolveDelayForTests(5);
});
afterEach(() => {
  resetEntityLinkState();
  setEntityResolveDelayForTests(250);
});

describe("MessagePart entity auto-linking", () => {
  it("renders exactly one link for a seeded story id, targeting the board deep link with a type+title label", () => {
    publishEntities([
      { kind: "task", key: "task:tsk-yeasty-magnet-scores", id: "tsk-yeasty-magnet-scores", title: "Fix yeasty magnet scoring" },
    ]);
    const { container } = render(
      <MessagePart
        part={{ type: "text", text: "handled in tsk-yeasty-magnet-scores today" }}
        isUser={false}
      />,
    );
    const links = container.querySelectorAll("a");
    expect(links).toHaveLength(1);
    const link = links.item(0);
    if (!link) throw new Error("link missing");
    expect(link.getAttribute("href")).toBe("/board?story=tsk-yeasty-magnet-scores");
    expect(link.getAttribute("aria-label")).toBe(
      "Board story tsk-yeasty-magnet-scores — Fix yeasty magnet scoring",
    );
    expect(link.textContent).toBe("tsk-yeasty-magnet-scores");
    // Keyboard-navigable real anchor (router Link renders one).
    expect(link.tagName).toBe("A");
  });

  it("links sessions to their chat route", () => {
    publishEntities([
      { kind: "session", key: "session:quaint-glacier-rushes", id: "quaint-glacier-rushes", title: "Harness selection flow audit", workspaceId: "ws-1" },
    ]);
    const { container } = render(
      <MessagePart part={{ type: "text", text: "ask quaint-glacier-rushes" }} isUser={false} />,
    );
    const links = container.querySelectorAll("a");
    expect(links).toHaveLength(1);
    const link = links.item(0);
    if (!link) throw new Error("link missing");
    expect(link.getAttribute("href")).toBe("/chat/ws-1/quaint-glacier-rushes");
    expect(link.getAttribute("aria-label")).toBe(
      "Session quaint-glacier-rushes — Harness selection flow audit",
    );
  });

  it("keeps an unresolvable 3-word kebab as plain text (false-positive pin)", () => {
    const { container } = render(
      <MessagePart part={{ type: "text", text: "ship on-the-fly maybe" }} isUser={false} />,
    );
    expect(container.querySelectorAll("a")).toHaveLength(0);
    expect(screen.getByText("ship on-the-fly maybe")).toBeDefined();
  });

  it("never linkifies ids inside code spans, even when resolved", () => {
    publishEntities([
      { kind: "task", key: "task:tsk-yeasty-magnet-scores", id: "tsk-yeasty-magnet-scores", title: "T" },
    ]);
    const { container } = render(
      <MessagePart
        part={{ type: "text", text: "run `tsk-yeasty-magnet-scores` now" }}
        isUser={false}
      />,
    );
    expect(container.querySelectorAll("a")).toHaveLength(0);
    expect(screen.getByText("tsk-yeasty-magnet-scores").tagName).toBe("CODE");
  });

  it("is rerender-stable: the same message re-renders one link, not more", () => {
    publishEntities([
      { kind: "task", key: "task:tsk-yeasty-magnet-scores", id: "tsk-yeasty-magnet-scores", title: "Fix yeasty magnet scoring" },
    ]);
    const part = { type: "text", text: "handled in tsk-yeasty-magnet-scores today" } as const;
    const view = render(<MessagePart part={{ ...part }} isUser={false} />);
    expect(view.container.querySelectorAll("a")).toHaveLength(1);
    view.rerender(<MessagePart part={{ ...part }} isUser={false} />);
    view.rerender(<MessagePart part={{ ...part }} isUser={false} />);
    expect(view.container.querySelectorAll("a")).toHaveLength(1);
    expect(view.container.querySelector("a")?.getAttribute("href")).toBe(
      "/board?story=tsk-yeasty-magnet-scores",
    );
  });

  it("lazily resolves an unknown story id through the batched resolver, then stays stable", async () => {
    mockedGetTask.mockResolvedValue({
      id: "uuid-9",
      short_id: "tsk-cold-story",
      title: "Cold resolved story",
    } as Awaited<ReturnType<typeof boardApi.getTask>>);
    const view = render(
      <MessagePart part={{ type: "text", text: "see tsk-cold-story" }} isUser={false} />,
    );
    // Before resolution: plain text, no link.
    expect(view.container.querySelectorAll("a")).toHaveLength(0);
    await waitFor(
      () => {
        expect(view.container.querySelectorAll("a")).toHaveLength(1);
      },
      { timeout: 2000 },
    );
    const link = view.container.querySelector("a");
    expect(link?.getAttribute("href")).toBe("/board?story=tsk-cold-story");
    expect(link?.getAttribute("aria-label")).toBe("Board story tsk-cold-story — Cold resolved story");
    // Settled: the probe served from cache, no second network call.
    const calls = mockedGetTask.mock.calls.length;
    view.rerender(<MessagePart part={{ type: "text", text: "see tsk-cold-story" }} isUser={false} />);
    expect(mockedGetTask.mock.calls.length).toBe(calls);
    expect(view.container.querySelectorAll("a")).toHaveLength(1);
  });

  it("assistant streaming text still resolves from the loaded index without lazy probes", () => {
    publishEntities([
      { kind: "task", key: "task:tsk-yeasty-magnet-scores", id: "tsk-yeasty-magnet-scores", title: "T" },
    ]);
    const { container } = render(
      <MessagePart
        part={{ type: "text", text: "streaming tsk-yeasty-magnet-scores…" }}
        isUser={false}
        isStreaming={true}
      />,
    );
    expect(container.querySelectorAll("a")).toHaveLength(1);
    expect(mockedGetTask).not.toHaveBeenCalled();
  });
});
