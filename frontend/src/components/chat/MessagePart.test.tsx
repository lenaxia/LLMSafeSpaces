import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { MessagePart, closeOpenFence } from "./MessagePart";

// Env mock for DevPreviewOutput's relative-link resolution. Top-level so
// vitest hoists it; mutable so the relative-link test can pin an absolute
// API base and everything else keeps the same-origin default.
const testEnvState = { apiBaseUrl: "/api/v1" };
vi.mock("../../env", () => ({
  getEnv: () => ({ apiBaseUrl: testEnvState.apiBaseUrl, turnstileSiteKey: "" }),
  loadEnv: async () => ({ apiBaseUrl: testEnvState.apiBaseUrl, turnstileSiteKey: "" }),
}));
afterEach(() => { testEnvState.apiBaseUrl = "/api/v1"; });
import { highlight } from "../../lib/shiki";

vi.mock("../../lib/shiki", () => ({
  highlight: vi.fn().mockResolvedValue(null),
}));
const mockHighlight = highlight as Mock;

describe("MessagePart", () => {
  let consoleSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    mockHighlight.mockResolvedValue(null);
    consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});
  });

  afterEach(() => {
    consoleSpy.mockRestore();
  });

  it("renders user text as plain paragraph", () => {
    render(<MessagePart part={{ type: "text", text: "Hello world" }} isUser={true} />);
    const p = screen.getByText("Hello world");
    expect(p.tagName).toBe("P");
  });

  it("renders assistant text as markdown", () => {
    render(<MessagePart part={{ type: "text", text: "**bold**" }} isUser={false} />);
    expect(screen.getByText("bold")).toBeInTheDocument();
    expect(screen.getByText("bold").tagName).toBe("STRONG");
  });

  it("renders nothing for unknown part type", () => {
    const { container } = render(<MessagePart part={{ type: "image" }} isUser={false} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders nothing when text is empty", () => {
    const { container } = render(<MessagePart part={{ type: "text", text: "" }} isUser={true} />);
    expect(container.innerHTML).toBe("");
  });

  it("sanitizes dangerous HTML in assistant messages", () => {
    render(<MessagePart part={{ type: "text", text: "<script>alert('xss')</script>\n\nsafe text" }} isUser={false} />);
    expect(screen.queryByText("alert('xss')")).not.toBeInTheDocument();
  });

  it("renders GFM tables", () => {
    const table = "| Col A | Col B |\n|-------|-------|\n| 1     | 2     |\n| 3     | 4     |";
    render(<MessagePart part={{ type: "text", text: table }} isUser={false} />);
    expect(screen.getByRole("table")).toBeInTheDocument();
    expect(screen.getByText("Col A")).toBeInTheDocument();
    expect(screen.getByText("4")).toBeInTheDocument();
  });

  it("renders fenced code blocks", async () => {
    const code = "```js\nconst x = 1;\n```";
    render(<MessagePart part={{ type: "text", text: code }} isUser={false} />);
    await waitFor(() => {
      expect(screen.getByText("const x = 1;")).toBeInTheDocument();
      const codeEl = screen.getByText("const x = 1;").closest("code");
      expect(codeEl).toBeInTheDocument();
    });
  });

  it("renders code block with react-markdown string children correctly", async () => {
    const code = "```js\nconst x = 1;\n```";
    const { container } = render(<MessagePart part={{ type: "text", text: code }} isUser={false} />);
    await waitFor(() => {
      const pre = container.querySelector("pre");
      expect(pre).toBeInTheDocument();
      expect(screen.getByText("const x = 1;")).toBeInTheDocument();
    });
  });

  it("renders tool_result with empty text as empty container", () => {
    const { container } = render(<MessagePart part={{ type: "tool_result", text: "" }} isUser={false} />);
    expect(container.querySelector("pre")).toBeInTheDocument();
  });

  it("renders tool_result with undefined text as null", () => {
    const { container } = render(<MessagePart part={{ type: "tool_result", text: undefined }} isUser={false} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders inline code", () => {
    render(<MessagePart part={{ type: "text", text: "Use `npm install` to install" }} isUser={false} />);
    const codeEl = screen.getByText("npm install");
    expect(codeEl.tagName).toBe("CODE");
  });

  it("renders strikethrough (GFM)", () => {
    render(<MessagePart part={{ type: "text", text: "~~deleted~~" }} isUser={false} />);
    const del = screen.getByText("deleted");
    expect(del.tagName).toBe("DEL");
  });

  describe("link rendering — open in new tab", () => {
    it("renders assistant-text links with target=_blank", () => {
      const { container } = render(
        <MessagePart part={{ type: "text", text: "[example](https://example.com)" }} isUser={false} />,
      );
      const link = container.querySelector("a");
      expect(link).not.toBeNull();
      expect(link?.getAttribute("href")).toBe("https://example.com");
      expect(link?.getAttribute("target")).toBe("_blank");
    });

    it("renders assistant-text links with rel=noopener noreferrer", () => {
      const { container } = render(
        <MessagePart part={{ type: "text", text: "[example](https://example.com)" }} isUser={false} />,
      );
      const link = container.querySelector("a");
      expect(link?.getAttribute("rel")).toContain("noopener");
      expect(link?.getAttribute("rel")).toContain("noreferrer");
    });

    it("renders thinking-part links with target=_blank", () => {
      const { container } = render(
        <MessagePart part={{ type: "thinking", text: "[docs](https://docs.example.com)" }} isUser={false} />,
      );
      const link = container.querySelector("a");
      expect(link).not.toBeNull();
      expect(link?.getAttribute("target")).toBe("_blank");
      expect(link?.getAttribute("rel")).toContain("noopener");
    });

    it("preserves href on external links", () => {
      const { container } = render(
        <MessagePart part={{ type: "text", text: "See <https://auto-link.example.com>" }} isUser={false} />,
      );
      const link = container.querySelector("a");
      expect(link?.getAttribute("href")).toBe("https://auto-link.example.com");
      expect(link?.getAttribute("target")).toBe("_blank");
    });
  });

  it("renders code block containing HTML-special characters safely", async () => {
    const md = '```html\n<div class="xss">hello</div>\n```';
    render(<MessagePart part={{ type: "text", text: md }} isUser={false} />);
    await waitFor(() => {
      const code = screen.getByText('<div class="xss">hello</div>');
      expect(code).toBeInTheDocument();
      expect(code.tagName).toBe("CODE");
    });
  });

  it("renders thinking part with collapsible details", () => {
    render(<MessagePart part={{ type: "thinking", text: "Let me reason about this" }} isUser={false} />);
    expect(screen.getByText("Thinking")).toBeInTheDocument();
    expect(screen.getByText("Let me reason about this")).toBeInTheDocument();
  });

  it("renders tool_call part", () => {
    render(<MessagePart part={{ type: "tool_call", text: "search" }} isUser={false} />);
    expect(screen.getByText(/search/)).toBeInTheDocument();
  });

  it("renders tool_use part with name and input", () => {
    render(<MessagePart part={{ type: "tool_use", name: "read_file", input: { path: "/foo" } }} isUser={false} />);
    expect(screen.getByText(/read_file/)).toBeInTheDocument();
  });

  it("renders tool_result part", () => {
    render(<MessagePart part={{ type: "tool_result", text: "Found 3 results" }} isUser={false} />);
    expect(screen.getByText("Tool result")).toBeInTheDocument();
    expect(screen.getByText("Found 3 results")).toBeInTheDocument();
  });

  it("renders tool_use part with empty text during streaming", () => {
    render(<MessagePart part={{ type: "tool_use", text: "" }} isUser={false} isStreaming={true} />);
    expect(screen.getByText(/tool/)).toBeInTheDocument();
  });

  it("renders tool_use part with empty text when not streaming", () => {
    render(<MessagePart part={{ type: "tool_use", text: "" }} isUser={false} isStreaming={false} />);
    expect(screen.getByText(/tool/)).toBeInTheDocument();
  });

  describe("overflow containment", () => {
    it("applies overflow-x-auto to tables via prose selector", () => {
      const table = "| A | B |\n|---|---|\n| 1 | 2 |";
      const { container } = render(
        <MessagePart part={{ type: "text", text: table }} isUser={false} />,
      );
      const prose = container.querySelector(".prose");
      expect(prose?.className).toContain("[&_table]:overflow-x-auto");
    });
  });

  describe("codeBlockWordWrap setting", () => {
    const STORAGE_KEY = "llmsafespaces_user_settings";
    const codeMarkdown = "```js\nconst x = 1;\n```";

    it("does not apply word-wrap classes when setting is false", async () => {
      localStorage.setItem(STORAGE_KEY, JSON.stringify({ codeBlockWordWrap: false }));
      const { _resetStoreFromStorage } = await import("../../hooks/useUserSettings");
      _resetStoreFromStorage();
      render(
        <MessagePart part={{ type: "text", text: codeMarkdown }} isUser={false} />,
      );
      await waitFor(() => {
        const pre = document.querySelector("pre");
        expect(pre?.className).not.toContain("whitespace-pre-wrap");
      });
    });

    it("applies word-wrap classes when setting is true", async () => {
      localStorage.setItem(STORAGE_KEY, JSON.stringify({ codeBlockWordWrap: true }));
      const { _resetStoreFromStorage } = await import("../../hooks/useUserSettings");
      _resetStoreFromStorage();
      render(
        <MessagePart part={{ type: "text", text: codeMarkdown }} isUser={false} />,
      );
      await waitFor(() => {
        const pre = document.querySelector("pre");
        expect(pre?.className).toContain("whitespace-pre-wrap");
      });
    });

    it("defaults to no word-wrap when setting is absent", async () => {
      localStorage.removeItem(STORAGE_KEY);
      const { _resetStoreFromStorage } = await import("../../hooks/useUserSettings");
      _resetStoreFromStorage();
      render(
        <MessagePart part={{ type: "text", text: codeMarkdown }} isUser={false} />,
      );
      await waitFor(() => {
        const pre = document.querySelector("pre");
        expect(pre?.className).not.toContain("whitespace-pre-wrap");
      });
    });
  });
});

