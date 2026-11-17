/**
 * The v0.44.15 owner report (2026-11-16), end to end with TRUSTED touch
 * input: "horizontal swiping does NOTHING — cannot scroll to the done
 * column, swiping does not move focus between columns, and all columns
 * always stay the same width."
 *
 * Why this spec exists when board.spec.ts already pinned the rail: every
 * existing rail test drives scrolling PROGRAMMATICALLY (el.scrollTo,
 * scrollIntoView, keyboard) — none of those paths consult touch-action
 * or run a touchmove's default action, so CI was green while real
 * fingers were dead. Measured on main at 390x844 with touchscreen CDP
 * input, two stacked causes killed the swipe:
 *
 *   1. useSwipeableSidebar's non-passive touchmove on the shell root
 *      preventDefault()ed EVERY horizontal-dominant move app-wide
 *      (claimed or not) — the native pan was canceled before it began.
 *   2. touch-action: pan-y on html/body/AppShell/PortalLayout —
 *      touch-action intersects along the ANCESTOR chain, so pan-y at
 *      the root forbade horizontal touch panning for every descendant
 *      (a descendant can never re-enable what an ancestor disabled).
 *
 * This spec is the regression pin for the fix: a real touchscreen swipe
 * pans the rail, mandatory snap settles per column, done is fully
 * reachable, and the 2079 focus-column transit works under real input.
 * The board's own API surface is mocked exactly like board.spec.ts.
 */
import { test, expect, type Page, type Route, type Locator } from "@playwright/test";

const WORKSPACE_ID = "ws-board-e2e";
const API = "**/api/v1";
const PROJECT_A = "prj-aaaa1111";

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

// --- fixtures (board.spec.ts shapes, trimmed to the rail's needs) ----------------

type E2ETask = {
  id: string;
  project_id: string;
  epic_id: string;
  short_id: string;
  title: string;
  description: string;
  stage: string;
  paused: boolean;
  project_name: string;
  epic_name: string;
  tags: string[];
  acceptance_criteria: Array<{ text: string; met: boolean }>;
  is_blocked: boolean;
  blocked_by?: string[];
  attempts: number;
  claim_incremented_attempts: boolean;
  last_attempt_failed: boolean;
  failure_count: number;
  stage_entered_at: string;
  created_at: string;
  updated_at: string;
};

function task(short_id: string, title: string, stage: string): E2ETask {
  return {
    id: `uuid-${short_id}`, short_id, title, stage, project_id: PROJECT_A, epic_id: "epic-1",
    description: "", paused: false, project_name: "Apollo", epic_name: "E1", tags: [],
    acceptance_criteria: [], is_blocked: false, attempts: 0, claim_incremented_attempts: false,
    last_attempt_failed: false, failure_count: 0, stage_entered_at: "2026-10-10T00:00:00Z",
    created_at: "2026-10-10T00:00:00Z", updated_at: "2026-10-10T00:00:00Z",
  };
}

const TASKS = [
  task("tsk-aaaa0001", "Write charter", "backlog"),
  task("tsk-bbbb0002", "Implement engine", "in_progress"),
  task("tsk-cccc0003", "Review drag arm", "in_review"),
  task("tsk-dddd0004", "Ship design doc", "done"),
  task("tsk-eeee0005", "Second backlog story", "backlog"),
  task("tsk-ffff0006", "Second in-progress story", "in_progress"),
];

