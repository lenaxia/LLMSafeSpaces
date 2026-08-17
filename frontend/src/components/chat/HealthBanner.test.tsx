import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { HealthBanner } from "./HealthBanner";

describe("HealthBanner", () => {
  it("renders nothing when healthy", () => {
    const { container } = render(
      <HealthBanner
        credentialState={{ available: true, reason: "CredentialsValid" }}
        agentHealth={{ status: "Healthy" }}
      />,
    );
    expect(container.innerHTML).toBe("");
  });

  // PR #909 review round: Healthy-with-warnings must render — the agent is
  // fine but running degraded (e.g. default model unresolvable, incident
  // 2026-08-16) and silently substituting the model breaks user intent.
  it("renders warnings when healthy with warnings", () => {
    render(
      <HealthBanner
        agentHealth={{
          status: "Healthy",
          warnings: ['default model "deepseek-v4-flash-free" unavailable — using the agent default model'],
        }}
      />,
    );
    expect(
      screen.getByText(/deepseek-v4-flash-free.*unavailable/),
    ).toBeInTheDocument();
  });

  it("renders every warning as its own row", () => {
    render(
      <HealthBanner
        agentHealth={{ status: "Healthy", warnings: ["warning one", "warning two"] }}
      />,
    );
    expect(screen.getByText("warning one")).toBeInTheDocument();
    expect(screen.getByText("warning two")).toBeInTheDocument();
  });

  it("renders nothing when healthy with empty warnings array", () => {
    const { container } = render(
      <HealthBanner agentHealth={{ status: "Healthy", warnings: [] }} />,
    );
    expect(container.innerHTML).toBe("");
  });

  // Review round 2: warnings can ride Degraded conditions too (the
  // controller appends on both sites). The status label shows WITHOUT the
  // raw "; warnings: ..." suffix, and warnings render as separate rows.
  it("renders degraded message without raw suffix plus structured warning rows", () => {
    render(
      <HealthBanner
        agentHealth={{
          status: "Degraded",
          message:
            'no providers connected (configured=1, connected=[]); warnings: default model "glm-5.3" unavailable — using the agent default model',
          warnings: ['default model "glm-5.3" unavailable — using the agent default model'],
        }}
      />,
    );
    expect(
      screen.getByText(/no providers connected \(configured=1/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/; warnings:/)).not.toBeInTheDocument();
    expect(
      screen.getByText(/glm-5.3.*unavailable/),
    ).toBeInTheDocument();
  });

  it("renders nothing when props are undefined", () => {
    const { container } = render(<HealthBanner />);
    expect(container.innerHTML).toBe("");
  });

  it("shows Opencode Zen message when no credential secret exists", () => {
    render(
      <HealthBanner
        credentialState={{ available: false, reason: "CredentialSecretNotFound" }}
      />,
    );
    expect(screen.getByText(/No providers configured/)).toBeInTheDocument();
    const link = screen.getByText("Click here to learn more");
    expect(link).toBeInTheDocument();
    expect(link.closest("a")).toHaveAttribute("href", "https://opencode.ai");
  });

  it("hides banner when credential state is not checked", () => {
    const { container } = render(
      <HealthBanner
        credentialState={{ available: false, reason: "NotChecked" }}
      />,
    );
    expect(container.innerHTML).toBe("");
  });

  it("renders credential warning when credentials empty", () => {
    render(
      <HealthBanner
        credentialState={{ available: false, reason: "CredentialEmpty" }}
      />,
    );
    expect(screen.getByText("Credentials are empty")).toBeInTheDocument();
  });

  it("renders credential warning when credentials invalid", () => {
    render(
      <HealthBanner
        credentialState={{ available: false, reason: "CredentialInvalid" }}
      />,
    );
    expect(screen.getByText("Credentials are invalid")).toBeInTheDocument();
  });

  it("renders agent degraded warning", () => {
    render(
      <HealthBanner
        agentHealth={{ status: "Degraded", message: "no providers connected" }}
      />,
    );
    expect(screen.getByText("no providers connected")).toBeInTheDocument();
  });

  it("renders agent unhealthy warning", () => {
    render(
      <HealthBanner
        agentHealth={{ status: "Unhealthy", message: "agent crashed" }}
      />,
    );
    expect(screen.getByText("agent crashed")).toBeInTheDocument();
  });

  it("renders agent unknown warning", () => {
    render(
      <HealthBanner agentHealth={{ status: "Unknown" }} />
    );
    expect(screen.getByText("Agent health unknown")).toBeInTheDocument();
  });

  it("renders both credential and agent issues", () => {
    render(
      <HealthBanner
        credentialState={{ available: false, reason: "CredentialInvalid" }}
        agentHealth={{ status: "Unhealthy", message: "down" }}
      />,
    );
    expect(screen.getByText("Credentials are invalid")).toBeInTheDocument();
    expect(screen.getByText("down")).toBeInTheDocument();
  });

  it("renders custom message for unknown reason with message", () => {
    render(
      <HealthBanner
        credentialState={{ available: false, reason: "SomeNewReason", message: "custom msg" }}
      />,
    );
    expect(screen.getByText("custom msg")).toBeInTheDocument();
  });

  it("hides banner for unknown reason without message", () => {
    const { container } = render(
      <HealthBanner
        credentialState={{ available: false, reason: "SomeNewReason" }}
      />,
    );
    expect(container.innerHTML).toBe("");
  });
});
