import { useEffect, useRef } from "react";

/**
 * Cloudflare Turnstile widget wrapper.
 *
 * Loads the Turnstile client script once (cached by the browser after
 * the first mount across the app) and renders an invisible/managed
 * widget targeting the passed siteKey. Fires `onToken` with the
 * verification token; the caller then attaches this token to
 * subsequent API requests (e.g. via the `cf-turnstile-response`
 * header).
 *
 * When siteKey is empty, the component renders nothing and never
 * loads the script — matches the chart-side `turnstile.enabled=false`
 * fallback so the register page still renders in dev / self-hosted
 * setups without Turnstile.
 *
 * Docs: https://developers.cloudflare.com/turnstile/get-started/client-side-rendering/
 */

interface TurnstileWidgetProps {
  siteKey: string;
  onToken: (token: string) => void;
  onExpire?: () => void;
  onError?: (err: string) => void;
  className?: string;
}

// Cloudflare's script exposes `window.turnstile.render(container, opts)`.
// Typed narrowly so we don't accidentally use a global that isn't there.
interface TurnstileGlobal {
  render: (
    container: HTMLElement,
    opts: {
      sitekey: string;
      callback?: (token: string) => void;
      "expired-callback"?: () => void;
      "error-callback"?: (err: string) => void;
      theme?: "auto" | "light" | "dark";
      size?: "normal" | "flexible" | "compact";
    },
  ) => string; // returns widgetId
  reset: (widgetId?: string) => void;
  remove: (widgetId?: string) => void;
}

declare global {
  interface Window {
    turnstile?: TurnstileGlobal;
  }
}

const TURNSTILE_SCRIPT_URL =
  "https://challenges.cloudflare.com/turnstile/v0/api.js";
const TURNSTILE_SCRIPT_ID = "cf-turnstile-script";

// Cache the load promise so parallel mounts don't inject the script
// twice. Never rejects — errors are surfaced via the widget's own
// error-callback since a rejected promise here means Turnstile is
// unusable and we shouldn't retry per-mount.
let loadPromise: Promise<void> | null = null;

function loadTurnstileScript(): Promise<void> {
  if (loadPromise) return loadPromise;
  loadPromise = new Promise<void>((resolve) => {
    // If the script tag already exists (e.g. injected by another
    // component or the app shell), wait for window.turnstile to appear.
    if (document.getElementById(TURNSTILE_SCRIPT_ID)) {
      const check = () => {
        if (window.turnstile) resolve();
        else setTimeout(check, 50);
      };
      check();
      return;
    }
    const s = document.createElement("script");
    s.id = TURNSTILE_SCRIPT_ID;
    s.src = TURNSTILE_SCRIPT_URL;
    s.async = true;
    s.defer = true;
    s.onload = () => resolve();
    s.onerror = () => {
      // Don't reject; the widget won't render and the user gets a
      // clear "verify_unavailable" from the backend when they submit.
      resolve();
    };
    document.head.appendChild(s);
  });
  return loadPromise;
}

export function TurnstileWidget({
  siteKey,
  onToken,
  onExpire,
  onError,
  className,
}: TurnstileWidgetProps) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const widgetIdRef = useRef<string | null>(null);

  useEffect(() => {
    if (!siteKey || !containerRef.current) return;
    const container = containerRef.current;
    let cancelled = false;

    loadTurnstileScript().then(() => {
      if (cancelled || !window.turnstile) return;
      widgetIdRef.current = window.turnstile.render(container, {
        sitekey: siteKey,
        callback: (token) => onToken(token),
        "expired-callback": () => onExpire?.(),
        "error-callback": (err) => onError?.(err),
        theme: "auto",
        size: "flexible",
      });
    });

    return () => {
      cancelled = true;
      if (widgetIdRef.current && window.turnstile) {
        try {
          window.turnstile.remove(widgetIdRef.current);
        } catch {
          // widget may already be gone; ignore.
        }
        widgetIdRef.current = null;
      }
    };
    // Deliberately omit callbacks from deps: they're expected to be
    // stable via useCallback in the parent. Re-rendering the widget on
    // every parent re-render would spam Cloudflare's edge with widget
    // creations and reset the user's verification state.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [siteKey]);

  if (!siteKey) return null;
  return <div ref={containerRef} className={className} />;
}
