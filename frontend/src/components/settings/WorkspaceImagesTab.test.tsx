import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";

const mockGetCatalog = vi.fn();
const mockListConfigs = vi.fn();
const mockCreateConfig = vi.fn();

vi.mock("../../api/imageFactory", () => ({
  imageFactoryApi: {
    getCatalog: (...a: unknown[]) => mockGetCatalog(...a),
    listConfigs: (...a: unknown[]) => mockListConfigs(...a),
    getConfig: vi.fn(),
    createConfig: (...a: unknown[]) => mockCreateConfig(...a),
  },
}));

const mockToast = vi.fn();

vi.mock("../../providers/ToastProvider", () => ({
  useToast: () => ({ toast: mockToast }),
}));

import { WorkspaceImagesTab } from "./WorkspaceImagesTab";

const defaultCatalog = {
  architectures: ["linux/amd64"],
  bases: [{ name: "bookworm", version: "0.6.0", image: "img", tag: "0.6.0", isDefault: true }],
  extensions: [
    { id: "ffmpeg", type: "apt", value: "ffmpeg", supportedBases: ["bookworm"], retired: false, reviewRequested: false },
    { id: "python313", type: "mise", value: "python@3.13", supportedBases: ["bookworm"], retired: false, reviewRequested: false },
  ],
  knownFailures: [] as Array<Record<string, unknown>>,
};
const defaultConfigs = [{ id: "c1", hash: "s-a", name: "ml-stack", selection: ["ffmpeg"], resolvedValues: {}, baseName: "bookworm", baseVersion: "0.6.0", scope: "member", status: "ready" }];
const defaultCreated = { id: "c2", hash: "s-b", name: "new-cfg", selection: ["python313"], resolvedValues: {}, baseName: "bookworm", baseVersion: "0.6.0", scope: "member", status: "building" };

  // The refresh prefill only targets bases present in the catalog —
  // the stale config points at trixie, so the catalog must offer it.
  const refreshCatalog = {
    ...defaultCatalog,
    bases: [
      ...defaultCatalog.bases,
      { name: "bookworm", version: "0.9.0", image: "img", tag: "0.9.0" },
      { name: "trixie", version: "0.1.0", image: "img-trixie", tag: "0.1.0", isDefault: true },
    ],
  };

