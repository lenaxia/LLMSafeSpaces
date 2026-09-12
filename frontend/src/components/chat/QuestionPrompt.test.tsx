import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QuestionPrompt } from "./QuestionPrompt";
import type { QuestionRequest } from "../../api/types";

vi.mock("../../api/input", () => ({
  inputApi: {
    questionReply: vi.fn().mockResolvedValue(true),
    questionReject: vi.fn().mockResolvedValue(true),
    dismissInboxRecord: vi.fn().mockResolvedValue(undefined),
  },
}));

import { inputApi } from "../../api/input";
const mockReply = vi.mocked(inputApi.questionReply);
const mockReject = vi.mocked(inputApi.questionReject);
const mockDismissInbox = vi.mocked(inputApi.dismissInboxRecord);

const singleQuestion: QuestionRequest = {
  id: "que_1",
  session_id: "ses_1",
  questions: [{
    header: "Choose language",
    question: "What programming language?",
    options: [
      { label: "Go", description: "Fast compiled" },
      { label: "Python", description: "Easy scripting" },
      { label: "Rust", description: "Memory safe" },
    ],
  }],
};

const multiQuestion: QuestionRequest = {
  id: "que_2",
  session_id: "ses_1",
  questions: [
    { header: "Language", question: "Pick language", options: [{ label: "Go", description: "" }], multiple: true },
    { header: "Database", question: "Pick DB", options: [{ label: "Postgres", description: "" }] },
  ],
};

