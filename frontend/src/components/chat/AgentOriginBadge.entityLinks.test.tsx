// AgentOriginBadge.entityLinks.test.tsx — born-red pins for the
// injected-message header (tsk-vibrant-cadet-pearls, owner direction
// 2026-11-18): the from-session renders as a LINK to its chat with
// its ACTUAL TITLE as display text; the raw phrase/id is the fallback
// when unknown — never blank. Subscription-notification injections
// render through this same badge, so they inherit these pins.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";
import { AgentOriginBadge } from "./AgentOriginBadge";

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

import {
  publishEntities,
  resetEntityLinkState,
  setEntityResolveDelayForTests,
  setSessionWorkspaceScope,
} from "../../lib/entityLinks";
import { learnSessionAlias, resetSessionAliases } from "../../session/sessionAliases";

beforeEach(() => {
  resetEntityLinkState();
  resetSessionAliases();
  vi.clearAllMocks();
  setEntityResolveDelayForTests(5);
});
afterEach(() => {
  resetEntityLinkState();
  resetSessionAliases();
  setEntityResolveDelayForTests(250);
});

describe("AgentOriginBadge session linking", () => {
  it("shows the linked session with its actual TITLE when the index knows it", () => {
    publishEntities([
      {
        kind: "session",
        key: "session:quaint-glacier-rushes",
        id: "quaint-glacier-rushes",
        title: "Harness selection flow audit",
        workspaceId: "ws-1",
      },
    ]);
    const { getByTestId, container } = render(
      <AgentOriginBadge origin={{ fromSession: "quaint-glacier-rushes" }} />,
    );
    const link = container.querySelector("a");
    expect(link).not.toBeNull();
    expect(link?.getAttribute("href")).toBe("/chat/ws-1/quaint-glacier-rushes");
    expect(link?.textContent).toBe("Harness selection flow audit");
    // A11y: type + id + title; hover keeps the raw phrase for copy.
    expect(link?.getAttribute("aria-label")).toBe(
      "Session quaint-glacier-rushes — Harness selection flow audit",
    );
    expect(link?.getAttribute("title")).toBe("quaint-glacier-rushes");
    expect(getByTestId("agent-origin-badge").textContent).toContain(
      "message from session Harness selection flow audit",
    );
  });

  it("renders the raw phrase as fallback (plain text, never blank) when the session is unknown", () => {
    const { getByTestId, container } = render(
      <AgentOriginBadge origin={{ fromSession: "merry-finch-loses" }} />,
    );
    expect(container.querySelector("a")).toBeNull();
    const id = getByTestId("agent-origin-session-id");
    expect(id.textContent).toBe("merry-finch-loses");
    expect(id.getAttribute("title")).toBe("merry-finch-loses");
    expect(getByTestId("agent-origin-badge").textContent).toContain(
      "message from session merry-finch-loses",
    );
  });

  it("resolves raw ses_* harness ids through the learned alias table", () => {
    learnSessionAlias("ws-1", "quaint-glacier-rushes", "ses_rawAb9");
    publishEntities([
      {
        kind: "session",
        key: "session:quaint-glacier-rushes",
        id: "quaint-glacier-rushes",
        title: "Harness selection flow audit",
        workspaceId: "ws-1",
      },
    ]);
    setSessionWorkspaceScope(["ws-1"]);
    const { container } = render(
      <AgentOriginBadge origin={{ fromSession: "ses_rawAb9" }} />,
    );
    const link = container.querySelector("a");
    expect(link?.getAttribute("href")).toBe("/chat/ws-1/quaint-glacier-rushes");
    expect(link?.textContent).toBe("Harness selection flow audit");
    expect(link?.getAttribute("title")).toBe("ses_rawAb9");
  });

  it("falls back to the raw ses_* id when no alias/index knows it", () => {
    const { getByTestId, container } = render(
      <AgentOriginBadge origin={{ fromSession: "ses_f4990c383ffe6Jr3rKx1nyKwtx" }} />,
    );
    expect(container.querySelector("a")).toBeNull();
    expect(getByTestId("agent-origin-session-id").textContent).toBe(
      "ses_f4990c383ffe6Jr3rKx1nyKwtx",
    );
  });
});