async function openBoard(page: Page) {
  const j = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });
  await page.route(`${API}/auth/me`, (r: Route) => r.fulfill(j({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true })));
  await page.route(`${API}/auth/config`, (r: Route) => r.fulfill(j({ registrationEnabled: false, oidcEnabled: false })));
  await page.route(`${API}/workspaces`, (r: Route) =>
    r.fulfill(j({
      items: [{ id: WORKSPACE_ID, name: "Board E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }],
      pagination: { limit: 50, offset: 0, total: 1 },
    })));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill(j({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } })));
  await page.route(`${API}/me/projects`, (r: Route) =>
    r.fulfill(j({ projects: [{ id: PROJECT_A, short_id: PROJECT_A, user_id: "u1", name: "Apollo", target_workspace_id: WORKSPACE_ID, worker_slots: 2, tags: [], status: "active", created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-01T00:00:00Z" }] })));
  await page.route(`${API}/me/projects/*/tasks*`, (r: Route) => r.fulfill(j({ tasks: TASKS })));
  await page.route(`${API}/me/projects/*/epics`, (r: Route) => r.fulfill(j({ epics: [] })));
  await page.goto("/board");
  await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();
  // The mobile shell AUTO-OPENS the navigation drawer on session-less
  // routes (AppShell's mount effect) — and whether it lands open is a
  // race (matchMedia resolution vs the effect's first run). An open
  // drawer covers the board with its overlay and its swipe-to-close
  // gesture claims horizontal drags — the rail would never pan.
  // Deterministically close it before any touch.
  const closeMenu = page.getByRole("button", { name: "Close menu" });
  if (await closeMenu.isVisible()) {
    await closeMenu.click();
    await expect(closeMenu).toBeHidden();
  }
  return page.getByTestId(`stage-grid-${PROJECT_A}`);
}

// --- trusted touch helpers -------------------------------------------------------

/** Real touchscreen swipe via CDP — trusted input, real hit-testing, real
 * default actions (native pan). 5px move steps keep the gesture in the
 * ~300px/s regime where the delivered scroll is ~0.85× the drag
 * distance (touch slop eats the rest — measured: dist 120 over 24
 * steps scrolls 105), and the stationary tail zeroes the release
 * velocity so no fling carries the rail past the drag endpoint. */
async function touchSwipe(page: Page, fromX: number, toX: number, y: number) {
  const dist = Math.abs(toX - fromX);
  const steps = Math.max(4, Math.ceil(dist / 5));
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: fromX, y }] });
  for (let i = 1; i <= steps; i++) {
    const x = fromX + ((toX - fromX) * i) / steps;
    await cdp.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x, y }] });
    await page.waitForTimeout(16);
  }
  // Deceleration tail: hold the endpoint across several move events.
  for (let i = 0; i < 3; i++) {
    await cdp.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: toX, y }] });
    await page.waitForTimeout(30);
  }
  await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await cdp.detach();
}

/** The rail's vertical center (mid-card area — the natural thumb band). */
async function railCenterY(rail: Locator) {
  const box = await rail.boundingBox();
  if (!box) throw new Error("rail not visible");
  return box.y + box.height / 2;
}

/** Every column's valid snap range, in MAIN_STAGES order. */
async function allSnapRanges(rail: Locator) {
  return rail.evaluate((el) => {
    const railEl = el as HTMLElement;
    const cs = getComputedStyle(railEl);
    const padLeft = parseFloat(cs.paddingLeft) || 0;
    const padRight = parseFloat(cs.paddingRight) || 0;
    const snapportW = railEl.clientWidth - padLeft - padRight;
    const maxScroll = railEl.scrollWidth - railEl.clientWidth;
    const cols = Array.from(railEl.querySelectorAll("[data-stage-col]")) as HTMLElement[];
    return cols.map((col) => {
      const lo = col.offsetLeft - railEl.offsetLeft - padLeft;
      const over = Math.max(0, col.offsetWidth - snapportW);
      return { stage: col.getAttribute("data-stage-col")!, lo, hi: Math.min(lo + over, maxScroll) };
    });
  });
}

/** A swipe start point whose hit target is safe to start a PAN from:
 * not a card ([data-task-card] elements are draggable — Chromium claims
 * long-press drags on draggable elements and suppresses the scroll),
 * not a button/link. Scans outward from the ideal x (and into the
 * header band) for the nearest rail-owned point (gutters, padding,
 * column wrappers). */
async function safeSwipeStart(page: Page, idealX: number, y: number, box: { x: number; y: number; width: number }) {
  const okAt = (x: number, yy: number) =>
    page.evaluate(
      (pt) => {
        const el = document.elementFromPoint(pt.x, pt.y);
        if (!el) return false;
        return el.closest("[draggable='true'], [data-task-card], button, a") === null;
      },
      { x, y: yy },
    );
  // Never start inside the sidebar's 30px edge zone (screen-left): the
  // edge gesture CLAIMS any touch that begins there, however safe the
  // hit target itself is (the rail begins at x≈13 — inside the zone).
  const minX = Math.max(box.x + 6, 34);
  const clamp = (x: number) => Math.min(box.x + box.width - 6, Math.max(minX, x));
  const candidates: Array<[number, number]> = [];
  for (const yy of [y, box.y + 10]) {
    for (let d = 0; d <= 60; d += 5) {
      candidates.push([clamp(idealX + d), yy]);
      if (d > 0) candidates.push([clamp(idealX - d), yy]);
    }
  }
  for (const [x, yy] of candidates) {
    if (await okAt(x, yy)) return { x, y: yy };
  }
  return { x: clamp(idealX), y };
}

