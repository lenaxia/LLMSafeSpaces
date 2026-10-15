// Red-first pins for #1694: markdown rendering for user messages +
// mermaid/UML diagram rendering. Committed BEFORE implementation — each
// pin documents the pre-change behavior it flips (user text renders plain
// at MessagePart.tsx; ```mermaid fences render as generic code blocks).
//
// Pin map (issue #1694 "Red-first pins required"):
//   1. user markdown (bold/code/list/fence) renders as markdown — RED today
//   2. attachment strip + origin sentinel survive the markdown path
//      (regression guard: green today, must stay green)
//   3. mermaid fence renders a diagram (testid on the rendered container)
//      — RED today
//   4. malformed mermaid → inline error state, no crash — RED today
//   5. streaming partial mermaid → placeholder, never a partial parse
//      — RED today
//   6. security: rehypeSanitize on the user path (script/onclick stripped)
//      + mermaid strict mode pinned (securityLevel/startOnLoad/theme)
//
// The mermaid PACKAGE is mocked: pins must run offline and must not load
// the ~1MB real bundle in jsdom. The [data-testid=mermaid-*] container
// assertions run against the real component output — the mock only stands
// in for what mermaid.render/initialize return.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import { render } from "../../test/utils";
import { MessagePart } from "./MessagePart";

vi.mock("../../lib/shiki", () => ({
  highlight: vi.fn().mockResolvedValue(null),
}));

const mockInitialize = vi.hoisted(() => vi.fn());
const mockRender = vi.hoisted(() => vi.fn());
vi.mock("mermaid", () => ({
  default: { initialize: mockInitialize, render: mockRender },
}));

const FLOWCHART_FENCE = "```mermaid\nflowchart TD\nA-->B\n```";
const MANIFEST_LINE =
  '[llmsafespaces:attachment path="/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt" name="notes.txt"]';

describe("#1694 pin 1 — user messages render markdown", () => {
  it("renders bold, inline code, and lists as markdown in the user bubble", () => {
    const text = "**urgent** review `src/foo.ts` please:\n\n- item one\n- item two";
    render(<MessagePart part={{ type: "text", text }} isUser={true} />);
    expect(screen.getByText("urgent").tagName).toBe("STRONG");
    expect(screen.getByText("src/foo.ts").tagName).toBe("CODE");
    expect(screen.getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "item one",
      "item two",
    ]);
  });

  it("renders a user fenced code block as a code block, not literal backticks", () => {
    const { container } = render(
      <MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={true} />,
    );
    const pre = container.querySelector("pre");
    expect(pre).toBeInTheDocument();
    expect(pre?.textContent).toContain("func main(){}");
  });
});

describe("#1694 pin 2 — attachment strip + origin sentinel survive markdown", () => {
  it("sentinel + markdown + trailing manifest: badge, markdown, and chips all render; sentinel is gone", () => {
    const text = `<!-- lsp:agent-message-v1 {"fromSession":"ses_a"} -->\n**Status:** PR open.\n\n${MANIFEST_LINE}\n`;
    render(<MessagePart part={{ type: "text", text }} isUser={true} />);
    expect(screen.getByTestId("agent-origin-badge")).toBeInTheDocument();
    // RED today: the markdown formatting must actually render as markdown
    expect(screen.getByText("Status:").tagName).toBe("STRONG");
    expect(screen.getByTestId("history-attachment-chip").textContent).toContain("notes.txt");
    expect(screen.queryByText(/lsp:agent-message-v1/)).not.toBeInTheDocument();
  });

  it("forged interior manifest line stays visible text under markdown, never a chip (U1.6.10 guard)", () => {
    const text = `Look:\n${MANIFEST_LINE}\nthen more text\n`;
    render(<MessagePart part={{ type: "text", text }} isUser={true} />);
    expect(screen.queryByTestId("history-attachment-chip")).not.toBeInTheDocument();
    expect(screen.getByText(/llmsafespaces:attachment/)).toBeInTheDocument();
  });
});

