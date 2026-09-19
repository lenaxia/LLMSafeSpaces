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
});
