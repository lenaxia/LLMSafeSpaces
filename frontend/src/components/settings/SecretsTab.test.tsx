// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Tests for the CreateSecretForm inside SecretsTab.
 *
 * Regression coverage for the bug where the form prepended
 * "/home/sandbox/.secrets/" to mount_path before sending to the API,
 * causing the backend's validateMountPath to reject the request with
 * HTTP 400 (it only accepts relative paths).
 *
 * Also covers the legacyOnly rendering path for "api-key" typed secrets:
 * these must be visible in the list (with a migration banner) and absent
 * from the create dropdown.
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { SecretsTab } from "./SecretsTab";
import * as secretsApiModule from "../../api/secrets";

// Mock the secrets API module so tests don't make real HTTP calls.
vi.mock("../../api/secrets", () => ({
  secretsApi: {
    list: vi.fn().mockResolvedValue({ secrets: [] }),
    create: vi.fn().mockResolvedValue({
      id: "sec-1",
      name: "my-cert",
      type: "secret-file",
      metadata: { mount_path: "cert.pem" },
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    }),
    delete: vi.fn().mockResolvedValue(undefined),
    getSecretBindings: vi.fn().mockResolvedValue({ workspaces: [] }),
  },
}));

// The ToastProvider is used inside SecretsTab; provide a stub.
vi.mock("../../providers/ToastProvider", () => ({
  useToast: () => ({ toast: vi.fn() }),
}));

describe("CreateSecretForm – mount_path handling", () => {
  const createMock = secretsApiModule.secretsApi.create as ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.clearAllMocks();
    // Re-apply defaults after clearAllMocks
    (secretsApiModule.secretsApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ secrets: [] });
    (secretsApiModule.secretsApi.getSecretBindings as ReturnType<typeof vi.fn>).mockResolvedValue({ workspaces: [] });
    createMock.mockResolvedValue({
      id: "sec-1",
      name: "my-cert",
      type: "secret-file",
      metadata: { mount_path: "cert.pem" },
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    });
  });

  async function openCreateForm() {
    const user = userEvent.setup();
    render(<SecretsTab />);
    // Wait for the loading state to resolve
    await waitFor(() => expect(screen.queryByText("Loading secrets...")).not.toBeInTheDocument());
    // Open the creation form
    await user.click(screen.getByRole("button", { name: "+ New Secret" }));
    return user;
  }

  async function selectSecretType(user: ReturnType<typeof userEvent.setup>, typeName: string) {
    // The create form may have multiple <select> elements (type selector + metaField
    // selectors like provider/key_type). The type selector is always the first combobox.
    const [typeSelect] = screen.getAllByRole("combobox");
    await user.selectOptions(typeSelect!, typeName);
  }

  it("sends mount_path as a plain relative path (no /home/sandbox/.secrets/ prefix)", { timeout: 15000 }, async () => {
    const user = await openCreateForm();
    await selectSecretType(user, "secret-file");

    // Fill required Name field
    await user.type(screen.getByPlaceholderText("my-api-key"), "my-cert");

    // Fill required Value textarea
    const valueArea = screen.getByPlaceholderText(/sk-/i);
    await user.type(valueArea, "CERTIFICATE_CONTENTS");

    // Fill the mount_path field (the relative portion only, e.g. "cert.pem")
    await user.type(screen.getByPlaceholderText("cert.pem"), "cert.pem");

    // Submit
    await user.click(screen.getByRole("button", { name: "Create Secret" }));

    await waitFor(() => expect(createMock).toHaveBeenCalled(), { timeout: 10000 });

    const callArg = createMock.mock.calls[0]![0] as { metadata?: { mount_path?: string } };
    const sentPath = callArg.metadata?.mount_path ?? "";

    // The sent value must be relative — no absolute path prefix.
    expect(sentPath).toBe("cert.pem");
    expect(sentPath).not.toMatch(/^\//);
    expect(sentPath).not.toContain("/home/sandbox/.secrets/");
  });

  it("strips leading slashes from mount_path input via the onChange sanitizer", { timeout: 15000 }, async () => {
    const user = await openCreateForm();
    await selectSecretType(user, "secret-file");

    await user.type(screen.getByPlaceholderText("my-api-key"), "my-cert");
    const valueArea = screen.getByPlaceholderText(/sk-/i);
    await user.type(valueArea, "cert-data");

    // Simulate the user somehow typing an absolute path into the relative input.
    // The onChange handler calls `.replace(/^\/+/, "")` so the leading slash
    // must be stripped before the value reaches state.
    const mountInput = screen.getByPlaceholderText("cert.pem");
    await user.type(mountInput, "/absolute/path.pem");

    await user.click(screen.getByRole("button", { name: "Create Secret" }));
    await waitFor(() => expect(createMock).toHaveBeenCalled(), { timeout: 10000 });

    const callArg = createMock.mock.calls[0]![0] as { metadata?: { mount_path?: string } };
    const sentPath = callArg.metadata?.mount_path ?? "";

    // Leading slash must have been stripped.
    expect(sentPath).not.toMatch(/^\//);
  });

  it("strips ../ traversal sequences from mount_path input via the onChange sanitizer", { timeout: 15000 }, async () => {
    const user = await openCreateForm();
    await selectSecretType(user, "secret-file");

    await user.type(screen.getByPlaceholderText("my-api-key"), "traversal-test");
    const valueArea = screen.getByPlaceholderText(/sk-/i);
    await user.type(valueArea, "data");

    // The onChange handler calls `.replace(/\.\.\//g, "")` to strip "../"
    const mountInput = screen.getByPlaceholderText("cert.pem");
    await user.type(mountInput, "../../etc/passwd");

    await user.click(screen.getByRole("button", { name: "Create Secret" }));
    await waitFor(() => expect(createMock).toHaveBeenCalled(), { timeout: 10000 });

    const callArg = createMock.mock.calls[0]![0] as { metadata?: { mount_path?: string } };
    const sentPath = callArg.metadata?.mount_path ?? "";

    // "../" sequences must have been removed from the value stored in state.
    expect(sentPath).not.toContain("../");
  });
});

