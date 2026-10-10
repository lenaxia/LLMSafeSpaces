import { useEffect } from "react";

// #1648 criterion 4 — on-screen keyboard. `100dvh` tracks the DYNAMIC
// viewport (browser chrome show/hide), NOT the OSK: Chrome 108+ defaults
// to interactive-widget=resizes-visual, where the keyboard resizes only
// the visual viewport and viewport units are unchanged; iOS Safari
// overlays the keyboard without resizing anything. Two composed defenses:
//
//  1. index.html: interactive-widget=resizes-content — where honored
//     (Chrome 108+, Android), the keyboard resizes the layout viewport,
//     so 100dvh itself shrinks. Pure platform behavior, no JS. When it
//     fires, window.innerHeight shrinks with the visual viewport and the
//     heuristic below never activates — the declarative unit already did
//     the work.
//
//  2. this guard (iOS Safari and any browser that ignores the meta key):
//     when the visual viewport shrinks sharply relative to the layout
//     viewport (keyboard heuristic), pin the dvh shells to the live
//     visual-viewport height via the [data-osk-open] rule in index.css.
//     Conditional by design: with no keyboard the shell stays purely
//     declarative 100dvh — this guard cannot regress the primary fix.
//
// The app shell is the whole page and never scrolls (html/body overscroll
// containment + fixed-height shell), so the visible region is
// [vv.offsetTop, vv.offsetTop + vv.height]; when a keyboard pans the
// visual viewport the shell height must keep the bottom row inside that
// region — hence the min() with the offset-corrected span.
const OSK_THRESHOLD_PX = 24;

function syncOskState() {
  const vv = window.visualViewport;
  if (!vv) return;
  const root = document.documentElement;
  if (vv.height <= window.innerHeight - OSK_THRESHOLD_PX) {
    root.dataset.oskOpen = "true";
    const visibleSpan = Math.min(vv.height, window.innerHeight - vv.offsetTop);
    root.style.setProperty("--shell-visual-height", `${visibleSpan}px`);
  } else {
    delete root.dataset.oskOpen;
    root.style.removeProperty("--shell-visual-height");
  }
}

export function useOskViewportGuard() {
  useEffect(() => {
    const vv = window.visualViewport;
    if (!vv) return;
    vv.addEventListener("resize", syncOskState);
    vv.addEventListener("scroll", syncOskState);
    syncOskState();
    return () => {
      vv.removeEventListener("resize", syncOskState);
      vv.removeEventListener("scroll", syncOskState);
      delete document.documentElement.dataset.oskOpen;
      document.documentElement.style.removeProperty("--shell-visual-height");
    };
  }, []);
}
