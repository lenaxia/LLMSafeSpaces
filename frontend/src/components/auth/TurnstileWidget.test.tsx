import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render as rtlRender, waitFor } from "@testing-library/react";

// TurnstileWidget covers script loading + widget lifecycle. These tests
// stub window.turnstile and mock document.head.appendChild so we can
// simulate the full load → render → cleanup flow without hitting the
// real Cloudflare CDN.
//
// Rationale for testing this component (per PR #501 review feedback):
// the widget is 139 net-new lines including a module-level singleton
// script loader, a callback map lifetime tied to useEffect cleanup,
// and a siteKey-empty short-circuit that hides the widget entirely.
// A regression that (a) never calls onToken, (b) leaks the widget on
// unmount, or (c) renders even when siteKey is empty would ship silently
// without this coverage.

// Reset the module's loadPromise singleton between tests. We can't
// import it (it's file-local), so we do the next best thing: reload
// the module fresh in each test via vi.resetModules + dynamic import.
// This is fine for a small file — the setup cost is negligible.
async function freshWidget() {
  vi.resetModules();
  return (await import("./TurnstileWidget")).TurnstileWidget;
}

// installFakeTurnstile installs a fake window.turnstile that records
// each render call and lets tests trigger the callbacks synchronously.
function installFakeTurnstile() {
  const widgets: Array<{
    id: string;
    opts: {
      sitekey: string;
      callback?: (token: string) => void;
      "expired-callback"?: () => void;
      "error-callback"?: (err: string) => void;
    };
    container: HTMLElement;
  }> = [];
  let removed = 0;

  (window as unknown as { turnstile: unknown }).turnstile = {
    render: (container: HTMLElement, opts: (typeof widgets)[0]["opts"]) => {
      const id = `widget-${widgets.length}`;
      widgets.push({ id, opts, container });
      return id;
    },
    reset: vi.fn(),
    remove: (id: string) => {
      removed++;
      const i = widgets.findIndex((w) => w.id === id);
      if (i >= 0) widgets.splice(i, 1);
    },
  };
  return {
    widgets,
    removedCount: () => removed,
  };
}

function uninstallFakeTurnstile() {
  delete (window as unknown as { turnstile?: unknown }).turnstile;
  // Also remove the injected script tag so freshWidget starts with a
  // clean DOM.
  document
    .querySelectorAll("script#cf-turnstile-script")
    .forEach((el) => el.remove());
}

describe("TurnstileWidget", () => {
  beforeEach(() => {
    // Simulate the script "loading" by pre-installing window.turnstile.
    // The module's loadTurnstileScript short-circuits when window.turnstile
    // is already defined via a polling wait — that path is what the
    // test exercises. Directly appending a stub script tag with id
    // #cf-turnstile-script triggers the fast-path.
    const script = document.createElement("script");
    script.id = "cf-turnstile-script";
    document.head.appendChild(script);
  });
  afterEach(() => {
    uninstallFakeTurnstile();
  });

  it("renders nothing when siteKey is empty", async () => {
    const Widget = await freshWidget();
    const { container } = rtlRender(
      <Widget siteKey="" onToken={vi.fn()} />,
    );
    // Component returns null → container has no children.
    expect(container.firstChild).toBeNull();
  });

  it("calls window.turnstile.render with the provided siteKey once loaded", async () => {
    const fake = installFakeTurnstile();
    const Widget = await freshWidget();
    const onToken = vi.fn();
    rtlRender(<Widget siteKey="test-site-key-123" onToken={onToken} />);

    await waitFor(() => {
      expect(fake.widgets).toHaveLength(1);
    });
    expect(fake.widgets[0]!.opts.sitekey).toBe("test-site-key-123");
  });

  it("invokes onToken when Cloudflare fires the success callback", async () => {
    const fake = installFakeTurnstile();
    const Widget = await freshWidget();
    const onToken = vi.fn();
    rtlRender(<Widget siteKey="key" onToken={onToken} />);

    await waitFor(() => expect(fake.widgets).toHaveLength(1));
    // Simulate Cloudflare firing the callback (this happens after
    // the user completes the challenge — invisible in "managed" mode).
    act(() => {
      fake.widgets[0]!.opts.callback?.("challenge-token-abc");
    });
    expect(onToken).toHaveBeenCalledWith("challenge-token-abc");
  });

  it("invokes onExpire when Cloudflare fires the expired callback", async () => {
    const fake = installFakeTurnstile();
    const Widget = await freshWidget();
    const onExpire = vi.fn();
    rtlRender(
      <Widget siteKey="key" onToken={vi.fn()} onExpire={onExpire} />,
    );

    await waitFor(() => expect(fake.widgets).toHaveLength(1));
    act(() => {
      fake.widgets[0]!.opts["expired-callback"]?.();
    });
    expect(onExpire).toHaveBeenCalled();
  });

  it("invokes onError when Cloudflare fires the error callback", async () => {
    const fake = installFakeTurnstile();
    const Widget = await freshWidget();
    const onError = vi.fn();
    rtlRender(
      <Widget siteKey="key" onToken={vi.fn()} onError={onError} />,
    );

    await waitFor(() => expect(fake.widgets).toHaveLength(1));
    act(() => {
      fake.widgets[0]!.opts["error-callback"]?.("script-fail");
    });
    expect(onError).toHaveBeenCalledWith("script-fail");
  });

  it("removes the widget when the component unmounts", async () => {
    const fake = installFakeTurnstile();
    const Widget = await freshWidget();
    const { unmount } = rtlRender(<Widget siteKey="key" onToken={vi.fn()} />);

    await waitFor(() => expect(fake.widgets).toHaveLength(1));
    unmount();
    expect(fake.removedCount()).toBe(1);
    // Widget list is emptied by remove.
    expect(fake.widgets).toHaveLength(0);
  });

  it("applies the passed className to the container", async () => {
    installFakeTurnstile();
    const Widget = await freshWidget();
    const { container } = rtlRender(
      <Widget siteKey="key" onToken={vi.fn()} className="my-custom-class" />,
    );
    // Container div is the immediate child of test container.
    expect(container.firstElementChild?.className).toBe("my-custom-class");
  });
});