// ─── legacyOnly rendering ────────────────────────────────────────────────────

describe("SecretsTab – legacy api-key type rendering", () => {
  const listMock = secretsApiModule.secretsApi.list as ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.clearAllMocks();
    (secretsApiModule.secretsApi.getSecretBindings as ReturnType<typeof vi.fn>).mockResolvedValue({ workspaces: [] });
    (secretsApiModule.secretsApi.create as ReturnType<typeof vi.fn>).mockResolvedValue({});
  });

  it("renders the 'API Keys (legacy)' group when api-key secrets exist", async () => {
    listMock.mockResolvedValue({
      secrets: [
        {
          id: "legacy-1",
          name: "old-api-key",
          type: "api-key",
          metadata: {},
          createdAt: new Date().toISOString(),
          updatedAt: new Date().toISOString(),
        },
      ],
    });

    render(<SecretsTab />);
    await waitFor(() => expect(screen.queryByText("Loading secrets...")).not.toBeInTheDocument());

    // The group header must be visible
    expect(screen.getByText("API Keys (legacy)")).toBeInTheDocument();
    // The secret name must be visible inside the group
    expect(screen.getByText("old-api-key")).toBeInTheDocument();
  });

  it("shows the deprecation banner with the sunset date in the api-key group", async () => {
    listMock.mockResolvedValue({
      secrets: [
        {
          id: "legacy-2",
          name: "another-key",
          type: "api-key",
          metadata: {},
          createdAt: new Date().toISOString(),
          updatedAt: new Date().toISOString(),
        },
      ],
    });

    render(<SecretsTab />);
    await waitFor(() => expect(screen.queryByText("Loading secrets...")).not.toBeInTheDocument());

    // The banner phrase lives in a single text node within the amber <p>;
    // anchor on it then verify the full migrated-content via the paragraph.
    const bannerAnchor = screen.getByText(/secrets are deprecated/i);
    const banner = bannerAnchor.closest("p");
    expect(banner?.textContent).toContain("api-key");
    expect(banner?.textContent).toContain("llm-provider");
    expect(banner?.textContent).toContain("env-secret");
    expect(banner?.textContent).toContain("December 19, 2026");
  });

  it("does not show the api-key group when no api-key secrets exist", async () => {
    listMock.mockResolvedValue({
      secrets: [
        {
          id: "env-1",
          name: "my-env",
          type: "env-secret",
          metadata: { var_name: "DATABASE_URL" },
          createdAt: new Date().toISOString(),
          updatedAt: new Date().toISOString(),
        },
      ],
    });

    render(<SecretsTab />);
    await waitFor(() => expect(screen.queryByText("Loading secrets...")).not.toBeInTheDocument());

    expect(screen.queryByText("API Keys (legacy)")).not.toBeInTheDocument();
  });

  it("excludes api-key from the create dropdown", async () => {
    listMock.mockResolvedValue({ secrets: [] });

    render(<SecretsTab />);
    await waitFor(() => expect(screen.queryByText("Loading secrets...")).not.toBeInTheDocument());

    // Open the create form
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "+ New Secret" }));

    // All comboboxes in the form (first one is the type selector)
    const [typeSelect] = screen.getAllByRole("combobox");
    const options = Array.from(typeSelect!.querySelectorAll("option")).map((o) => o.textContent ?? "");

    // api-key should not appear in the dropdown
    expect(options.some((o) => o.toLowerCase().includes("api key") || o.toLowerCase().includes("api-key"))).toBe(false);
    // Standard types should be present
    expect(options.some((o) => o.toLowerCase().includes("llm provider"))).toBe(true);
    expect(options.some((o) => o.toLowerCase().includes("ssh key"))).toBe(true);
  });
});