/** A REAL touch swipe sized from live geometry so the snap SETTLE lands
 * on a chosen column. Mandatory snap picks whichever snap point the
 * endpoint is nearest — the endpoint must clear the midpoint between
 * the target's range and its neighbor's ((neighbor.hi + target.lo)/2,
 * ranges because a column wider than the snapport snaps across a
 * span), with margin for the ~0.85× delivery ratio. Gated on LAYOUT
 * STABILITY first: the epic-band fetch reflows the board after mount,
 * and a box read before it settles puts the touch start OFF the rail
 * (measured: off-rail touches pan nothing — the flake source). The
 * start point is additionally clamped INSIDE the rail and moved off
 * draggable cards (safeSwipeStart). */
async function swipeToward(page: Page, rail: Locator, stage: string) {
  const geometry = () =>
    rail.evaluate((el) => {
      const railEl = el as HTMLElement;
      const b = railEl.getBoundingClientRect();
      const cols = Array.from(railEl.querySelectorAll("[data-stage-col]"))
        .map((c) => `${c.getAttribute("data-stage-col")}:${Math.round(c.getBoundingClientRect().width * 4)}`)
        .join("|");
      return `${Math.round(b.x)},${Math.round(b.y)},${Math.round(b.width)};${cols}`;
    });
  let prevGeo = await geometry();
  for (let i = 0; i < 20; i++) {
    await page.waitForTimeout(150);
    const curGeo = await geometry();
    if (curGeo === prevGeo) break;
    prevGeo = curGeo;
  }
  const box = await rail.boundingBox();
  if (!box) throw new Error("rail not visible");
  const y = box.y + box.height / 2;
  const ranges = await allSnapRanges(rail);
  const idx = ranges.findIndex((r) => r.stage === stage);
  if (idx === -1) throw new Error(`column ${stage} not found`);
  const cur = await rail.evaluate((el) => el.scrollLeft);
  const target = ranges[idx]!;
  // The neighbor whose shared boundary the endpoint must cross.
  const boundary =
    target.lo > cur
      ? (ranges[idx - 1]!.hi + target.lo) / 2 + 20
      : (target.hi + ranges[idx + 1]!.lo) / 2 - 20;
  // Drag sized so the delivered scroll (~0.85×) lands past the boundary.
  const needed = Math.abs(boundary - cur) / 0.85 + 20;
  const drag = Math.min(370, Math.max(40, needed));
  // Reaching a target with a LARGER offset means swiping LEFTWARD
  // (the content travels left, scrollLeft grows) — and the swipe must
  // START inside the rail on the side it travels toward, never in the
  // sidebar's 30px edge zone at the screen's left edge.
  const leftward = target.lo > cur;
  const idealX = leftward ? Math.min(box.x + box.width - 10, 195 + drag / 2) : Math.max(box.x + 10, 195 - drag / 2);
  const start = await safeSwipeStart(page, idealX, y, box);
  const fromX = start.x;
  const toX = leftward ? Math.max(2, fromX - drag) : Math.min(388, fromX + drag);
  await touchSwipe(page, fromX, toX, start.y);
}

/** The VALID snap range for a column under scroll-px-2. The snapport
 * origin (scrollLeft + paddingLeft) lands on the column's content
 * start at `lo`. When the column is WIDER than the snapport (done's
 * full page), CSS snap admits every position where the area still
 * covers the snapport — up to `lo + (width − snapport)`, clamped to
 * maxScroll. Computed from live layout, never hand-derived. */
