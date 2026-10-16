import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import { render } from "../../test/utils";
import { MessageBubble } from "./MessageBubble";
import {
  parseThemeTokens,
  resolveColorClasses,
  surfaceFromClasses,
  token,
  textOnSurfaceRatio,
} from "../../test/wcag";
import type { Message } from "../../api/types";

/**
 * #1716 (owner report) — the agent-origin "message from session X"
 * sentinel badge renders INSIDE the user bubble, which is an INVERTED
 * surface (bg-primary + text-primary-foreground; dark: bg-slate-600 +
 * text-slate-100). The badge historically borrowed muted-surface tokens
 * (text-muted-foreground on bg-muted/40); composed over the inverted
 * bubble in the LIGHT scheme its text measured 1.05:1 — invisible. Dark
 * measured 3.73:1 — visible, which is why only light was reported.
 *
 * These pins compute the real composed contrast (badge classes resolved
 * against the palette tokens, alpha-composited over the bubble surface
 * the way the browser cascade does) and demand WCAG AA normal-text
 * contrast (the badge is 11px monospace) in BOTH schemes.
 */

vi.mock("../../hooks/useNow", () => ({
  useNow: () => new Date("2024-06-15T12:00:00Z").getTime(),
}));

const { light, dark } = parseThemeTokens();
const NORMAL_TEXT = 4.5;

const sentinelMsg: Message = {
  id: "1",
  role: "user",
  createdAt: "2024-06-15T11:30:00Z",
  parts: [
    {
      type: "text",
      text: '<!-- lsp:agent-message-v1 {"fromSession":"ses_f499ee9e6ffe52BJ8jxc2TEQQJ"} -->',
    },
  ],
};

/**
 * The bubble an element renders inside. NOT `closest("div[class*='bg-']")`
 * — the badge itself carries bg utilities (bg-muted/40 today), and a
 * substring match would return the element under test as its own surface.
 * Walk strict parents to the first bubble surface (user: bg-primary,
 * assistant: bg-muted).
 */
function bubbleOf(el: HTMLElement): HTMLElement {
  let node = el.parentElement;
  while (node && !/(^|\s)bg-(primary|muted)(\s|$)/.test(node.className)) {
    node = node.parentElement;
  }
  if (!node) throw new Error("element rendered outside a bubble surface");
  return node;
}

function badgeInBubble(): { badgeClass: string; bubbleClass: string } {
  // Rendered through the REAL MessageBubble/user-part path (parseAgentMessage
  // → AgentOriginBadge), not the badge in isolation — the pin must see the
  // surface the timeline actually places it on.
  render(<MessageBubble message={sentinelMsg} />);
  const badge = screen.getByTestId("agent-origin-badge");
  const bubble = bubbleOf(badge);
  return { badgeClass: badge.className, bubbleClass: bubble.className };
}

describe("agent-origin sentinel contrast (#1716)", () => {
  it("LIGHT: badge text reads on the inverted user bubble (≥ 4.5:1)", () => {
    const { badgeClass, bubbleClass } = badgeInBubble();
    const surface = surfaceFromClasses(bubbleClass, "light", light, token(light, "background"));
    const { ratio } = textOnSurfaceRatio(
      resolveColorClasses(badgeClass, "light", light),
      surface,
    );
    // Pre-fix: 1.05:1 (fg #64748b ≈ bg #69707d) — the invisible sentinel.
    expect(ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("DARK: badge text reads on the dark user bubble (≥ 4.5:1)", () => {
    const { badgeClass, bubbleClass } = badgeInBubble();
    const surface = surfaceFromClasses(bubbleClass, "dark", dark, token(dark, "background"));
    const { ratio } = textOnSurfaceRatio(
      resolveColorClasses(badgeClass, "dark", dark),
      surface,
    );
    // Pre-fix: 3.73:1 — visible-but-sub-AA; the fix must lift both schemes.
    expect(ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("the badge never re-borrows muted tokens (structural: muted is an assistant-surface palette)", () => {
    // Computed ratios above are the contract; this structural pin catches
    // the regression at the class level with a readable failure message.
    const { badgeClass } = badgeInBubble();
    expect(badgeClass).not.toMatch(/(^|\s)(text|bg|border)-muted(-foreground)?(\/\d+)?(\s|$)/);
  });
});

describe("chat timeline meta text contrast (#1716 audit, reading surface)", () => {
  const userMsg: Message = {
    id: "2",
    role: "user",
    createdAt: "2024-06-15T11:30:00Z",
    parts: [{ type: "text", text: "Hey" }],
  };
  const agentMsg: Message = {
    id: "3",
    role: "assistant",
    createdAt: "2024-06-15T11:30:00Z",
    parts: [{ type: "text", text: "Hi there" }],
  };

  function timestampInBubble(msg: Message): { spanClass: string; bubbleClass: string } {
    render(<MessageBubble message={msg} />);
    // data-testid sits on an inner span; the color utilities are carried by
    // its parent span (cn("text-xs leading-none truncate", …)) — resolve the
    // colored element, not the marker.
    const marker = screen.getByTestId("message-timestamp");
    const colored = marker.parentElement;
    if (!colored || !/(^|\s)text-/.test(colored.className)) {
      throw new Error("timestamp color span not found above the marker");
    }
    const bubble = bubbleOf(marker);
    return { spanClass: colored.className, bubbleClass: bubble.className };
  }

  it("LIGHT: assistant-bubble timestamp reads on the muted bubble (≥ 4.5:1)", () => {
    // Pre-fix: text-muted-foreground/70 on bg-muted = 2.58:1 — RED.
    const { spanClass, bubbleClass } = timestampInBubble(agentMsg);
    const surface = surfaceFromClasses(bubbleClass, "light", light, token(light, "background"));
    const { ratio } = textOnSurfaceRatio(resolveColorClasses(spanClass, "light", light), surface);
    expect(ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("LIGHT: user-bubble timestamp reads on the inverted bubble (≥ 4.5:1, pin)", () => {
    // text-primary-foreground/70 over bg-primary ≈ 8.8:1 — passes today;
    // pinned so the sentinel fix cannot regress it.
    const { spanClass, bubbleClass } = timestampInBubble(userMsg);
    const surface = surfaceFromClasses(bubbleClass, "light", light, token(light, "background"));
    const { ratio } = textOnSurfaceRatio(resolveColorClasses(spanClass, "light", light), surface);
    expect(ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });
});