// ─── ConfirmDialog (#814) ────────────────────────────────────────────────────

describe("SecretsTab – ConfirmDialog delete (#814)", () => {
  const listMock = secretsApiModule.secretsApi.list as ReturnType<typeof vi.fn>;
  const deleteMock = secretsApiModule.secretsApi.delete as ReturnType<typeof vi.fn>;

  const SECRET = {
    id: "sec-env-1",
    name: "DATABASE_URL",
    type: "env-secret",
    metadata: { var_name: "DATABASE_URL" },
    globalDefault: false,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
  } as const;

  beforeEach(() => {
    vi.clearAllMocks();
    listMock.mockResolvedValue({ secrets: [SECRET] });
    (secretsApiModule.secretsApi.getSecretBindings as ReturnType<typeof vi.fn>).mockResolvedValue({ workspaces: [] });
    deleteMock.mockResolvedValue(undefined);
  });

  it("opens the confirm dialog and deletes the secret on confirm", async () => {
    const user = userEvent.setup();
    render(<SecretsTab />);
    await waitFor(() => expect(screen.queryByText("Loading secrets...")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByText("DATABASE_URL")).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "Delete" }));

    // ConfirmDialog opens — title visible, and a second "Delete" button (the
    // dialog's confirm) is now present. The last matching button is the dialog's.
    await waitFor(() => expect(screen.getByText("Delete secret?")).toBeInTheDocument());
    const dialogConfirm = screen.getAllByRole("button", { name: "Delete" }).pop()!;
    await user.click(dialogConfirm);

    await waitFor(() => expect(deleteMock).toHaveBeenCalledWith("sec-env-1"));
  });

  it("does not delete when the dialog is cancelled", async () => {
    const user = userEvent.setup();
    render(<SecretsTab />);
    await waitFor(() => expect(screen.getByText("DATABASE_URL")).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "Delete" }));
    const cancelBtn = await screen.findByRole("button", { name: "Cancel" });
    await user.click(cancelBtn);

    expect(deleteMock).not.toHaveBeenCalled();
  });
});