async function snapRange(rail: Locator, stage: string) {
  return rail.evaluate((el, s) => {
    const railEl = el as HTMLElement;
    const col = railEl.querySelector(`[data-stage-col="${s}"]`) as HTMLElement | null;
    if (!col) throw new Error(`column ${s} not found`);
    const cs = getComputedStyle(railEl);
    const padLeft = parseFloat(cs.paddingLeft) || 0;
    const padRight = parseFloat(cs.paddingRight) || 0;
    const snapportW = railEl.clientWidth - padLeft - padRight;
    const lo = col.offsetLeft - railEl.offsetLeft - padLeft;
    const over = Math.max(0, col.offsetWidth - snapportW);
    const maxScroll = railEl.scrollWidth - railEl.clientWidth;
    return { lo, hi: Math.min(lo + over, maxScroll) };
  }, stage);
}

/** Wait until mandatory snap has SETTLED the rail inside the column's
 * valid snap range (±1px for sub-pixel rounding). The range itself is
 * only meaningful once the column GEOMETRY is stable — mode changes
 * animate column widths over 200ms, and a range computed mid-flight
 * brackets a snap point that no longer exists — so bounds stability
 * (two identical reads 150ms apart) gates the settle check. */
async function expectSettledOn(rail: Locator, stage: string) {
  const boundsFingerprint = () =>
    rail.evaluate((el) =>
      Array.from(el.querySelectorAll("[data-stage-col]"))
        .map((c) => `${c.getAttribute("data-stage-col")}:${Math.round(c.getBoundingClientRect().width * 4)}`)
        .join("|"));
  const deadline = Date.now() + 8000;
  let prevBounds = await boundsFingerprint();
  while (Date.now() < deadline) {
    await rail.page().waitForTimeout(150);
    const curBounds = await boundsFingerprint();
    if (curBounds === prevBounds) {
      const { lo, hi } = await snapRange(rail, stage);
      const sl = await rail.evaluate((el) => el.scrollLeft);
      if (sl >= lo - 1 && sl <= hi + 1) return { lo, hi };
    }
    prevBounds = curBounds;
  }
  const { lo, hi } = await snapRange(rail, stage);
  const sl = await rail.evaluate((el) => el.scrollLeft);
  throw new Error(`rail did not settle on ${stage} within [${lo - 1}, ${hi + 1}] — last scrollLeft ${sl}`);
}

/** The rail's layout signature for orientation assertions: column
 * widths as fractions of the rail's content width, plus the mode and
 * whether the rail overflows. */
async function layoutSignature(rail: Locator) {
  return rail.evaluate((el) => {
    const railEl = el as HTMLElement;
    const cs = getComputedStyle(railEl);
    const pad = parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight);
    const contentW = railEl.getBoundingClientRect().width - pad;
    const cols = Array.from(railEl.querySelectorAll("[data-stage-col]")) as HTMLElement[];
    const fractions: Record<string, number> = {};
    for (const c of cols) fractions[c.getAttribute("data-stage-col")!] = c.getBoundingClientRect().width / contentW;
    return {
      fractions,
      mode: railEl.getAttribute("data-mode") ?? "",
      overflow: railEl.scrollWidth - railEl.clientWidth,
    };
  });
}

/** Wait for the rail's layout to go STABLE after a viewport change (the
 * portrait↔md reflow animates through transition-[width], and the
 * epic-band/sidebar reflow shifts the box) — two identical signature
 * reads 200ms apart. */
async function stableLayout(page: Page, rail: Locator) {
  const deadline = Date.now() + 8000;
  let prev = JSON.stringify(await layoutSignature(rail));
  while (Date.now() < deadline) {
    await page.waitForTimeout(200);
    const cur = JSON.stringify(await layoutSignature(rail));
    if (cur === prev) return JSON.parse(cur) as Awaited<ReturnType<typeof layoutSignature>>;
    prev = cur;
  }
  return (await layoutSignature(rail)) as Awaited<ReturnType<typeof layoutSignature>>;
}

/** The portrait rail geometry (owner direction 2026-11-15): the trio
 * shares ~95% (29/36/30 class weights), done is a FULL content page
 * (its 5% peek is viewport framing, never content width), and the rail
 * overflows so the snap carousel is live. */
