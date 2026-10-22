import { defineConfig } from "@playwright/test";

// The dev-server port: 5173 (the Vite default) unless E2E_PORT pins a
// unique one — required when sibling worktrees run their suites
// concurrently on the same host (reuseExistingServer would otherwise
// latch onto a foreign server). CI leaves it unset.
const E2E_PORT = Number(process.env.E2E_PORT ?? 5173);

export default defineConfig({
  testDir: "./tests/e2e",
  timeout: 30_000,
  retries: 1,
  // Pin workers to the CI-true default: Playwright resolves an unset
  // workers to 50% of CPU cores, which is 2 on the 4-vCPU CI runners but
  // 18 on large local hosts — enough concurrent Chromiums to trip the
  // 30s timeouts from resource contention alone. Pinning to 2 keeps CI
  // behaviour identical and makes local runs deterministic.
  workers: 2,
  use: {
    baseURL: `http://localhost:${E2E_PORT}`,
    headless: true,
    screenshot: "only-on-failure",
  },
  webServer: {
    command: `npm run dev -- --port ${E2E_PORT} --strictPort`,
    port: E2E_PORT,
    reuseExistingServer: true,
    timeout: 10_000,
  },
});