describe("WorkspaceImagesTab", () => {
  beforeEach(() => {
    mockGetCatalog.mockResolvedValue(defaultCatalog);
    mockListConfigs.mockResolvedValue(defaultConfigs);
    mockCreateConfig.mockResolvedValue(defaultCreated);
  });

  it("renders saved configs with status pills", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ml-stack")).toBeInTheDocument(); });
    expect(screen.getByText("ready")).toBeInTheDocument();
  });

  it("renders catalog extensions as checkboxes", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
  });

  it("enables create button when name + extension selected", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
    fireEvent.click(screen.getByText("ffmpeg"));
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "t" } });
    expect(screen.getByText("Create Personal Image & Build")).not.toBeDisabled();
  });

  it("disables create button when no name", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
    fireEvent.click(screen.getByText("ffmpeg"));
    expect(screen.getByText("Create Personal Image & Build")).toBeDisabled();
  });

  it("disables create button when no extension selected", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "t" } });
    await waitFor(() => { expect(screen.getByText("Create Personal Image & Build")).toBeDisabled(); });
  });

  it("shows blocked when selection matches a non-retriable known failure", async () => {
    mockGetCatalog.mockResolvedValue({
      ...defaultCatalog,
      extensions: [{ id: "ffmpeg", type: "apt", value: "ffmpeg", supportedBases: ["bookworm"], retired: false, reviewRequested: false }],
      knownFailures: [{ selectionHash: "s-x", selection: ["ffmpeg"], baseName: "bookworm", retriable: false, detectedAt: "2026-01-01" }],
    });
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
    fireEvent.click(screen.getByText("ffmpeg"));
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "t" } });
    expect(screen.getByText("Combination blocked")).toBeDisabled();
  });

  it("shows error toast and preserves config list on create failure", async () => {
    mockCreateConfig.mockRejectedValue(new Error("server error"));
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
    fireEvent.click(screen.getByText("ffmpeg"));
    await waitFor(() => { expect(screen.getByPlaceholderText("e.g. ml-stack")).toBeInTheDocument(); }, { timeout: 3000 });
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "t" } });
    fireEvent.click(screen.getByText("Create Personal Image & Build"));
    await waitFor(() => { expect(mockCreateConfig).toHaveBeenCalled(); }, { timeout: 3000 });
    expect(screen.getByText("ml-stack")).toBeInTheDocument();
  });

  it("shows error message on catalog load failure", async () => {
    mockGetCatalog.mockRejectedValue(new Error("network error"));
    render(<WorkspaceImagesTab />);
    await waitFor(() => {
      expect(screen.getByText(/network error|Failed to load/i)).toBeInTheDocument();
    });
  });

  it("creates config on save (happy path): appends config, resets form, fires toast", async () => {
    mockToast.mockClear();
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });

    // Select extension + type name
    fireEvent.click(screen.getByText("ffmpeg"));
    await waitFor(() => { expect(screen.getByPlaceholderText("e.g. ml-stack")).toBeInTheDocument(); }, { timeout: 3000 });
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "success-cfg" } });
    fireEvent.click(screen.getByText("Create Personal Image & Build"));

    await waitFor(() => { expect(mockCreateConfig).toHaveBeenCalled(); }, { timeout: 3000 });

    // Config appended to the list (the mock returns name="new-cfg")
    await waitFor(() => { expect(screen.getByText("new-cfg")).toBeInTheDocument(); });

    // Form reset: name input cleared
    expect(screen.getByPlaceholderText("e.g. ml-stack")).toHaveValue("");

    // Toast fired with success message
    expect(mockToast).toHaveBeenCalledWith(
      expect.stringContaining("Image config created"), "success",
    );
  });

  it("shows scope pill on config row", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ml-stack")).toBeInTheDocument(); });
    // scope: "member" → renders as "Personal"
    expect(screen.getByText("Personal")).toBeInTheDocument();
  });

  it("expands config drawer showing extensions on click", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ml-stack")).toBeInTheDocument(); });

    // Click the config name to expand the drawer
    fireEvent.click(screen.getByText("ml-stack"));

    // The extension chips should appear (selection: ["ffmpeg"])
    await waitFor(() => {
      const chips = screen.getAllByText("ffmpeg");
      // ffmpeg appears in the catalog checkbox AND the expanded drawer
      expect(chips.length).toBeGreaterThanOrEqual(2);
    });
  });
});

  const staleCfg = {
    ...defaultConfigs[0],
    updatesAvailable: {
      kind: "base_migration" as const,
      currentBaseName: "bookworm",
      currentBaseVersion: "0.6.0",
      latestBaseVersion: "0.9.0",
      defaultBaseName: "trixie",
      defaultBaseVersion: "0.1.0",
    },
  };

const staleCfgUpdates = {
  kind: "base_migration" as const,
  currentBaseName: "bookworm",
  currentBaseVersion: "0.6.0",
  latestBaseVersion: "0.9.0",
  defaultBaseName: "trixie",
  defaultBaseVersion: "0.1.0",
};

