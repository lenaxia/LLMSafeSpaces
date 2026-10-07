import "@testing-library/jest-dom/vitest";

// jsdom doesn't implement scrollIntoView
Element.prototype.scrollIntoView = () => {};

// jsdom 20 doesn't expose a Touch constructor (TouchEvent exists); tests
// that dispatch touch sequences need it (extracted from the
// useSwipeableSidebar suite's local polyfill so every test file gets it).
if (typeof globalThis.Touch === "undefined") {
  const PolyfillTouch = class {
    clientX: number;
    clientY: number;
    identifier: number;
    target: EventTarget;
    constructor(init: TouchInit) {
      this.clientX = init.clientX ?? 0;
      this.clientY = init.clientY ?? 0;
      this.identifier = init.identifier;
      this.target = init.target;
    }
  };
  (globalThis as unknown as Record<string, unknown>)["Touch"] = PolyfillTouch;
}

// jsdom doesn't implement matchMedia
Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }),
});

// jsdom doesn't implement ResizeObserver
class MockResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}
Object.defineProperty(window, "ResizeObserver", {
  writable: true,
  value: MockResizeObserver,
});
