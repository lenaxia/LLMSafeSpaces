import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { render } from "../../test/utils";
import { MessageList } from "./MessageList";
import type { Message } from "../../api/types";
import {
  parseThemeTokens,
  resolveColorClasses,
  token,
  textOnSurfaceRatio,
} from "../../test/wcag";

/**
 * #1720 — contrast pins for the "Resume tailing" control.
 *
 * The resume affordance (#1720 rule 3) is the user's only way back into
 * the stream tail once they scroll away mid-stream, so it must be
 * readable in both schemes. Per the #1716/#1718 computed-contrast
 * harness pattern: render the real control, resolve its Tailwind
 * classes against the theme tokens, compute the WCAG ratio per scheme
 * (the pill body is text-xs → normal text, ≥ 4.5:1), compositing the
 * pill's own surface over the page background.
 */

const { light, dark } = parseThemeTokens();
const NORMAL_TEXT = 4.5;

const messages: Message[] = [
  { id: "1", role: "user", parts: [{ type: "text", text: "Hello" }] },
  { id: "2", role: "assistant", parts: [{ type: "text", text: "Hi!" }] },
];

function resumeControlClass(): string {
  render(
    <MessageList messages={messages} streaming={true} streamingBubble={<div>streaming…</div>} />,
  );
  const log = screen.getByRole("log");
  Object.defineProperty(log, "scrollHeight", { value: 1000, configurable: true });
  Object.defineProperty(log, "clientHeight", { value: 200, configurable: true });
  Object.defineProperty(log, "scrollTop", { value: 0, writable: true, configurable: true });
  fireEvent.scroll(log);
  let btn: HTMLElement | null = null;
  return waitFor(() => {
    expect(screen.getByText("Resume tailing")).toBeInTheDocument();
    btn = screen.getByText("Resume tailing").closest("button");
    if (!btn) throw new Error("resume control button not found");
    return btn.className;
  });
}

function controlRatio(pillClass: string, scheme: "light" | "dark") {
  const tokens = scheme === "light" ? light : dark;
  return textOnSurfaceRatio(
    resolveColorClasses(pillClass, scheme, tokens),
    token(tokens, "background"),
  );
}

describe("resume-tail control contrast (#1720, per #1716/#1718 harness)", () => {
  it("LIGHT: control ink on its surface ≥ 4.5:1", async () => {
    const cls = await resumeControlClass();
    expect(controlRatio(cls, "light").ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("DARK: control ink on its surface ≥ 4.5:1", async () => {
    const cls = await resumeControlClass();
    expect(controlRatio(cls, "dark").ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });
});
