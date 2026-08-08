// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, vi } from "vitest";
import { WorkflowEditor } from "./WorkflowEditor";

describe("WorkflowEditor", () => {
  it("renders create mode with empty fields", () => {
    render(
      <WorkflowEditor
        mode="create"
        onSave={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    expect(screen.getByPlaceholderText("Workflow name")).toBeInTheDocument();
    expect(screen.getByText("Create")).toBeInTheDocument();
  });

  it("renders edit mode with workflow data", () => {
    render(
      <WorkflowEditor
        mode="edit"
        workflow={{
          id: "wf-1", ownerType: "user", name: "test-wf", slug: "test-wf",
          description: "", specYaml: '{"nodes":[]}', status: "active",
          createdAt: "", updatedAt: "",
        }}
        onSave={vi.fn()}
        onDelete={vi.fn()}
        onRun={vi.fn()}
      />,
    );
    expect(screen.getByDisplayValue("test-wf")).toBeInTheDocument();
    expect(screen.getByText("Save")).toBeInTheDocument();
    expect(screen.getByText("Run")).toBeInTheDocument();
    expect(screen.getByText("Delete")).toBeInTheDocument();
  });

  it("disables save button when name is empty", () => {
    render(
      <WorkflowEditor mode="create" onSave={vi.fn()} onCancel={vi.fn()} />,
    );
    const saveButton = screen.getByText("Create").closest("button");
    expect(saveButton).toBeDisabled();
  });

  it("calls onSave with name, spec, and status", async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(
      <WorkflowEditor mode="create" onSave={onSave} onCancel={vi.fn()} />,
    );
    fireEvent.change(screen.getByPlaceholderText("Workflow name"), { target: { value: "new-wf" } });
    fireEvent.click(screen.getByText("Create"));
    await waitFor(() => {
      expect(onSave).toHaveBeenCalledWith("new-wf", expect.any(String), "draft");
    });
  });

  it("shows error message on save failure", async () => {
    const onSave = vi.fn().mockRejectedValue(new Error("API error"));
    render(
      <WorkflowEditor mode="create" onSave={onSave} onCancel={vi.fn()} />,
    );
    fireEvent.change(screen.getByPlaceholderText("Workflow name"), { target: { value: "test" } });
    fireEvent.click(screen.getByText("Create"));
    await waitFor(() => {
      expect(screen.getByText("API error")).toBeInTheDocument();
    });
  });

  it("shows run dialog when Run clicked", () => {
    render(
      <WorkflowEditor
        mode="edit"
        workflow={{
          id: "wf-1", ownerType: "user", name: "test", slug: "test",
          description: "", specYaml: "{}", status: "active",
          createdAt: "", updatedAt: "",
        }}
        onSave={vi.fn()}
        onDelete={vi.fn()}
        onRun={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByText("Run"));
    expect(screen.getByText("Run Workflow")).toBeInTheDocument();
    expect(screen.getByText("Start Run")).toBeInTheDocument();
  });
});