describe("closeOpenFence", () => {
  it("returns unchanged text when no fences are present", () => {
    expect(closeOpenFence("no code here")).toBe("no code here");
  });

  it("returns unchanged text when fences are balanced", () => {
    const text = "```go\nfunc main(){}\n```";
    expect(closeOpenFence(text)).toBe(text);
  });

  it("closes an open 3-backtick fence", () => {
    expect(closeOpenFence("```go\nfunc main(){}")).toBe("```go\nfunc main(){}\n```");
  });

  it("closes an open 4-backtick fence with 4 backticks, not 3", () => {
    expect(closeOpenFence("````go\nfunc main(){}")).toBe("````go\nfunc main(){}\n````");
  });

  it("closes an open tilde fence", () => {
    expect(closeOpenFence("~~~python\nprint('hi')")).toBe("~~~python\nprint('hi')\n~~~");
  });

  it("closes an open 4-tilde fence", () => {
    expect(closeOpenFence("~~~~sh\necho hello")).toBe("~~~~sh\necho hello\n~~~~");
  });

  it("does not close a fence with mismatched character (backtick vs tilde)", () => {
    const text = "~~~\ncode\n```";
    const result = closeOpenFence(text);
    expect(result).toBe("~~~\ncode\n```\n~~~");
  });

  it("does not close with shorter fence than opening", () => {
    const text = "````\ncode\n```\nmore";
    const result = closeOpenFence(text);
    expect(result).toBe("````\ncode\n```\nmore\n````");
  });

  it("handles multiple balanced fences correctly", () => {
    const text = "```go\ncode1\n```\n```py\ncode2\n```";
    expect(closeOpenFence(text)).toBe(text);
  });

  it("closes the second open fence in a mixed sequence", () => {
    const text = "```go\ncode1\n```\ntext\n```py\ncode2";
    expect(closeOpenFence(text)).toBe("```go\ncode1\n```\ntext\n```py\ncode2\n```");
  });

  it("handles empty string", () => {
    expect(closeOpenFence("")).toBe("");
  });

  it("handles text with only a fence opening and no newline", () => {
    expect(closeOpenFence("```")).toBe("```\n```");
  });

  it("handles fence with no language info string", () => {
    expect(closeOpenFence("```\ncode")).toBe("```\ncode\n```");
  });

  it("handles CRLF line endings", () => {
    // CRLF is normalized to LF via replace(/\r\n?/g, "\n")
    expect(closeOpenFence("```go\r\nfunc main(){}"))
      .toBe("```go\nfunc main(){}\n```");
  });

  it("normalizes CR line endings", () => {
    expect(closeOpenFence("```\rcode"))
      .toBe("```\ncode\n```");
  });

  it("handles indented fence (3 leading spaces)", () => {
    expect(closeOpenFence("   ```go\nfunc main(){}"))
      .toBe("   ```go\nfunc main(){}\n   ```");
  });

  it("handles indented fence (1 leading space)", () => {
    expect(closeOpenFence(" ```\ncode"))
      .toBe(" ```\ncode\n ```");
  });

  it("does not treat 4+ space indent as fence", () => {
    const text = "    ```go\ncode";
    expect(closeOpenFence(text)).toBe(text);
  });

  it("handles balanced indented fences (2 spaces)", () => {
    const text = "  ```go\ncode\n  ```";
    expect(closeOpenFence(text)).toBe(text);
  });
});