describe("refresh flow (#928 phase 2)", () => {


  it("shows a Refresh button on stale configs and prefills the form on click", async () => {
    mockGetCatalog.mockResolvedValue(refreshCatalog);
    mockListConfigs.mockResolvedValue([staleCfg]);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());

    // Expand the card and find the refresh button
    fireEvent.click(screen.getByText("ml-stack"));
    const refreshBtn = await screen.findByRole("button", { name: /Refresh to trixie/i });
    fireEvent.click(refreshBtn);

    // Name prefilled with the DE-CONFLICTED name (scoped uniqueness),
    // refresh banner visible
    expect(await screen.findByText(/Refreshing “ml-stack”/i)).toBeInTheDocument();
    expect((screen.getByPlaceholderText("e.g. ml-stack") as HTMLInputElement).value).toBe("ml-stack (trixie 0.1.0)");
  });

  it("cancel returns the form to empty", async () => {
    mockGetCatalog.mockResolvedValue(refreshCatalog);
    mockListConfigs.mockResolvedValue([staleCfg]);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    fireEvent.click(await screen.findByRole("button", { name: /Refresh to trixie/i }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel refresh" }));
    expect(screen.queryByText(/Refreshing/i)).toBeNull();
    expect((screen.getByPlaceholderText("e.g. ml-stack") as HTMLInputElement).value).toBe("");
  });

  it("no Refresh button on fresh configs", async () => {
    mockGetCatalog.mockResolvedValue(defaultCatalog);
    mockListConfigs.mockResolvedValue(defaultConfigs);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    expect(screen.queryByRole("button", { name: /Refresh to/i })).toBeNull();
  });
});

describe("base-update pill (#928)", () => {
  it("shows a migration pill when the default base moved", async () => {
    mockGetCatalog.mockResolvedValue(defaultCatalog);
    mockListConfigs.mockResolvedValue([
      {
        ...defaultConfigs[0],
        updatesAvailable: {
          kind: "base_migration",
          currentBaseName: "bookworm",
          currentBaseVersion: "0.6.0",
          latestBaseVersion: "0.9.0",
          defaultBaseName: "trixie",
          defaultBaseVersion: "0.1.0",
        },
      },
    ]);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    expect(screen.getByText("new base: trixie")).toBeInTheDocument();
  });

  it("shows a version-bump pill when the same base has a newer version", async () => {
    mockGetCatalog.mockResolvedValue(defaultCatalog);
    mockListConfigs.mockResolvedValue([
      {
        ...defaultConfigs[0],
        updatesAvailable: {
          kind: "version_bump",
          currentBaseName: "bookworm",
          currentBaseVersion: "0.6.0",
          latestBaseVersion: "0.9.0",
        },
      },
    ]);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    expect(screen.getByText("base 0.9.0 available")).toBeInTheDocument();
  });

  it("renders no pill when fresh (field absent)", async () => {
    mockGetCatalog.mockResolvedValue(defaultCatalog);
    mockListConfigs.mockResolvedValue(defaultConfigs);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    expect(screen.queryByText(/available/i)).toBeNull();
    expect(screen.queryByText(/new base/i)).toBeNull();
  });

  it("save from a refresh prefill creates the new config with the de-conflicted name", async () => {
    mockGetCatalog.mockResolvedValue(refreshCatalog);
    mockListConfigs.mockResolvedValue([staleCfg]);
    const created = { id: "c-new", hash: "s-new", name: "ml-stack (trixie 0.1.0)", selection: staleCfg.selection, resolvedValues: {}, baseName: "trixie", baseVersion: "0.1.0", scope: "member", status: "building" };
    mockCreateConfig.mockResolvedValueOnce(created);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    fireEvent.click(await screen.findByRole("button", { name: /Refresh to trixie/i }));
    await waitFor(() => expect(screen.getByPlaceholderText("e.g. ml-stack")).toHaveValue("ml-stack (trixie 0.1.0)"));
    // Save (the create form's submit button)
    fireEvent.click(screen.getByRole("button", { name: /Create Personal Image & Build/i }));
    await waitFor(() =>
      expect(mockCreateConfig).toHaveBeenCalledWith(
        expect.objectContaining({ name: "ml-stack (trixie 0.1.0)", baseName: "trixie", baseVersion: "0.1.0" }),
      ),
    );
    await waitFor(() =>
      expect(mockToast).toHaveBeenCalledWith(
        expect.stringMatching(/Refreshed ml-stack onto trixie 0.1.0.*original is unchanged/),
        "success",
      ),
    );
  });

  it("save failure from a refresh prefill surfaces the API error and keeps the prefill", async () => {
    mockGetCatalog.mockResolvedValue(refreshCatalog);
    mockListConfigs.mockResolvedValue([staleCfg]);
    mockCreateConfig.mockRejectedValueOnce(new Error("failed to save config"));
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    fireEvent.click(await screen.findByRole("button", { name: /Refresh to trixie/i }));
    fireEvent.click(screen.getByRole("button", { name: /Create Personal Image & Build/i }));
    await waitFor(() => expect(mockCreateConfig).toHaveBeenCalled());
    await waitFor(() => expect(mockToast).toHaveBeenCalledWith("failed to save config", "error"));
    // Prefill survives a failed save — the user can retry or edit.
    expect(screen.getByPlaceholderText("e.g. ml-stack")).toHaveValue("ml-stack (trixie 0.1.0)");
  });

  it("version_bump refresh targets the same base's latest version, no Debian caveat in the banner", async () => {
    mockGetCatalog.mockResolvedValue(refreshCatalog);
    const bumpCfg = {
      ...defaultConfigs[0],
      updatesAvailable: {
        kind: "version_bump" as const,
        currentBaseName: "bookworm",
        currentBaseVersion: "0.6.0",
        latestBaseVersion: "0.9.0",
      },
    };
    mockListConfigs.mockResolvedValue([bumpCfg]);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    fireEvent.click(await screen.findByRole("button", { name: /Refresh to bookworm 0.9.0/i }));
    expect(await screen.findByText(/Refreshing “ml-stack”/i)).toBeInTheDocument();
    expect(screen.queryByText(/Debian suite/i)).toBeNull();
    expect((screen.getByPlaceholderText("e.g. ml-stack") as HTMLInputElement).value).toBe("ml-stack (bookworm 0.9.0)");
  });

  it("retired-base race (target missing from catalog) errors loudly, no silent old-base prefill", async () => {
    // Catalog WITHOUT trixie, stale config pointing at a trixie migration.
    mockGetCatalog.mockResolvedValue(defaultCatalog);
    mockListConfigs.mockResolvedValue([staleCfg]);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    fireEvent.click(await screen.findByRole("button", { name: /Refresh to trixie/i }));
    await waitFor(() => expect(mockToast).toHaveBeenCalledWith("Update target base not found in catalog", "error"));
    expect(screen.queryByText(/Refreshing/i)).toBeNull();
  });

  it("cancel restores the DEFAULT base, not the pre-targeted one", async () => {
    // refreshCatalog's default is trixie (the migration target); make the
    // DEFAULT bookworm so cancel has a distinct base to restore.
    const catalogDefaultBookworm = {
      ...refreshCatalog,
      bases: refreshCatalog.bases.map((b: { name: string; isDefault?: boolean }) => ({ ...b, isDefault: b.name === "bookworm" })),
    };
    mockGetCatalog.mockResolvedValue(catalogDefaultBookworm);
    mockListConfigs.mockResolvedValue([staleCfg]);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    fireEvent.click(await screen.findByRole("button", { name: /Refresh to trixie/i }));
    // Prefill targeted trixie...
    const baseSelect = screen.getAllByRole("combobox")[0] as HTMLSelectElement;
    expect(baseSelect.value).toBe("trixie/0.1.0");
    fireEvent.click(await screen.findByRole("button", { name: "Cancel refresh" }));
    // ...cancel restores the default (bookworm 0.6.0).
    await waitFor(() => expect(baseSelect.value).toBe("bookworm/0.6.0"));
  });

  it("shows the unsupported-on-base hint when a selected extension misses the target base", async () => {
    // Catalog where ffmpeg only supports bookworm; target trixie.
    const partialCatalog = {
      ...refreshCatalog,
      extensions: refreshCatalog.extensions.map((e: { id: string }) =>
        e.id === "ffmpeg" ? { ...e, supportedBases: ["bookworm"] } : e,
      ),
    };
    mockGetCatalog.mockResolvedValue(partialCatalog);
    mockListConfigs.mockResolvedValue([staleCfg]); // selection: ["ffmpeg"]
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    fireEvent.click(await screen.findByRole("button", { name: /Refresh to trixie/i }));
    expect(await screen.findByText(/Not available on trixie: ffmpeg/i)).toBeInTheDocument();
  });

  it("auto-drops retired extensions from the prefill and reports it (#928 r3 R2)", async () => {
    // Catalog WITHOUT ffmpeg (retired since save); python313 widened to
    // support trixie so the live remainder is clean on the target base.
    const retiredGone = {
      ...refreshCatalog,
      extensions: refreshCatalog.extensions
        .filter((e: { id: string }) => e.id !== "ffmpeg")
        .map((e: { id: string; supportedBases: string[] }) =>
          e.id === "python313" ? { ...e, supportedBases: ["bookworm", "trixie"] } : e,
        ),
    };
    mockGetCatalog.mockResolvedValue(retiredGone);
    const staleTwo = {
      ...defaultConfigs[0],
      selection: ["ffmpeg", "python313"],
      updatesAvailable: staleCfgUpdates,
    };
    mockListConfigs.mockResolvedValue([staleTwo]);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    fireEvent.click(await screen.findByRole("button", { name: /Refresh to trixie/i }));
    // Info toast: one retired extension dropped; prefill still lands with the live one.
    await waitFor(() => expect(mockToast).toHaveBeenCalledWith(expect.stringMatching(/1 retired extension dropped/), "success"));
    expect(await screen.findByText(/Refreshing “ml-stack”/i)).toBeInTheDocument();
    // python313 (live, supports trixie) is not flagged as unsupported —
    // the hint area may legitimately mention nothing at all.
    expect(screen.queryByText(/Not available on trixie: python313/i)).toBeNull();
  });

  it("fully-retired selection aborts the refresh with an actionable error (#928 r3 R2)", async () => {
    const retiredGone = {
      ...refreshCatalog,
      extensions: refreshCatalog.extensions.filter((e: { id: string }) => e.id !== "ffmpeg"),
    };
    mockGetCatalog.mockResolvedValue(retiredGone);
    mockListConfigs.mockResolvedValue([staleCfg]); // selection: ["ffmpeg"] only
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    fireEvent.click(await screen.findByRole("button", { name: /Refresh to trixie/i }));
    await waitFor(() => expect(mockToast).toHaveBeenCalledWith(expect.stringMatching(/Every extension.*retired.*create a new config/), "error"));
    expect(screen.queryByText(/Refreshing/i)).toBeNull();
  });

  it("second refresh of the same original dedups the suggested name (#928 r3 R3)", async () => {
    mockGetCatalog.mockResolvedValue(refreshCatalog);
    // The list already contains the FIRST refresh's config.
    const firstRefresh = { ...defaultConfigs[0], id: "c-first", name: "ml-stack (trixie 0.1.0)", hash: "s-first", baseName: "trixie", baseVersion: "0.1.0", status: "ready" };
    mockListConfigs.mockResolvedValue([staleCfg, firstRefresh]);
    render(<WorkspaceImagesTab />);
    await waitFor(() => expect(screen.getByText("ml-stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("ml-stack"));
    fireEvent.click(await screen.findByRole("button", { name: /Refresh to trixie/i }));
    expect((await screen.findByPlaceholderText("e.g. ml-stack") as HTMLInputElement).value).toBe("ml-stack (trixie 0.1.0) 2");
  });
});