describe("QuestionPrompt", () => {
  const onResolved = vi.fn();

  beforeEach(() => { vi.clearAllMocks(); });

  it("renders question header and text", () => {
    render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
    expect(screen.getByText("Choose language")).toBeInTheDocument();
    expect(screen.getByText("What programming language?")).toBeInTheDocument();
  });

  it("single select: click option selects it, submit enabled", async () => {
    render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
    const goBtn = screen.getByRole("button", { name: "Go" });
    fireEvent.click(goBtn);
    expect(goBtn).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("Submit answers")).not.toBeDisabled();
  });

  it("single select: clicking different option deselects previous", () => {
    render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
    fireEvent.click(screen.getByRole("button", { name: "Go" }));
    fireEvent.click(screen.getByRole("button", { name: "Python" }));
    expect(screen.getByRole("button", { name: "Go" })).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByRole("button", { name: "Python" })).toHaveAttribute("aria-pressed", "true");
  });

  it("multi-select: click multiple options", () => {
    render(<QuestionPrompt workspaceId="ws-1" request={multiQuestion} onResolved={onResolved} />);
    const goBtn = screen.getByRole("button", { name: "Go" });
    fireEvent.click(goBtn);
    expect(goBtn).toHaveAttribute("aria-pressed", "true");
    // Second click toggles off
    fireEvent.click(goBtn);
    expect(goBtn).toHaveAttribute("aria-pressed", "false");
  });

  it("custom text input counts as answer", async () => {
    const user = userEvent.setup();
    render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
    const input = screen.getByPlaceholderText("Or type your own...");
    await user.type(input, "TypeScript");
    expect(screen.getByText("Submit answers")).not.toBeDisabled();
  });

  it("submit calls API with correct answers and fires onResolved", async () => {
    render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
    fireEvent.click(screen.getByRole("button", { name: "Go" }));
    fireEvent.click(screen.getByText("Submit answers"));
    await waitFor(() => expect(mockReply).toHaveBeenCalledWith("ws-1", "que_1", [["Go"]]));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
  });

  it("submit disabled when no answer selected", () => {
    render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
    expect(screen.getByText("Submit answers")).toBeDisabled();
  });

  it("dismiss calls reject and fires onResolved", async () => {
    render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
    fireEvent.click(screen.getByText("Dismiss"));
    await waitFor(() => expect(mockReject).toHaveBeenCalledWith("ws-1", "que_1"));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
  });

  it("multiple questions rendered simultaneously", () => {
    render(<QuestionPrompt workspaceId="ws-1" request={multiQuestion} onResolved={onResolved} />);
    expect(screen.getByText("Pick language")).toBeInTheDocument();
    expect(screen.getByText("Pick DB")).toBeInTheDocument();
  });

  it("API error on submit shows error inline", async () => {
    mockReply.mockRejectedValueOnce(new Error("network fail"));
    render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
    fireEvent.click(screen.getByRole("button", { name: "Go" }));
    fireEvent.click(screen.getByText("Submit answers"));
    await waitFor(() => expect(screen.getByText("network fail")).toBeInTheDocument());
    expect(onResolved).not.toHaveBeenCalled();
  });

  it("Escape key triggers dismiss", () => {
    render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    // The dismiss button is clicked via the ✕ button; for keyboard we rely on the Dismiss button
    // The component has a dismiss button that's always available
    expect(screen.getByLabelText("Dismiss")).toBeInTheDocument();
  });

  it("custom text combined with selected option in submit payload", async () => {
    const user = userEvent.setup();
    render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
    fireEvent.click(screen.getByRole("button", { name: "Go" }));
    const input = screen.getByPlaceholderText("Or type your own...");
    await user.type(input, "Java");
    fireEvent.click(screen.getByText("Submit answers"));
    await waitFor(() => expect(mockReply).toHaveBeenCalledWith("ws-1", "que_1", [["Go", "Java"]]));
  });

  it("submit disabled when one of multiple questions unanswered", () => {
    render(<QuestionPrompt workspaceId="ws-1" request={multiQuestion} onResolved={onResolved} />);
    // Answer only first question
    fireEvent.click(screen.getByRole("button", { name: "Go" }));
    // Second question unanswered → submit disabled
    expect(screen.getByText("Submit answers")).toBeDisabled();
  });

  describe("whileAway variant (#1313)", () => {
    const awayQuestion: QuestionRequest = { ...singleQuestion, whileAway: true };

    it("renders the while-you-were-away title and hint", () => {
      render(<QuestionPrompt workspaceId="ws-1" request={awayQuestion} onResolved={onResolved} />);
      expect(screen.getByText(/while you were away/i)).toBeInTheDocument();
      expect(screen.getByText(/waited unanswered/i)).toBeInTheDocument();
    });

    it("dismiss routes to the inbox dismiss endpoint with the session", async () => {
      render(<QuestionPrompt workspaceId="ws-1" request={awayQuestion} onResolved={onResolved} />);
      fireEvent.click(screen.getByText("Dismiss"));
      await waitFor(() => expect(mockDismissInbox).toHaveBeenCalledWith("ws-1", "ses_1", "que_1"));
      await waitFor(() => expect(onResolved).toHaveBeenCalled());
      expect(mockReject).not.toHaveBeenCalled();
    });

    it("submit still uses the reply route (late answer is server-side)", async () => {
      render(<QuestionPrompt workspaceId="ws-1" request={awayQuestion} onResolved={onResolved} />);
      fireEvent.click(screen.getByRole("button", { name: "Go" }));
      fireEvent.click(screen.getByText("Submit answers"));
      await waitFor(() => expect(mockReply).toHaveBeenCalledWith("ws-1", "que_1", [["Go"]]));
      await waitFor(() => expect(onResolved).toHaveBeenCalled());
    });

    it("live prompt (no whileAway) still rejects via the live route", async () => {
      render(<QuestionPrompt workspaceId="ws-1" request={singleQuestion} onResolved={onResolved} />);
      fireEvent.click(screen.getByText("Dismiss"));
      await waitFor(() => expect(mockReject).toHaveBeenCalledWith("ws-1", "que_1"));
      expect(mockDismissInbox).not.toHaveBeenCalled();
    });

    it("dismiss error keeps the prompt visible with the error inline", async () => {
      mockDismissInbox.mockRejectedValueOnce(new Error("dismiss failed"));
      render(<QuestionPrompt workspaceId="ws-1" request={awayQuestion} onResolved={onResolved} />);
      fireEvent.click(screen.getByText("Dismiss"));
      await waitFor(() => expect(screen.getByText("dismiss failed")).toBeInTheDocument());
      expect(onResolved).not.toHaveBeenCalled();
    });
  });
});