function expectPortraitLayout(sig: Awaited<ReturnType<typeof layoutSignature>>) {
  expect(sig.mode).toBe("overview");
  expect(sig.overflow).toBeGreaterThan(100);
  expect(sig.fractions.backlog!).toBeGreaterThan(0.24);
  expect(sig.fractions.backlog!).toBeLessThan(0.31);
  expect(sig.fractions.in_progress!).toBeGreaterThan(0.31);
  expect(sig.fractions.in_progress!).toBeLessThan(0.38);
  expect(sig.fractions.in_review!).toBeGreaterThan(0.25);
  expect(sig.fractions.in_review!).toBeLessThan(0.32);
  expect(sig.fractions.done!).toBeGreaterThan(0.95);
}

/** The md+ rail geometry: 24/28/24/24 splits the row — all four
 * visible, no overflow, no lens. */
function expectLandscapeLayout(sig: Awaited<ReturnType<typeof layoutSignature>>) {
  expect(sig.mode).toBe("overview");
  expect(sig.overflow).toBeLessThanOrEqual(1);
  for (const stage of ["backlog", "in_progress", "in_review", "done"]) {
    expect(sig.fractions[stage]!).toBeGreaterThan(0.2);
    expect(sig.fractions[stage]!).toBeLessThan(0.31);
  }
}

type RailGeom = {
  scrollLeft: number;
  mode: string;
  colStates: Record<string, string>;
  colWidths: Record<string, number>;
  doneLeft: number | null;
  doneRight: number | null;
  railWidth: number;
};

async function railGeom(rail: Locator): Promise<RailGeom> {
  return rail.evaluate((el) => {
    const cols = Array.from(el.querySelectorAll("[data-stage-col]")) as HTMLElement[];
    const colStates: Record<string, string> = {};
    const colWidths: Record<string, number> = {};
    for (const c of cols) {
      const s = c.getAttribute("data-stage-col")!;
      colStates[s] = c.getAttribute("data-col-state") ?? "";
      colWidths[s] = c.getBoundingClientRect().width;
    }
    const done = el.querySelector('[data-stage-col="done"]') as HTMLElement | null;
    const r = done?.getBoundingClientRect() ?? null;
    return {
      scrollLeft: el.scrollLeft,
      mode: el.getAttribute("data-mode") ?? "",
      colStates,
      colWidths,
      doneLeft: r?.left ?? null,
      doneRight: r?.right ?? null,
      railWidth: el.getBoundingClientRect().width,
    };
  });
}

// --- the pins --------------------------------------------------------------------