describe("CodeBlock (via MessagePart)", () => {
  let consoleSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    mockHighlight.mockReset();
    mockHighlight.mockResolvedValue(null);
    consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});
  });

  afterEach(() => {
    consoleSpy.mockRestore();
  });

  it("renders language label when language is present", async () => {
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} />);
    await waitFor(() => expect(screen.getByText("go")).toBeInTheDocument());
  });

  it("does not render header bar for unlabelled fence", async () => {
    render(<MessagePart part={{ type: "text", text: "```\ncode\n```" }} isUser={false} />);
    await waitFor(() => {
      expect(screen.queryByRole("button", { name: /copy code/i })).not.toBeInTheDocument();
    });
  });

  it("renders copy button with accessible label when language is present", async () => {
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} />);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /copy code/i })).toBeInTheDocument()
    );
  });

  it("does not call highlight() while isStreaming=true", () => {
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} isStreaming={true} />);
    expect(mockHighlight).not.toHaveBeenCalled();
  });

  it("calls highlight() when isStreaming is false", async () => {
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} isStreaming={false} />);
    await waitFor(() => expect(mockHighlight).toHaveBeenCalledWith("func main(){}", "go"));
  });

  it("renders shiki HTML when highlight() returns HTML", async () => {
    mockHighlight.mockResolvedValue('<pre class="shiki"><code><span style="color:#333">func</span></code></pre>');
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} />);
    await waitFor(() => expect(document.querySelector(".shiki")).toBeInTheDocument());
  });

  it("renders plain pre fallback when highlight() returns null", async () => {
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} />);
    await waitFor(() => {
      const pre = document.querySelector("pre");
      expect(pre).toBeInTheDocument();
      expect(pre?.textContent).toContain("func main(){}");
    });
  });

  it("renders plain pre fallback when highlight() rejects", async () => {
    mockHighlight.mockRejectedValue(new Error("shiki init failed"));
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} />);
    await waitFor(() => {
      const pre = document.querySelector("pre");
      expect(pre).toBeInTheDocument();
      expect(pre?.textContent).toContain("func main(){}");
    });
  });

  it("copy button copies raw code", async () => {
    const user = userEvent.setup();
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} />);
    await waitFor(() => screen.getByRole("button", { name: /copy code/i }));
    await user.click(screen.getByRole("button", { name: /copy code/i }));
    // userEvent handles clipboard internally; verify the copied state transition
    await waitFor(() => expect(screen.getByRole("button", { name: /copied/i })).toBeInTheDocument());
  });

  it("copy button shows check icon after copy", async () => {
    const user = userEvent.setup();
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} />);
    await waitFor(() => screen.getByRole("button", { name: /copy code/i }));
    await user.click(screen.getByRole("button", { name: /copy code/i }));
    await waitFor(() => expect(screen.getByRole("button", { name: /copied/i })).toBeInTheDocument());
  });

  it("copy button stays in idle state on clipboard failure", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    const writeText = vi.fn().mockRejectedValue(new Error("denied"));
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
    });
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} />);
    await waitFor(() => screen.getByRole("button", { name: /copy code/i }));
    await user.click(screen.getByRole("button", { name: /copy code/i }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("func main(){}"));
    expect(screen.queryByRole("button", { name: /copied/i })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /copy code/i })).toBeInTheDocument();
    vi.useRealTimers();
  });

  it("copy button reverts after 2s", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<MessagePart part={{ type: "text", text: "```go\nfunc main(){}\n```" }} isUser={false} />);
    await waitFor(() => screen.getByRole("button", { name: /copy code/i }));
    await user.click(screen.getByRole("button", { name: /copy code/i }));
    await waitFor(() => expect(screen.getByRole("button", { name: /copied/i })).toBeInTheDocument());
    vi.advanceTimersByTime(2100);
    await waitFor(() => expect(screen.getByRole("button", { name: /copy code/i })).toBeInTheDocument());
    vi.useRealTimers();
  });

  it("calls highlight() exactly once when isStreaming transitions to false", async () => {
    const { rerender } = render(<MessagePart
      part={{ type: "text", text: "```go\nfunc main(){}\n```" }}
      isUser={false}
      isStreaming={true}
    />);
    expect(mockHighlight).not.toHaveBeenCalled();

    rerender(<MessagePart
      part={{ type: "text", text: "```go\nfunc main(){}\n```" }}
      isUser={false}
      isStreaming={false}
    />);
    await waitFor(() => expect(mockHighlight).toHaveBeenCalledTimes(1), { timeout: 10000 });
    expect(mockHighlight).toHaveBeenCalledWith("func main(){}", "go");
  });

  it("cancels stale highlight when isStreaming toggles true mid-flight", () => new Promise<void>((done) => {
    let resolveHighlight: (html: string | null) => void;
    const delayPromise = new Promise<string | null>((resolve) => {
      resolveHighlight = resolve;
    });
    mockHighlight.mockReturnValue(delayPromise);

    const { rerender } = render(<MessagePart
      part={{ type: "text", text: "```go\nfunc main(){}\n```" }}
      isUser={false}
      isStreaming={false}
    />);

    // Wait for useEffect to fire and call highlight().
    waitFor(() => expect(mockHighlight).toHaveBeenCalledTimes(1)).then(() => {
      // Toggle streaming true mid-flight — the cancelled flag prevents stale update.
      rerender(<MessagePart
        part={{ type: "text", text: "```go\nfunc main(){}\n```" }}
        isUser={false}
        isStreaming={true}
      />);

      // Resolve the stale highlight with HTML.
      resolveHighlight!('<pre class="shiki"><code><span>func</span></code></pre>');

      // After microtask flush, the stale highlight should NOT have been applied.
      setTimeout(() => {
        expect(document.querySelector(".shiki")).toBeNull();
        expect(document.querySelector("pre")).toBeInTheDocument();
        done();
      }, 0);
    });
  }));
});