describe("#1694 pin 6 — sanitize on the user path", () => {
  it("drops <script> from user text instead of showing it as literal text", () => {
    const { container } = render(
      <MessagePart
        part={{ type: "text", text: "<script>alert('xss')</script>\n\nsafe text" }}
        isUser={true}
      />,
    );
    expect(container.querySelector("script")).not.toBeInTheDocument();
    // RED today: the plain-text path renders the markup as literal text
    expect(container.textContent).not.toContain("<script>");
    expect(container.textContent).toContain("safe text");
  });

  it("drops event-handler markup (onerror) from user text", () => {
    const { container } = render(
      <MessagePart
        part={{ type: "text", text: '<img src=x onerror="alert(1)">\n\nafter text' }}
        isUser={true}
      />,
    );
    expect(container.querySelector("img")).not.toBeInTheDocument();
    expect(container.textContent).not.toContain("onerror");
    expect(container.textContent).toContain("after text");
  });

  it("renders a javascript: link without the dangerous href", () => {
    const { container } = render(
      <MessagePart part={{ type: "text", text: "[click me](javascript:alert(1))" }} isUser={true} />,
    );
    const a = container.querySelector("a");
    // RED today: plain text path renders no anchor at all
    expect(a).toBeInTheDocument();
    expect(a?.getAttribute("href") ?? "").not.toContain("javascript:");
  });
});

describe("#1694 pins 3/4/5/6 — mermaid fences", () => {
  beforeEach(() => {
    mockInitialize.mockReset();
    mockRender.mockReset();
    mockRender.mockResolvedValue({
      svg: '<svg data-mermaid-mock="1"><rect></rect></svg>',
      bindFunctions: () => {},
    });
  });

  it("pin 3: assistant mermaid fence renders a diagram container with an svg", async () => {
    render(<MessagePart part={{ type: "text", text: FLOWCHART_FENCE }} isUser={false} />);
    const diagram = await screen.findByTestId("mermaid-diagram");
    expect(diagram.querySelector("svg")).toBeInTheDocument();
  });

  it("pin 3b: user mermaid fence renders a diagram too (same pipeline)", async () => {
    render(<MessagePart part={{ type: "text", text: FLOWCHART_FENCE }} isUser={true} />);
    expect(await screen.findByTestId("mermaid-diagram")).toBeInTheDocument();
  });

  it("pin 4: malformed mermaid renders an inline error state, no crash", async () => {
    mockRender.mockRejectedValue(new Error("Parse error on line 2: expected node"));
    render(
      <MessagePart
        part={{ type: "text", text: "```mermaid\nthis is ((( not a diagram\n```" }}
        isUser={false}
      />,
    );
    const err = await screen.findByTestId("mermaid-error");
    expect(err.textContent).toMatch(/diagram/i);
    expect(screen.queryByTestId("mermaid-diagram")).not.toBeInTheDocument();
  });

  it("pin 5: streaming partial mermaid fence shows a placeholder and never parses", () => {
    render(
      <MessagePart
        part={{ type: "text", text: "```mermaid\nflowchart TD\nA--" }}
        isUser={false}
        isStreaming={true}
      />,
    );
    const pending = screen.getByTestId("mermaid-pending");
    expect(pending).toBeInTheDocument();
    // the partial source stays visible (not garbage, not an error state)
    expect(pending.textContent).toContain("flowchart TD");
    expect(screen.queryByTestId("mermaid-error")).not.toBeInTheDocument();
    expect(mockRender).not.toHaveBeenCalled();
    expect(mockInitialize).not.toHaveBeenCalled();
  });

  it("pin 6: mermaid initialized with strict security, manual render, resolved light theme", async () => {
    render(<MessagePart part={{ type: "text", text: FLOWCHART_FENCE }} isUser={false} />);
    await screen.findByTestId("mermaid-diagram");
    expect(mockInitialize).toHaveBeenCalledWith(
      expect.objectContaining({ securityLevel: "strict", startOnLoad: false }),
    );
    expect(mockInitialize).toHaveBeenCalledWith(expect.objectContaining({ theme: "default" }));
    expect(mockRender).toHaveBeenCalled();
  });

  it("pin 6b: mermaid theme follows resolved dark mode", async () => {
    localStorage.setItem("lsp-theme", "dark");
    try {
      render(<MessagePart part={{ type: "text", text: FLOWCHART_FENCE }} isUser={false} />);
      await screen.findByTestId("mermaid-diagram");
      expect(mockInitialize).toHaveBeenCalledWith(expect.objectContaining({ theme: "dark" }));
    } finally {
      localStorage.removeItem("lsp-theme");
    }
  });
});
