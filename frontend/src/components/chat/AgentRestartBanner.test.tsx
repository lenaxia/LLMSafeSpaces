import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { AgentRestartBanner } from "./AgentRestartBanner";
import type { AgentRestartEvent } from "../../api/types";

const RESTART: AgentRestartEvent = {
  reason: "health_watchdog",
  verdict: "hung",
  at: "2026-10-19T17:08:14Z",
};

describe("AgentRestartBanner", () => {
  it("renders nothing without a restart record", () => {
    const { container } = render(<AgentRestartBanner />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the watchdog restart with agent-neutral copy", () => {
    render(<AgentRestartBanner restart={RESTART} />);
    const banner = screen.getByRole("alert");
    expect(banner).toHaveTextContent("restarted by the supervisor at");
    expect(banner).toHaveTextContent("stopped responding to health checks");
    expect(banner).toHaveTextContent("In-flight work may have been interrupted");
    expect(banner).not.toHaveTextContent("opencode");
  });

  it("dismisses and stays dismissed for the same restart", () => {
    render(<AgentRestartBanner restart={RESTART} />);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("re-shows for a NEW restart (different timestamp)", () => {
    const { rerender } = render(<AgentRestartBanner restart={RESTART} />);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("alert")).toBeNull();

    const second: AgentRestartEvent = { ...RESTART, at: "2026-10-19T18:26:40Z", reason: "health_watchdog_unknown_episode", verdict: "unknown" };
    rerender(<AgentRestartBanner restart={second} />);
    expect(screen.getByRole("alert")).toHaveTextContent("health evidence was unavailable for too long");
  });

  it("renders uncorroborated crash restarts without a verdict clause", () => {
    render(<AgentRestartBanner restart={{ reason: "crash", at: "2026-10-19T17:08:14Z" }} />);
    const banner = screen.getByRole("alert");
    expect(banner).toHaveTextContent("exited unexpectedly");
    expect(banner).not.toHaveTextContent("(");
  });
});