test.describe("board rail touch scrolling (the v0.44.15 owner report)", () => {
  test("born-red pin: a real touchscreen swipe pans the rail and done becomes FULLY visible", async ({ page }) => {
    const rail = await openBoard(page);
    await expect(rail).toBeVisible();

    // The rail genuinely overflows (done is a full content page — the
    // 95/5 split is the VIEWPORT framing, never the content width).
    const overflow = await rail.evaluate((el) => el.scrollWidth - el.clientWidth);
    expect(overflow).toBeGreaterThan(100);

    // The swipe that did nothing on .15: leftward, mid-rail, real touch,
    // sized from live geometry to carry the settle onto done.
    await swipeToward(page, rail, "done");

    // The pan happened at all…
    await expect
      .poll(() => rail.evaluate((el) => el.scrollLeft), { timeout: 5000 })
      .toBeGreaterThan(100);
    // …mandatory snap settled exactly on done's start…
    await expectSettledOn(rail, "done");
    // …and done is FULLY inside the viewport (not a 5% sliver).
    const geom = await railGeom(rail);
    expect(geom.doneLeft).not.toBeNull();
    expect(geom.doneLeft!).toBeGreaterThanOrEqual(-0.5);
    expect(geom.doneRight!).toBeLessThanOrEqual(390.5);
  });

  test("snap lands per column: successive swipes settle exactly on each column's start, both directions", async ({ page }) => {
    const rail = await openBoard(page);

    // Geometry-sized swipes advance exactly one snap point each.
    await swipeToward(page, rail, "in_progress");
    await expectSettledOn(rail, "in_progress");

    await swipeToward(page, rail, "in_review");
    await expectSettledOn(rail, "in_review");

    await swipeToward(page, rail, "done");
    await expectSettledOn(rail, "done");

    // And back: rightward swipes return through the same snap points.
    await swipeToward(page, rail, "in_review");
    await expectSettledOn(rail, "in_review");

    await swipeToward(page, rail, "backlog");
    await expectSettledOn(rail, "backlog");
    const geom = await railGeom(rail);
    expect(geom.mode).toBe("overview");
  });

  test("keyboard parity on the phone viewport: ArrowRight ×3 reaches done and focuses it", async ({ page }) => {
    const rail = await openBoard(page);

    await rail.evaluate((el) => {
      (el.querySelector('[data-stage-col="backlog"]') as HTMLElement).focus();
    });
    for (const _stage of ["in_progress", "in_review", "done"]) {
      await page.keyboard.press("ArrowRight");
      // smooth scrollIntoView settles, then the next key.
      await page.waitForTimeout(450);
    }

    const geom = await railGeom(rail);
    // Done is fully in view and DOM focus sits on the done column.
    expect(geom.doneLeft!).toBeGreaterThanOrEqual(-0.5);
    expect(geom.doneRight!).toBeLessThanOrEqual(390.5);
    const focusedStage = await rail.evaluate((el) => (el.ownerDocument.activeElement as HTMLElement | null)?.getAttribute("data-stage-col"));
    expect(focusedStage).toBe("done");
  });

  test("focus-column mode under real touch: chevron tap enters, swipe TRANSITS the lens on snap-settle, Escape exits re-centered", async ({ page }) => {
    const rail = await openBoard(page);

    // Overview default on mount (2079).
    expect((await railGeom(rail)).mode).toBe("overview");

    // Enter focus on in_progress via the header chevron (a real tap).
    const chevron = page.locator('[data-stage-col="in_progress"] [data-testid="col-focus-toggle-in_progress"]');
    await chevron.tap();
    await expect(rail).toHaveAttribute("data-mode", "focus");
    let geom = await railGeom(rail);
    expect(geom.colStates.in_progress).toBe("focused");
    expect(geom.colStates.backlog).toBe("compressed");
    expect(geom.colStates.done).toBe("periphery");
    // The mode change animates (one 200ms width transition) and the
    // explicit re-snap fires just past it — the settle below doubles as
    // the pin for that re-snap (2079: the expanded column fully in
    // view). Widths are only meaningful once it lands.
    await expectSettledOn(rail, "in_progress");
    geom = await railGeom(rail);
    // The focused column takes the width (70%), compressed neighbors are stubs.
    expect(geom.colWidths.in_progress!).toBeGreaterThan(geom.colWidths.backlog! * 3);

    // The swipe transit (the 2079 core that was dead on .15): a swipe in
    // focus mode settles on the neighbor and the LENS follows the settle.
    // Geometry-sized: focus mode's hop to the neighbor is far larger than
    // a column stub (the focused column owns ~70% of the rail), so the
    // drag is scaled off the measured boundary to in_review's snap point.
    await swipeToward(page, rail, "in_review");
    await expect
      .poll(async () => (await railGeom(rail)).colStates.in_review, { timeout: 5000 })
      .toBe("focused");
    geom = await railGeom(rail);
    expect(geom.colStates.in_progress).toBe("compressed");
    expect(geom.colStates.done).toBe("periphery");

    // Escape exits to overview, re-centered on the column just left.
    await page.keyboard.press("Escape");
    await expect(rail).toHaveAttribute("data-mode", "overview");
    await expectSettledOn(rail, "in_review");
  });

  test("landscape regression (md+): all four columns visible, nothing to swipe, no focus affordances", async ({ page }) => {
    const rail = await openBoard(page);
    await page.setViewportSize({ width: 844, height: 390 });

    // The portrait→md resize animates: the columns carry
    // transition-[width] (200ms), so done is mid-flight from its
    // portrait full page (~546px) toward its md basis for a moment.
    // Threshold polls can pass mid-transition — wait for the geometry
    // to go STABLE (two identical reads 250ms apart) before measuring.
    const railFingerprint = () =>
      rail.evaluate((el) =>
        Array.from(el.querySelectorAll("[data-stage-col]"))
          .map((c) => Math.round(c.getBoundingClientRect().width * 10))
          .join(","));
    let prev = await railFingerprint();
    for (let i = 0; i < 20; i++) {
      await page.waitForTimeout(250);
      const cur = await railFingerprint();
      if (cur === prev) break;
      prev = cur;
    }

    // One row, no horizontal scroll: the rail fits its content.
    const overflow = await rail.evaluate((el) => el.scrollWidth - el.clientWidth);
    expect(overflow).toBeLessThanOrEqual(1);

    // All four columns fully inside the viewport — done included, no
    // edge-peek sliver at landscape.
    const boxes = await rail.evaluate((el) => {
      const vw = el.ownerDocument.defaultView!.innerWidth;
      const cols = Array.from(el.querySelectorAll("[data-stage-col]")) as HTMLElement[];
      return Object.fromEntries(
        cols.map((c) => {
          const r = c.getBoundingClientRect();
          return [c.getAttribute("data-stage-col"), { left: r.left, right: r.right, vw }];
        }),
      );
    });
    for (const [stage, b] of Object.entries(boxes)) {
      const box = b as { left: number; right: number; vw: number };
      expect(box.left, `${stage} left inside viewport`).toBeGreaterThanOrEqual(-0.5);
      expect(box.right, `${stage} right inside viewport`).toBeLessThanOrEqual(box.vw + 0.5);
    }

    // No focus-mode entry points at md+ (the chevrons are md:hidden —
    // display:none, but still in the DOM: assert hidden, not absent)…
    for (const stage of ["backlog", "in_progress", "in_review", "done"]) {
      await expect(page.getByTestId(`col-focus-toggle-${stage}`)).toBeHidden();
    }
    // …and a swipe across the rail pans nothing (there is no overflow).
    const y = await railCenterY(rail);
    await touchSwipe(page, 400, 100, y);
    await page.waitForTimeout(300);
    expect(await rail.evaluate((el) => el.scrollLeft)).toBe(0);
  });

  test("orientation round-trips (owner report #2): portrait↔landscape restores the exact layout per class, rapid rotations converge, the lens resets", async ({ page }) => {
    const rail = await openBoard(page);

    // The portrait baseline (fresh mount).
    const portrait0 = await stableLayout(page, rail);
    expectPortraitLayout(portrait0);

    // portrait → landscape: the md row.
    await page.setViewportSize({ width: 844, height: 390 });
    expectLandscapeLayout(await stableLayout(page, rail));

    // landscape → portrait: the EXACT portrait layout returns (the
    // owner report: the 95/3+peek layout "does not re-apply").
    await page.setViewportSize({ width: 390, height: 844 });
    const portrait1 = await stableLayout(page, rail);
    expectPortraitLayout(portrait1);
    for (const stage of ["backlog", "in_progress", "in_review", "done"]) {
      expect(portrait1.fractions[stage]!).toBeCloseTo(portrait0.fractions[stage]!, 1);
    }

    // Rapid repeated rotations (the reflow animates each crossing — the
    // final state must still converge to the correct layout).
    for (const [w, h] of [
      [844, 390], [390, 844], [844, 390], [390, 844],
    ] as const) {
      await page.setViewportSize({ width: w, height: h });
      await page.waitForTimeout(60);
    }
    expectPortraitLayout(await stableLayout(page, rail));

    // The lens resets across the viewport-class change: enter focus at
    // portrait, rotate to md+ — overview, no focus widths anywhere —
    // and back: STILL overview with the portrait layout (a lens that
    // survives the rotation is the owner's wrong-sizes symptom).
    await page.locator('[data-stage-col="in_progress"] [data-testid="col-focus-toggle-in_progress"]').tap();
    await expect(rail).toHaveAttribute("data-mode", "focus");
    await page.setViewportSize({ width: 844, height: 390 });
    const landAfterLens = await stableLayout(page, rail);
    expectLandscapeLayout(landAfterLens);
    for (const stage of ["backlog", "in_progress", "in_review", "done"]) {
      await expect(page.getByTestId(`col-focus-toggle-${stage}`)).toBeHidden();
    }
    await page.setViewportSize({ width: 390, height: 844 });
    const portraitAfterLens = await stableLayout(page, rail);
    expectPortraitLayout(portraitAfterLens);
  });
});
