import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

/**
 * #1720 — source-shape pins on the chat timeline scroll controller.
 *
 * The snap-back loop is a race between the scroll-event handler and the
 * programmatic snap sites, so parts of the contract cannot be forced
 * deterministically in jsdom (real event interleaving decides). Per the
 * issue's acceptance, these pins freeze the SHAPE of the controller so
 * the racing invariants survive refactors:
 *
 *  S1 the stream-follow MutationObserver gates BOTH its callback and its
 *     rAF on the tail flag (stream handlers scroll only when tailing);
 *  S2 the streaming-transition effect gates scrollToBottom on the tail
 *     flag (a stream starting never silently re-arms a disarmed tail);
 *  S3 the scroll-event handler has a programmatic-scroll guard branch
 *     (our snaps are invisible to the user-intent detector);
 *  S4 every `scrollTop` write site is preceded by a programmatic-scroll
 *     guard mark (no unguarded assignment may regress in).
 *
 * fs-read precedent: styles/index.test.ts.
 */

// vitest runs with the frontend package as cwd (wcag.ts precedent).
const source = readFileSync(
  resolve(process.cwd(), "src/components/chat/MessageList.tsx"),
  "utf8",
);

describe("#1720 source-shape pins (scroll controller)", () => {
  it("S1: stream-follow observer gates callback AND rAF on the tail flag", () => {
    const observerMatch = source.match(/new MutationObserver\(\(\) => \{[\s\S]*?\}\);/);
    expect(observerMatch).not.toBeNull();
    const observer = observerMatch![0];
    expect(observer).toMatch(/if \(!stickToBottom\.current/);
    expect(observer).toMatch(/if \(stickToBottom\.current && el\)/);
  });

  it("S2: the streaming-transition effect gates scrollToBottom on the tail flag", () => {
    const effectMatch = source.match(
      /useEffect\(\(\) => \{[\s\S]*?\}, \[streaming, scrollToBottom\]\);/,
    );
    expect(effectMatch).not.toBeNull();
    expect(effectMatch![0]).toContain("if (streaming && stickToBottom.current)");
  });

  it("S3: the scroll-event handler carries a programmatic-scroll guard branch", () => {
    const handlerMatch = source.match(/const checkIfAtBottom = useCallback\(\(\) => \{[\s\S]*?\n {2}\}, \[\]\);/);
    expect(handlerMatch).not.toBeNull();
    const handler = handlerMatch![0];
    expect(handler).toMatch(/if \(programmaticScrollRef\.current\)/);
  });

  it("S4: every scrollTop write goes through the guarded helper", () => {
    const lines = source.split("\n");
    // Raw assignment sites (exclude `===` reads). After the #1720 fix
    // there is exactly ONE — inside setProgrammaticScrollTop, directly
    // after the guard flag is set. Any new raw write regresses in
    // unguarded and must fail this pin.
    const writeLines = lines
      .map((line, i) => ({ line, i }))
      .filter(({ line }) => /el\.scrollTop\s*=(?!=)/.test(line) && !line.trim().startsWith("//"));
    expect(writeLines.length).toBe(1);
    const { i } = writeLines[0]!;
    const window = lines.slice(Math.max(0, i - 6), i + 1).join("\n");
    expect(window).toContain("programmaticScrollRef.current = true");

    // And the four behavior sites route through the helper: explicit
    // jump/resume, layout snap on append, prepend scroll-anchoring,
    // stream-follow observer rAF.
    const callSites = (source.match(/setProgrammaticScrollTop\(el/g) ?? []).length;
    expect(callSites).toBeGreaterThanOrEqual(4);
  });

  it("S5: no near-bottom arm of the tail flag while a stream is active (loop guard)", () => {
    // The handler may only arm when NOT streaming: pin the
    // `!streamingRef.current` condition on the arm branch.
    const handlerMatch = source.match(/const checkIfAtBottom = useCallback\(\(\) => \{[\s\S]*?\n {2}\}, \[\]\);/);
    expect(handlerMatch).not.toBeNull();
    const handler = handlerMatch![0];
    const armSites = handler.match(/stickToBottom\.current = true/g) ?? [];
    // The handler must contain NO unconditional arm site at all (the
    // only arms are the guarded idle branch — matched by S5's condition.
    expect(handler).toMatch(/!streamingRef\.current && !stickToBottom\.current/);
    expect(armSites.length).toBe(1);
  });
});
