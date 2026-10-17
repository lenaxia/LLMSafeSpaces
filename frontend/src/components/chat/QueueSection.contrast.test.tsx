import { describe, expect, it } from "vitest";
import { render } from "../../test/utils";
import { screen, within } from "@testing-library/react";
import { QueueSection } from "./QueueSection";
import type { QueuedMessage } from "../../hooks/useMessageQueue";
import {
  parseThemeTokens,
  resolveColorClasses,
  token,
  textOnSurfaceRatio,
} from "../../test/wcag";

/**
 * #1716 r1 — the queue error pill. PR #1718's review (CHANGES_REQUESTED)
 * recomputed the pill with this PR's own harness: the failed-send pill
 * (`bg-destructive/20 text-destructive dark:bg-destructive/30`) carried
 * `opacity-70`/`opacity-80` on its text children, measuring 3.06–3.58:1
 * in light and 1.46–1.80:1 in dark — worse than the sentinel that opened
 * the issue. These pins compute the pill's real rendered contrast per
 * scheme, opacity dilution included, and demand AA normal-text contrast
 * (the pill body is text-sm/text-xs).
 *
 * The pill's ink is inherited from its container (currentColor), so each
 * child is resolved as container-classes + child-classes — the resolver
 * treats that concatenation exactly as the cascade does.
 */

const { light, dark } = parseThemeTokens();
const NORMAL_TEXT = 4.5;

const failedSend: QueuedMessage = {
  id: "q1",
  text: "Ship the r1 rework",
  status: "error",
  error: "session suspended before delivery",
  sessionId: "ses_x",
};

function errorPill(): { pillClass: string; struckClass: string; errorClass: string; retryClass: string } {
  render(
    <QueueSection messages={[failedSend]} onRetry={() => {}} onDismiss={() => {}} isMobile={false} />,
  );
  const pill = document.querySelector<HTMLElement>("div[class*='bg-destructive']");
  if (!pill) throw new Error("error pill not rendered");
  const struck = screen.getByText("Ship the r1 rework");
  const errorLine = screen.getByText("session suspended before delivery");
  const retry = within(pill).getByRole("button", { name: "Retry" });
  return {
    pillClass: pill.className,
    struckClass: struck.className,
    errorClass: errorLine.className,
    retryClass: retry.className,
  };
}

function pillRatio(
  mergedClasses: string,
  scheme: "light" | "dark",
): ReturnType<typeof textOnSurfaceRatio> {
  const tokens = scheme === "light" ? light : dark;
  // The pill sits on the page background surface; its own translucent
  // bg-destructive tint composites over it inside the helper.
  return textOnSurfaceRatio(resolveColorClasses(mergedClasses, scheme, tokens), token(tokens, "background"));
}

describe("queue error pill contrast (#1716 r1)", () => {
  // Pre-fix (head ab177f01): struck text 3.06 light / 1.46 dark, error
  // line 3.58 light / 1.80 dark, retry ink 4.59 light / 1.60 dark — the
  // diluted rows are RED; the light retry row passes today and is a pin.
  describe("LIGHT", () => {
    it("struck-through original text ≥ 4.5:1", () => {
      const { pillClass, struckClass } = errorPill();
      expect(pillRatio(`${pillClass} ${struckClass}`, "light").ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
    });

    it("delivery-error line ≥ 4.5:1", () => {
      const { pillClass, errorClass } = errorPill();
      expect(pillRatio(`${pillClass} ${errorClass}`, "light").ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
    });

    it("retry/dismiss action ink ≥ 4.5:1 (undiluted — pin)", () => {
      const { pillClass, retryClass } = errorPill();
      expect(pillRatio(`${pillClass} ${retryClass}`, "light").ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
    });
  });

  describe("DARK", () => {
    it("struck-through original text ≥ 4.5:1", () => {
      const { pillClass, struckClass } = errorPill();
      expect(pillRatio(`${pillClass} ${struckClass}`, "dark").ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
    });

    it("delivery-error line ≥ 4.5:1", () => {
      const { pillClass, errorClass } = errorPill();
      expect(pillRatio(`${pillClass} ${errorClass}`, "dark").ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
    });

    it("retry/dismiss action ink ≥ 4.5:1", () => {
      const { pillClass, retryClass } = errorPill();
      expect(pillRatio(`${pillClass} ${retryClass}`, "dark").ratio).toBeGreaterThanOrEqual(NORMAL_TEXT);
    });
  });
});