describe("streaming fence + CodeBlock integration", () => {
  let consoleSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    mockHighlight.mockReset();
    mockHighlight.mockResolvedValue(null);
    consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});
  });

  afterEach(() => {
    consoleSpy.mockRestore();
  });

  it("renders a streaming code block as plain pre without calling highlight", () => {
    // Simulate mid-stream: unclosed fence, isStreaming=true.
    render(<MessagePart
      part={{ type: "text", text: "```go\nfunc main(){" }}
      isUser={false}
      isStreaming={true}
    />);
    // closeOpenFence appends closing ```, so the text becomes:
    // "```go\nfunc main(){\n```" — a complete code block.
    // CodeBlock should render but NOT call highlight() (streaming guard).
    expect(mockHighlight).not.toHaveBeenCalled();
    // The code content should still be visible as plain text.
    const pre = document.querySelector("pre");
    expect(pre).toBeInTheDocument();
    expect(pre?.textContent).toContain("func main(){");
  });

  it("closes streaming fence and highlights after streaming ends", async () => {
    const { rerender } = render(<MessagePart
      part={{ type: "text", text: "intro text\n```go\nfunc main(){" }}
      isUser={false}
      isStreaming={true}
    />);
    expect(mockHighlight).not.toHaveBeenCalled();

    // Streaming completes — the full text is already closed.
    rerender(<MessagePart
      part={{ type: "text", text: "intro text\n```go\nfunc main(){}\n```" }}
      isUser={false}
      isStreaming={false}
    />);
    await waitFor(() =>
      expect(mockHighlight).toHaveBeenCalledWith("func main(){}", "go")
    );
  });

  it("handles multiple streaming code blocks without calling highlight", () => {
    // Two unclosed fences — closeOpenFence closes the second one.
    render(<MessagePart
      part={{ type: "text", text: "```go\ncode1\n```\n```py\ncode2" }}
      isUser={false}
      isStreaming={true}
    />);
    expect(mockHighlight).not.toHaveBeenCalled();
  });

  it("renders tilde-fence code block during streaming correctly", () => {
    render(<MessagePart
      part={{ type: "text", text: "~~~py\ndef foo():" }}
      isUser={false}
      isStreaming={true}
    />);
    // closeOpenFence should append ~~~ to close the tilde fence.
    const pre = document.querySelector("pre");
    expect(pre).toBeInTheDocument();
    expect(pre?.textContent).toContain("def foo():");
    expect(mockHighlight).not.toHaveBeenCalled();
  });
});

