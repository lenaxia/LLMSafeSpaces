/**
 * jsdom geometry mocking for viewport-aware menu positioning tests.
 *
 * jsdom doesn't compute layout — getBoundingClientRect/offsetWidth/scrollHeight
 * return zeros. These helpers mock just enough geometry to assert that a
 * component wires DOM measurement through computeMenuPosition into the applied
 * inline style (flip-above / left-clamp / maxHeight-cap), following the pattern
 * established by KebabMenu.test.tsx.
 *
 * Usage:
 *
 *   afterEach(restoreMenuGeometry);
 *   mockViewport(600);            // window.innerHeight = 600
 *   mockMenuSize(150, 240);       // every element reports 150×240
 *   mockButtonRect(trigger, { top: 580, bottom: 600, left: 300, right: 460 });
 *
 * mockButtonRect is per-element (defineProperty on the instance); the others
 * are prototype/window level and are undone by restoreMenuGeometry.
 */

interface Rect {
  top: number;
  bottom: number;
  left: number;
  right: number;
}

export function mockButtonRect(el: HTMLElement, rect: Rect): void {
  Object.defineProperty(el, "getBoundingClientRect", {
    configurable: true,
    value: () => rect,
  });
}

const restores: Array<() => void> = [];

export function mockMenuSize(height: number, width = 240): void {
  const proto = HTMLElement.prototype as unknown as Record<string, unknown>;
  const origScrollHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  const origOffsetWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", { configurable: true, get: () => height });
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get: () => width });
  restores.push(() => {
    if (origScrollHeight) Object.defineProperty(HTMLElement.prototype, "scrollHeight", origScrollHeight);
    else delete proto.scrollHeight;
    if (origOffsetWidth) Object.defineProperty(HTMLElement.prototype, "offsetWidth", origOffsetWidth);
    else delete proto.offsetWidth;
  });
}

export function mockViewport(height: number, width?: number): void {
  const origInnerHeight = Object.getOwnPropertyDescriptor(window, "innerHeight");
  Object.defineProperty(window, "innerHeight", { configurable: true, value: height });
  restores.push(() => {
    if (origInnerHeight) Object.defineProperty(window, "innerHeight", origInnerHeight);
    else delete (window as unknown as Record<string, unknown>).innerHeight;
  });
  if (width !== undefined) {
    const origInnerWidth = Object.getOwnPropertyDescriptor(window, "innerWidth");
    Object.defineProperty(window, "innerWidth", { configurable: true, value: width });
    restores.push(() => {
      if (origInnerWidth) Object.defineProperty(window, "innerWidth", origInnerWidth);
      else delete (window as unknown as Record<string, unknown>).innerWidth;
    });
  }
}

export function restoreMenuGeometry(): void {
  while (restores.length > 0) restores.pop()!();
}
