/**
 * E2E: failed tool executions render their error message in the transcript
 * (owner report 2026-11-12, tsk-copper-archer-rushes).
 *
 * Proves in a real browser (mocked API, real component tree) the property
 * jsdom structurally cannot assert — REAL visibility without expanding
 * anything. The bug class is "content behind a collapsed pane effectively
 * does not exist" (the 1eee7f28 lesson): if a future change wraps the
 * error block in a closed <details> or hides it via CSS, every jsdom pin
 * still passes while the error is invisible here — this spec is the gate.
 *
 *   Happy:   a history-loaded tool part with ToolState{status:"error",
 *            error} renders its error text visibly, no clicks, with the
 *            raw JSON body behind the "Raw output" toggle.
 *   Contrast (regression): a completed tool part in the same transcript
 *            renders NO error block — the success path is unchanged.
 *   Collapse: a long error payload shows its head inline with the full
 *            text behind a visible "Show full error" toggle; the tail is
 *            genuinely invisible until the toggle is clicked (a real
 *            browser assertion, unlike jsdom's ambiguous closed-details
 *            visibility).
 *
 * Mock shape mirrors the platform-session contract (dev-preview-button.spec.ts).
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WORKSPACE_ID = "ws-toolerror-e2e";
const SESSION_ID = "ses-toolerror-e2e";
const API = "**/api/v1";

const ERROR_MESSAGE = "HTTP 409 epic_state: epic epc-951a is not active — activate it first";

async function setupAPIMocks(page: Page, messages: unknown[]) {
  await page.route("**/env.json", (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ apiBaseUrl: "/api/v1", turnstileSiteKey: "" }) }));
  await page.route(`${API}/auth/login`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ token: "e2e-token", user: { id: "u1", username: "tester", role: "user" } }) }));
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) }));
  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { void r.continue(); return; }
    void r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "ToolError E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/activate`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ resumed: WORKSPACE_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions?*`, (r: Route) => {
    if (r.request().method() === "POST") {
      void r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) });
      return;
    }
    void r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "ToolError Test", messageCount: 2, status: "idle", hasUnread: false }]) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "ToolError Test" }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(messages) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
  await mockIdleContractStream(page, `${API}/workspaces/${WORKSPACE_ID}/contract-events`, SESSION_ID);
}

test.describe("failed tool error visibility (tsk-copper-archer-rushes)", () => {
  test("error-state tool renders its error visibly without expanding; completed tool does not (contrast)", async ({ page }) => {
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: "import the plan" }] },
      {
        id: "m2",
        type: "assistant",
        parts: [
          {
            type: "tool",
            tool: {
              callId: "c-err",
              name: "llmsafespaces_board_import_plan",
              input: { projectId: "prj-1", planFile: "plan.json" },
              state: { status: "error", error: ERROR_MESSAGE },
            },
          },
        ],
      },
      {
        id: "m3",
        type: "assistant",
        parts: [
          {
            type: "tool",
            tool: {
              callId: "c-ok",
              name: "bash",
              input: { command: "ls" },
              output: "file.go",
              state: { status: "completed" },
            },
          },
        ],
      },
    ]);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // THE GATE: the error text is visible in a real browser with ZERO
    // clicks — the bug's entire subject.
    const err = page.getByTestId("tool-error");
    await expect(err).toBeVisible({ timeout: 10_000 });
    await expect(err).toContainText("epic epc-951a is not active");
    // The failed tool's ✗ header renders alongside.
    await expect(page.getByText("✗ llmsafespaces_board_import_plan")).toBeVisible();
    // Raw payload absent on this shape (state error only, no output) —
    // no raw toggle, nothing fabricated.
    await expect(err.getByText(/Raw output/i)).toHaveCount(0);

    // CONTRAST (regression pin): the completed tool in the same
    // transcript renders NO error block — exactly one exists, the
    // failed one's.
    await expect(page.getByText("✓ bash")).toBeVisible();
    await expect(page.getByTestId("tool-error")).toHaveCount(1);
  });

  test("JSON error body extracts the message; the raw payload hides behind the Raw output toggle", async ({ page }) => {
    await setupAPIMocks(page, [
      {
        id: "m2",
        type: "assistant",
        parts: [
          {
            type: "tool",
            tool: {
              callId: "c-json",
              name: "llmsafespaces_board_epic_activate",
              input: { epicId: "epc-1" },
              output: `{"error":"invalid reference: no such role reviewerx","requestId":"req-9"}`,
              state: { status: "error" },
            },
          },
        ],
      },
    ]);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    const err = page.getByTestId("tool-error");
    await expect(err).toBeVisible({ timeout: 10_000 });
    await expect(err).toContainText("invalid reference: no such role reviewerx");
    // The extracted message shows; the raw JSON does NOT (real-browser
    // visibility — it is inside the closed toggle).
    await expect(err.getByText(/Raw output/i)).toBeVisible();
    await expect(err.getByText(/requestId/)).toBeHidden();
    // Opening the toggle reveals the raw payload.
    await err.getByText(/Raw output/i).click();
    await expect(err.getByText(/requestId/)).toBeVisible();
  });

  test("a long error payload collapses behind a visible Show-full-error affordance", async ({ page }) => {
    const longError = Array.from({ length: 30 }, (_, i) => `trace frame ${i + 1}: failure detail`).join("\n");
    await setupAPIMocks(page, [
      {
        id: "m2",
        type: "assistant",
        parts: [
          {
            type: "tool",
            tool: {
              callId: "c-long",
              name: "bash",
              input: { command: "x" },
              state: { status: "error", error: longError },
            },
          },
        ],
      },
    ]);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    const err = page.getByTestId("tool-error");
    await expect(err).toBeVisible({ timeout: 10_000 });
    // The affordance and the head are visible; the tail is genuinely
    // INVISIBLE until clicked (the jsdom-ambiguous property, asserted
    // for real here). The head pre is the block's first pre (the full
    // text inside the toggle also contains the head's lines — strict
    // mode needs the explicit scope).
    const affordance = err.getByText(/Show full error/i);
    await expect(affordance).toBeVisible();
    const headPre = err.locator("pre").first();
    await expect(headPre).toContainText("trace frame 3:");
    await expect(err.getByText(/trace frame 30:/)).toBeHidden();
    await affordance.click();
    await expect(err.getByText(/trace frame 30:/)).toBeVisible();
  });
});