// Design 0050 D5: running tools with a known start time show a live
// elapsed badge; an orphaned tool honestly shows hours — the visible
// signal that state is stale.
describe("MessagePart tool elapsed badge (#892 D5)", () => {
  it("renders elapsed badge on a running tool with startedAt", () => {
    const started = new Date(Date.now() - 42_000).toISOString();
    render(
      <MessagePart
        part={{ type: "tool_use", name: "bash", toolState: "running", toolStartedAt: started, input: { command: "sleep 100" } }}
        isUser={false}
      />,
    );
    expect(screen.getByLabelText("elapsed time").textContent).toMatch(/^4[12]s$/);
  });

  it("formats minutes and hours coarsely", () => {
    const started = new Date(Date.now() - 3 * 3600_000 - 5 * 60_000).toISOString();
    render(
      <MessagePart
        part={{ type: "tool_use", name: "bash", toolState: "running", toolStartedAt: started, input: { command: "sleep 720" } }}
        isUser={false}
      />,
    );
    expect(screen.getByLabelText("elapsed time").textContent).toMatch(/^3h (4|5)m$/);
  });

  it("renders no badge on a completed tool", () => {
    render(
      <MessagePart
        part={{ type: "tool_use", name: "bash", toolState: "completed", toolStartedAt: new Date().toISOString(), input: { command: "ls" } }}
        isUser={false}
      />,
    );
    expect(screen.queryByLabelText("elapsed time")).not.toBeInTheDocument();
  });

  it("renders no badge on a running tool without startedAt (older API)", () => {
    render(
      <MessagePart part={{ type: "tool_use", name: "bash", toolState: "running", input: { command: "ls" } }} isUser={false} />,
    );
    expect(screen.queryByLabelText("elapsed time")).not.toBeInTheDocument();
  });
});

