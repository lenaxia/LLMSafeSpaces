// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { ToastProvider } from "../../providers/ToastProvider";
import { PromptsTab } from "./PromptsTab";

const mockList = vi.fn();
const mockCreate = vi.fn();
const mockUpdate = vi.fn();
const mockDelete = vi.fn();

vi.mock("../../api/userPrompts", () => ({
  userPromptsApi: {
    list: () => mockList(),
    create: (req: unknown) => mockCreate(req),
    update: (id: string, req: unknown) => mockUpdate(id, req),
    delete: (id: string) => mockDelete(id),
  },
}));

const P1 = {
  id: "p1",
  name: "Weekly summary",
  content: "Summarize the week.",
  createdAt: "2026-09-01T00:00:00Z",
  updatedAt: "2026-09-20T00:00:00Z",
};

function renderTab() {
  return render(
    <ToastProvider>
      <PromptsTab />
    </ToastProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("PromptsTab", () => {
  it("renders the named-envelope list", async () => {
    mockList.mockResolvedValue({ prompts: [P1] });
    renderTab();
    await waitFor(() => expect(screen.getByText("Weekly summary")).toBeTruthy());
    expect(screen.getByText("Summarize the week.")).toBeTruthy();
  });

  it("creates a prompt through the editor", async () => {
    mockList.mockResolvedValue({ prompts: [] });
    mockCreate.mockResolvedValue({ prompt: P1 });
    renderTab();
    await waitFor(() => expect(screen.getByText(/No saved prompts yet/)).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "New prompt" }));
    fireEvent.change(screen.getByLabelText("Prompt name"), { target: { value: "Weekly summary" } });
    fireEvent.change(screen.getByLabelText("Prompt content"), { target: { value: "Summarize the week." } });
    fireEvent.click(screen.getByText("Create"));

    await waitFor(() => expect(mockCreate).toHaveBeenCalledWith({ name: "Weekly summary", content: "Summarize the week." }));
    await waitFor(() => expect(screen.getByText("Weekly summary")).toBeTruthy());
  });

  it("surfaces the name-taken conflict as a friendly row error", async () => {
    mockList.mockResolvedValue({ prompts: [] });
    mockCreate.mockRejectedValue(new Error('409 {"error":"prompt_name_taken"}'));
    renderTab();
    await waitFor(() => expect(screen.getByText(/No saved prompts yet/)).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "New prompt" }));
    fireEvent.change(screen.getByLabelText("Prompt name"), { target: { value: "dup" } });
    fireEvent.change(screen.getByLabelText("Prompt content"), { target: { value: "x" } });
    fireEvent.click(screen.getByText("Create"));

    await waitFor(() =>
      expect(screen.getByText("A prompt with that name already exists.")).toBeTruthy(),
    );
    expect(screen.getByTestId("prompt-editor")).toBeTruthy();
  });

  it("edits an existing prompt in place", async () => {
    mockList.mockResolvedValue({ prompts: [P1] });
    mockUpdate.mockResolvedValue({ prompt: { ...P1, name: "Renamed" } });
    renderTab();
    await waitFor(() => expect(screen.getByText("Weekly summary")).toBeTruthy());

    fireEvent.click(screen.getByLabelText("Edit Weekly summary"));
    fireEvent.change(screen.getByLabelText("Prompt name"), { target: { value: "Renamed" } });
    fireEvent.click(screen.getByText("Save"));

    await waitFor(() => expect(mockUpdate).toHaveBeenCalledWith("p1", { name: "Renamed", content: "Summarize the week." }));
    await waitFor(() => expect(screen.getByText("Renamed")).toBeTruthy());
  });

  it("deletes only after confirmation", async () => {
    mockList.mockResolvedValue({ prompts: [P1] });
    renderTab();
    await waitFor(() => expect(screen.getByText("Weekly summary")).toBeTruthy());

    fireEvent.click(screen.getByLabelText("Delete Weekly summary"));
    expect(mockDelete).not.toHaveBeenCalled();
    fireEvent.click(screen.getByText("Delete", { selector: "button.bg-destructive" }));

    await waitFor(() => expect(mockDelete).toHaveBeenCalledWith("p1"));
    await waitFor(() => expect(screen.queryByText("Weekly summary")).toBeNull());
  });
});
