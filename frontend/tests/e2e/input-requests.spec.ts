/**
 * E2E test for Epic 16: Agent input requests (questions + permissions).
 *
 * Uses Playwright route interception to mock the backend. Tests the full
 * browser flow: contract-stream snapshot/events → prompt renders → user
 * interacts → API call fires.
 *
 * US-69.10 hard cutover: prompt content is projection-authoritative — the
 * fold's pendingInputs (seeded by /contract-events snapshots and kept
 * current by INPUT_REQUEST/INPUT_RESOLVED events) drive the QuestionPrompt
 * / PermissionPrompt UI. Answering still POSTs the input endpoints.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const WORKSPACE_ID = "ws-e2e-input";
const SESSION_ID = "ses_e2e_input";
const API_PREFIX = "**/api/v1";

// --- contract-frame builders (mirrors contract-stream.spec.ts) ---------------

function snapshotFrame(atSeq: number | bigint, sessions: Array<Record<string, unknown>>): string {
  return `data: ${JSON.stringify({
    snapshot: { atSeq: String(atSeq), snapshot: { sessions } },
  })}\n\n`;
}

function sessionState(pendingInputs: Array<Record<string, unknown>>): Record<string, unknown> {
  return { sessionId: SESSION_ID, status: "SESSION_STATUS_BUSY", inFlightParts: [], queueDepth: 0, pendingInputs };
}

function questionInput(id: string, question: string, header: string, options: Array<[string, string]>): Record<string, unknown> {
  return {
    id,
    sessionId: SESSION_ID,
    rootSessionId: SESSION_ID,
    kind: "INPUT_KIND_QUESTION",
    question,
    header,
    options: options.map(([label, description]) => ({ label, description })),
  };
}

function permissionInput(id: string, permission: string, patterns: string[]): Record<string, unknown> {
  return {
    id,
    sessionId: SESSION_ID,
    rootSessionId: SESSION_ID,
    kind: "INPUT_KIND_PERMISSION",
    permission,
    patterns,
    always: [],
  };
}

/**
 * Contract-stream mock. Hits 1-2 deliver `firstBody` (React StrictMode's
 * dev double-mount destroys connection #1 before its body is processed),
 * then behavior splits:
 *  - with `afterReply`: later connections hold until the gate resolves
 *    (the reply/reject POST), then deliver `afterBody` — deterministic
 *    fold cleanup once the prompt is answered;
 *  - without: later connections hold forever (no re-delivery churn).
 */
function mockContractStream(page: Page, firstBody: string, opts: { gate?: Promise<void>; afterBody?: string } = {}) {
  let hits = 0;
  return page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, async (route: Route) => {
    hits++;
    if (hits <= 2) {
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: firstBody });
      return;
    }
    if (opts.gate && opts.afterBody !== undefined) {
      await opts.gate;
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: opts.afterBody });
      return;
    }
    await new Promise<never>(() => {}); // hold — no further delivery
  });
}

function idleSnapshot(): string {
  return snapshotFrame(1, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_IDLE", inFlightParts: [], queueDepth: 0, pendingInputs: [] }]);
}

// Extra sessions (e.g. subtasks with parentId) that the NEXT sessions-list
// response should include — set by a test before navigation.
const sessionsExtra: Array<Record<string, unknown>> = [];

async function setupAPIMocks(page: Page) {
  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "testuser", email: "t@t.com", role: "user", active: true }) });
  });
  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) });
  });
  await page.route(`${API_PREFIX}/workspaces`, async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Input Test", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
    } else { await route.continue(); }
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/status`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" }, sessions: [{ id: SESSION_ID, status: "busy" }] }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions`, async (route: Route) => {
    const extra = sessionsExtra.splice(0, sessionsExtra.length);
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "Input Test", messageCount: 0, status: "busy" }, ...extra]) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/message`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) });
  });

  // Platform stream — platform events only after the US-69.10 cutover.
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-events`, async (route: Route) => {
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: ":\n\n" });
  });

  // Contract stream — default idle snapshot (tests override).
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, async (route: Route) => {
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: idleSnapshot() });
  });

  // Question/permission API endpoints
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/question/*/reply`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/question/*/reject`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/permission/*/reply`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
  });
}