describe("MessagePart tool elapsed badge edge cases (#892 D5)", () => {
  it("renders no badge when startedAt is an unparseable string (NaN guard)", () => {
    render(
      <MessagePart
        part={{ type: "tool_use", name: "bash", toolState: "running", toolStartedAt: "not-a-timestamp", input: { command: "ls" } }}
        isUser={false}
      />,
    );
    expect(screen.queryByLabelText("elapsed time")).not.toBeInTheDocument();
  });
});

describe("MessagePart tool elapsed badge tick growth (#892 D5)", () => {
  it("advances as the clock ticks while the tool runs", async () => {
    vi.useFakeTimers();
    try {
      const started = new Date(Date.now() - 1000).toISOString();
      render(
        <MessagePart
          part={{ type: "tool_use", name: "bash", toolState: "running", toolStartedAt: started, input: { command: "sleep 100" } }}
          isUser={false}
        />,
      );
      const badge = screen.getByLabelText("elapsed time");
      const first = badge.textContent;
      expect(first).toMatch(/^[12]s$/);
      // Advance 61s: the badge must honestly grow (fake timers drive
      // useNow's 1s interval).
      act(() => {
        vi.advanceTimersByTime(61_000);
      });
      expect(badge.textContent).toMatch(/^1m (0|1|2)s$/);
      expect(badge.textContent).not.toEqual(first);
    } finally {
      vi.useRealTimers();
    }
  });
});

