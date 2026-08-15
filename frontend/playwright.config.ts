import { defineConfig } from "@playwright/test";

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
    baseURL: "http://localhost:5173",
    headless: true,
    screenshot: "only-on-failure",
  },
  webServer: {
    command: "npm run dev",
    port: 5173,
    reuseExistingServer: true,
    timeout: 10_000,
  },
});