test.describe("Epic 16: Agent input requests (mocked backend)", () => {
  test.beforeEach(async ({ page }) => {
    await setupAPIMocks(page);
  });

  test("question prompt renders when the fold snapshot carries a pendingInput", async ({ page }) => {
    // Pre-load the contract stream with a busy snapshot carrying the
    // standing question (the fold's pendingInputs seed the prompt UI).
    await mockContractStream(page, snapshotFrame(3, [sessionState([questionInput("que_e2e1", "Which database?", "Choose DB", [["PostgreSQL", "Relational"], ["MongoDB", "Document"]])])]));

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("Which database?")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByRole("button", { name: "PostgreSQL" })).toBeVisible();
    await expect(page.getByRole("button", { name: "MongoDB" })).toBeVisible();
  });

  test("user can select option and submit question answer", async ({ page }) => {
    let resolveReply!: () => void;
    const replyGate = new Promise<void>((resolve) => { resolveReply = resolve; });

    await mockContractStream(page, snapshotFrame(3, [sessionState([questionInput("que_e2e2", "Pick one", "Language", [["Go", "Fast"], ["Rust", "Safe"]])])]), {
      gate: replyGate,
      // After the reply POST the fold drops the input (production emits
      // INPUT_RESOLVED / re-snapshot without it) — the prompt stays gone.
      afterBody: idleSnapshot(),
    });

    let replyCalled = false;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/question/que_e2e2/reply`, async (route: Route) => {
      replyCalled = true;
      resolveReply();
      const body = await route.request().postDataJSON();
      expect(body.answers).toEqual([["Go"]]);
      await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("Pick one")).toBeVisible({ timeout: 10_000 });

    await page.getByRole("button", { name: "Go" }).click();
    await page.getByText("Submit answers").click();

    // Prompt should disappear after submit
    await expect(page.getByText("Pick one")).not.toBeVisible({ timeout: 10_000 });
    expect(replyCalled).toBe(true);
  });

  test("permission prompt renders and user can approve", async ({ page }) => {
    let resolveReply!: () => void;
    const replyGate = new Promise<void>((resolve) => { resolveReply = resolve; });

    await mockContractStream(page, snapshotFrame(3, [sessionState([permissionInput("per_e2e1", "shell", ["rm -rf /tmp/cache"])])]), {
      gate: replyGate,
      afterBody: idleSnapshot(),
    });

    let replyCalled = false;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/permission/per_e2e1/reply`, async (route: Route) => {
      replyCalled = true;
      resolveReply();
      const body = await route.request().postDataJSON();
      expect(body.reply).toBe("always");
      await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("Run shell command")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("rm -rf /tmp/cache")).toBeVisible();

    await page.getByText("Allow always").click();
    await expect(page.getByText("Run shell command")).not.toBeVisible({ timeout: 10_000 });
    expect(replyCalled).toBe(true);
  });

  test("permission deny shows feedback input", async ({ page }) => {
    let resolveReply!: () => void;
    const replyGate = new Promise<void>((resolve) => { resolveReply = resolve; });

    await mockContractStream(page, snapshotFrame(3, [sessionState([permissionInput("per_e2e2", "write", ["/etc/passwd"])])]), {
      gate: replyGate,
      afterBody: idleSnapshot(),
    });
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/permission/per_e2e2/reply`, async (route: Route) => {
      resolveReply();
      await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("Write file")).toBeVisible({ timeout: 10_000 });

    // First click shows feedback
    await page.getByText("Deny").click();
    await expect(page.getByLabel("Feedback")).toBeVisible();

    // Type feedback and confirm
    await page.getByLabel("Feedback").fill("Not safe");
    await page.getByText("Confirm deny").click();
    await expect(page.getByText("Write file")).not.toBeVisible({ timeout: 10_000 });
  });

  test("question dismiss calls reject API", async ({ page }) => {
    let resolveReject!: () => void;
    const rejectGate = new Promise<void>((resolve) => { resolveReject = resolve; });

    await mockContractStream(page, snapshotFrame(3, [sessionState([questionInput("que_e2e3", "Dismiss me", "Test", [["A", ""]])])]), {
      gate: rejectGate,
      afterBody: idleSnapshot(),
    });

    let rejectCalled = false;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/question/que_e2e3/reject`, async (route: Route) => {
      rejectCalled = true;
      resolveReject();
      await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("Dismiss me")).toBeVisible({ timeout: 10_000 });
    await page.locator("button", { hasText: /^Dismiss$/ }).click();
    await expect(page.getByText("Dismiss me")).not.toBeVisible({ timeout: 10_000 });
    expect(rejectCalled).toBe(true);
  });

  // Subtask bubbling: opencode's `task` tool spawns a subagent session whose
  // Subtask prompts after the US-69.10 cutover: prompt content comes from
  // the pod-wide contract fold; a subtask's input bubbles to the parent
  // view via the sessions list's parentId chain (the client-side root
  // resolution that the retired user-stream copies used to carry).
  test("subtask permission bubbles to parent session via the parentId chain", async ({ page }) => {
    const SUBTASK_ID = "ses_subtask_xyz";
    sessionsExtra.push({ id: SUBTASK_ID, parentId: SESSION_ID, title: "subtask", messageCount: 0, status: "busy" });
    await mockContractStream(page, snapshotFrame(3, [{
      sessionId: SUBTASK_ID,
      status: "SESSION_STATUS_BUSY",
      inFlightParts: [],
      queueDepth: 0,
      pendingInputs: [{ id: "per_subtask_e2e", sessionId: SUBTASK_ID, kind: "INPUT_KIND_PERMISSION", permission: "shell", patterns: ["ls /workspace"], always: [] }],
    }]));

    let replyCalled = false;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/permission/per_subtask_e2e/reply`, async (route: Route) => {
      replyCalled = true;
      await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
    });

    // Navigate to the PARENT session — without root resolution the prompt
    // would never appear here.
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("Run shell command")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("ls /workspace")).toBeVisible();

    await page.getByText("Allow always").click();
    await expect(page.getByText("Run shell command")).not.toBeVisible({ timeout: 5_000 });
    expect(replyCalled).toBe(true);
  });

  test("subtask question bubbles to parent session via the parentId chain", async ({ page }) => {
    const SUBTASK_ID = "ses_subtask_q";
    sessionsExtra.push({ id: SUBTASK_ID, parentId: SESSION_ID, title: "subtask", messageCount: 0, status: "busy" });
    await mockContractStream(page, snapshotFrame(3, [{
      sessionId: SUBTASK_ID,
      status: "SESSION_STATUS_BUSY",
      inFlightParts: [],
      queueDepth: 0,
      pendingInputs: [{ id: "que_subtask_e2e", sessionId: SUBTASK_ID, kind: "INPUT_KIND_QUESTION", question: "Proceed with refactor?", header: "Subagent confirm", options: [{ label: "Yes", description: "go" }, { label: "No", description: "stop" }], multiple: false }],
    }]));

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("Proceed with refactor?")).toBeVisible({ timeout: 10_000 });
  });

  test("subtask permission for a different parent tree is NOT shown", async ({ page }) => {
    // Two subtask trees coexist in the same workspace. The user is viewing
    // tree A (SESSION_ID); the input belongs to tree B. Must NOT render.
    sessionsExtra.push({ id: "ses_other_parent", title: "other tree", messageCount: 0, status: "busy" });
    sessionsExtra.push({ id: "ses_other_subtask", parentId: "ses_other_parent", title: "other subtask", messageCount: 0, status: "busy" });
    await mockContractStream(page, snapshotFrame(3, [{
      sessionId: "ses_other_subtask",
      status: "SESSION_STATUS_BUSY",
      inFlightParts: [],
      queueDepth: 0,
      pendingInputs: [{ id: "per_other_tree", sessionId: "ses_other_subtask", kind: "INPUT_KIND_PERMISSION", permission: "shell", patterns: ["echo hi"], always: [] }],
    }]));

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    // Wait long enough that any erroneous render would have happened.
    await page.waitForTimeout(2000);
    await expect(page.getByText("Run shell command")).not.toBeVisible();
  });
});
