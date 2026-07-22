import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { MessagePart, closeOpenFence } from "./MessagePart";
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