// Dev-preview tool output renders as an action button (DEV_PREVIEW marker).
// Uses the file's existing provider-wrapped render + screen imports.
describe("DevPreviewOutput via MessagePart", () => {
  it("renders an open-preview button from the marker + markdown link", () => {
    const output = "LSP_DEV_PREVIEW_V1 port=5173 origin=safespaces.dev\n[Open dev preview :5173](https://api.example.com/api/v1/workspaces/ws/dev-preview-bootstrap/5173)\nOpens the preview. Requires login.";
    render(<MessagePart part={{ type: "tool_result", text: output, name: "dev_preview_url" } as never} isUser={false} />);
    const btn = screen.getByTestId("dev-preview-button");
    expect(btn).toHaveAttribute("href", "https://api.example.com/api/v1/workspaces/ws/dev-preview-bootstrap/5173");
    expect(btn).toHaveAttribute("target", "_blank");
    expect(btn.textContent).toContain("Open dev preview :5173");
  });

  it("button is visible WITHOUT expanding the tool pane (regression, 1eee7f28)", () => {
    // RED before 1eee7f28: dev_preview_url output rendered inside
    // ToolDetails → LazyDetails, which does not MOUNT children until the
    // pane is expanded — the button did not exist in the DOM unexpanded,
    // defeating the one-click UX. GREEN after: the tool_use branch
    // early-returns the always-visible DevPreviewToolCard.
    render(
      <MessagePart
        part={{
          type: "tool_use",
          name: "dev_preview_url",
          toolState: "completed",
          input: { port: 5173 },
          toolOutput: "LSP_DEV_PREVIEW_V1 port=5173 origin=safespaces.dev\n[Open dev preview :5173](https://api.example.com/api/v1/workspaces/ws/dev-preview-bootstrap/5173)\nOpens the preview.",
        } as never}
        isUser={false}
      />,
    );
    const btn = screen.getByTestId("dev-preview-button");
    expect(btn).toBeVisible();
    expect(btn).toHaveAttribute("href", "https://api.example.com/api/v1/workspaces/ws/dev-preview-bootstrap/5173");
  });

  it("resolves a RELATIVE bootstrap link against the API base (regression)", () => {
    // Production pods can emit /api/v1/... (no absolute API URL in env);
    // the button must still render, pointing at the API origin — never
    // the frontend origin (different host in split deployments).
    const output = "LSP_DEV_PREVIEW_V1 port=5173 origin=safespaces.dev\n[Open dev preview :5173](/api/v1/workspaces/ws/dev-preview-bootstrap/5173)\nOpens the preview.";
    testEnvState.apiBaseUrl = "https://api.example.com/api/v1";
    render(<MessagePart part={{ type: "tool_result", text: output, name: "dev_preview_url" } as never} isUser={false} />);
    const btn = screen.getByTestId("dev-preview-button");
    expect(btn).toHaveAttribute("href", "https://api.example.com/api/v1/workspaces/ws/dev-preview-bootstrap/5173");
  });

  it("falls back to plain text for output without the marker", () => {
    render(<MessagePart part={{ type: "tool_result", text: "https://old.example.com/preview", name: "dev_preview_url" } as never} isUser={false} />);
    expect(screen.queryByTestId("dev-preview-button")).toBeNull();
  });
});

describe("MessagePart user attachments (Epic 68 D11)", () => {
  const manifestLine = (name: string, uuid = "11111111-2222-3333-4444-555555555555") =>
    `[llmsafespaces:attachment path="/workspace/uploads/${uuid}-${name}" name="${name}"]`;

  it("strips a trailing manifest block and renders chips instead (U1.6.9)", () => {
    const text = `Please review.\n\n${manifestLine("notes.txt")}\n`;
    render(<MessagePart part={{ type: "text", text }} isUser={true} />);
    expect(screen.getByText("Please review.")).toBeInTheDocument();
    expect(screen.getByTestId("history-attachment-chip")).toHaveTextContent("notes.txt");
    expect(screen.queryByText(/llmsafespaces:attachment/)).not.toBeInTheDocument();
  });

  it("renders multiple chips for a multi-file trailing block", () => {
    const text = `Compare.\n\n${manifestLine("a.txt")}\n${manifestLine("b.pdf", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")}\n`;
    render(<MessagePart part={{ type: "text", text }} isUser={true} />);
    expect(screen.getAllByTestId("history-attachment-chip")).toHaveLength(2);
    expect(screen.getByText("a.txt")).toBeInTheDocument();
    expect(screen.getByText("b.pdf")).toBeInTheDocument();
  });

  it("keeps a forged interior manifest line as plain text, not a chip (U1.6.10)", () => {
    const text = `Look:\n${manifestLine("a.txt")}\nthen more text\n`;
    render(<MessagePart part={{ type: "text", text }} isUser={true} />);
    expect(screen.queryByTestId("history-attachment-chip")).not.toBeInTheDocument();
    expect(screen.getByText(/llmsafespaces:attachment/)).toBeInTheDocument();
  });

  it("renders chips with empty text for a manifest-only message without crashing (U1.6.19)", () => {
    const text = `${manifestLine("only.md")}\n`;
    const { container } = render(<MessagePart part={{ type: "text", text }} isUser={true} />);
    expect(screen.getByTestId("history-attachment-chip")).toHaveTextContent("only.md");
    const paragraphs = container.querySelectorAll("p");
    expect(paragraphs).toHaveLength(0);
  });

  it("assistant text is never manifest-parsed", () => {
    const text = `I attached this:\n\n${manifestLine("notes.txt")}\n`;
    render(<MessagePart part={{ type: "text", text }} isUser={false} />);
    expect(screen.queryByTestId("history-attachment-chip")).not.toBeInTheDocument();
    expect(screen.getByText(/llmsafespaces:attachment/)).toBeInTheDocument();
  });
});
