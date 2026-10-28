// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render as rtlRender } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  BoardDefaultsDialog,
  ProjectConfigDialog,
} from "./BoardConfigPanels";
import { boardApi, type BoardConfig, type BoardProject, type ProjectBoardConfig } from "../../api/board";

// BoardConfigPanels — the relocated board config surfaces (owner
// ruling 2026-10-28: config ON the board, alongside the projects).
// These tests pin the persona editor at BOTH tiers: the set-ness
// round-trip (never materialize {}, never silently wipe a stored
// map), the {} opt-out, the legacy-reviewer-suppression warning with
// its one-click default-reviewer import, the effective merged view
// with inherited/override badges, and the worker-slots PATCH seam.

vi.mock("../../api/board", () => ({
  boardApi: {
    listProjects: vi.fn(),
    getUserBoardConfig: vi.fn(),
    setUserBoardConfig: vi.fn(),
    getProjectBoardConfig: vi.fn(),
    setProjectBoardConfig: vi.fn(),
    updateProject: vi.fn(),
  },
  BOARD_REVIEW_COLUMNS: ["off", "manual", "auto"],
}));

const USER: BoardConfig = { reviewColumn: "manual", autoAssignReviewer: false, defaultWorkerSlots: 2 };
const PC_A: ProjectBoardConfig = {
  project_id: "prj-aaaa1111",
  overrides: { reviewColumn: "auto" },
  effective: { reviewColumn: "auto", autoAssignReviewer: false, defaultWorkerSlots: 2 },
};
const PC_B: ProjectBoardConfig = {
  project_id: "prj-bbbb2222",
  overrides: {},
  effective: USER,
};
// Round-1 review finding: a project carrying a STORED stagePersonas
// override — a save of the OTHER keys must carry it back verbatim
// (the server PUT is REPLACE: absent keys are deletes).
const PC_PERSONAS: ProjectBoardConfig = {
  project_id: "prj-cccc3333",
  overrides: { stagePersonas: { in_review: { name: "auditor", instructions: "Verify." } } },
  effective: { ...USER, stagePersonas: { in_review: { name: "auditor", instructions: "Verify." } } },
};
const PROJECT: BoardProject = {
  id: "proj-uuid-b",
  short_id: "prj-bbbb2222",
  user_id: "u1",
  name: "Borealis",
  target_workspace_id: "ws-1",
  worker_slots: 1,
  tags: [],
  status: "active",
  created_at: "",
  updated_at: "",
};

function renderDefaults() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return rtlRender(
    <QueryClientProvider client={qc}>
      <BoardDefaultsDialog open onOpenChange={() => {}} />
    </QueryClientProvider>,
  );
}

function renderProject(project: BoardProject = PROJECT) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return rtlRender(
    <QueryClientProvider client={qc}>
      <ProjectConfigDialog project={project} open onOpenChange={() => {}} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue(USER);
  vi.mocked(boardApi.getProjectBoardConfig).mockImplementation(async (id: string) =>
    id === "prj-aaaa1111" ? PC_A : id === "prj-cccc3333" ? PC_PERSONAS : PC_B,
  );
  vi.mocked(boardApi.setUserBoardConfig).mockResolvedValue(USER);
  vi.mocked(boardApi.setProjectBoardConfig).mockImplementation(async (id: string) =>
    id === "prj-aaaa1111" ? PC_A : id === "prj-cccc3333" ? PC_PERSONAS : PC_B,
  );
  vi.mocked(boardApi.updateProject).mockResolvedValue(PROJECT);
});

describe("BoardDefaultsDialog — the user tier, ON the board", () => {
  it("renders the global defaults from GET /me/board/config", async () => {
    renderDefaults();
    expect(await screen.findByTestId("cfg-reviewColumn")).toBeInTheDocument();
    expect((screen.getByTestId("cfg-reviewColumn") as HTMLElement).textContent).toContain("manual");
  });

  it("Save writes the full config through the API (typed body)", async () => {
    const user = userEvent.setup();
    renderDefaults();
    const slots = await screen.findByTestId("cfg-defaultWorkerSlots");
    await user.clear(slots.querySelector("input")!);
    await user.type(slots.querySelector("input")!, "3");
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    // Round-2 pin: nothing stored, stagePersonasSet falsy → the save
    // MUST NOT materialize a stagePersonas key ({} would become an
    // explicit opt-out row and silently suppress the flipped default).
    expect(body).toEqual({ reviewColumn: "manual", autoAssignReviewer: false, defaultWorkerSlots: 3 });
  });

  it("round-2 pin: an explicitly-SET stagePersonas map is carried back verbatim (REPLACE never wipes it)", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_review: { name: "auditor", instructions: "Verify." } },
      stagePersonasSet: true,
    });
    renderDefaults();
    const slots = await screen.findByTestId("cfg-defaultWorkerSlots");
    await user.type(slots.querySelector("input")!, "4");
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({
      in_review: { name: "auditor", instructions: "Verify." },
    });
  });

  it("client validation mirrors the server: slots < 1 blocks the send (no PUT fires)", async () => {
    const user = userEvent.setup();
    renderDefaults();
    const slots = await screen.findByTestId("cfg-defaultWorkerSlots");
    await user.clear(slots.querySelector("input")!);
    await user.type(slots.querySelector("input")!, "0");
    const save = screen.getByTestId("cfg-save-global") as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(boardApi.setUserBoardConfig).not.toHaveBeenCalled();
  });

  it("nothing stored: the status rows show the legacy composition, not a phantom map", async () => {
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      reviewColumn: "auto",
      autoAssignReviewer: true,
    });
    renderDefaults();
    expect(await screen.findByTestId("cfg-personas-user-in_review-status")).toHaveTextContent(
      "default reviewer persona (auto-review composition)",
    );
    expect(screen.getByTestId("cfg-personas-user-in_progress-status")).toHaveTextContent("no persona");
    expect(screen.queryByTestId("cfg-personas-user-in_review-row")).not.toBeInTheDocument();
    expect(screen.queryByTestId("cfg-personas-user-empty-note")).not.toBeInTheDocument();
  });

  it("a STORED explicit map renders editable name+instructions rows (both stages listed)", async () => {
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: {
        in_review: { name: "auditor", instructions: "Verify." },
        in_progress: { name: "implementer" },
      },
      stagePersonasSet: true,
    });
    renderDefaults();
    const name = await screen.findByTestId("cfg-personas-user-in_review-name");
    expect((name as HTMLInputElement).value).toBe("auditor");
    expect((screen.getByTestId("cfg-personas-user-in_review-instructions") as HTMLTextAreaElement).value).toBe(
      "Verify.",
    );
    expect((screen.getByTestId("cfg-personas-user-in_progress-name") as HTMLInputElement).value).toBe("implementer");
  });

  it("unlisted stages render explicitly as no persona while the map is explicit", async () => {
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_progress: { name: "implementer" } },
      stagePersonasSet: true,
    });
    renderDefaults();
    expect(await screen.findByTestId("cfg-personas-user-in_progress-row")).toBeInTheDocument();
    const none = screen.getByTestId("cfg-personas-user-in_review-none");
    expect(none).toHaveTextContent(/no persona/);
    expect(none).toHaveTextContent(/owns every stage it does not list/);
  });

  it("add-stage → edit name+instructions → save carries the explicit map", async () => {
    const user = userEvent.setup();
    renderDefaults();
    await screen.findByTestId("cfg-personas-user");
    await user.click(screen.getByTestId("cfg-personas-user-manage"));
    await user.click(screen.getByTestId("cfg-personas-user-in_progress-add"));
    await user.type(screen.getByTestId("cfg-personas-user-in_progress-name"), "builder");
    await user.type(screen.getByTestId("cfg-personas-user-in_progress-instructions"), "Build the thing.");
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    expect((body as { stagePersonas?: Record<string, unknown> }).stagePersonas).toEqual({
      in_progress: { name: "builder", instructions: "Build the thing." },
    });
  });

  it("{} opt-out — explicit empty map saves stagePersonas: {} and renders the plain label", async () => {
    const user = userEvent.setup();
    renderDefaults();
    await screen.findByTestId("cfg-personas-user");
    await user.click(screen.getByTestId("cfg-personas-user-manage"));
    expect(screen.getByTestId("cfg-personas-user-empty-note")).toHaveTextContent(/no personas/);
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({});
  });

  it("suppression warning fires when the composition is live and no in_review entry exists; the one-click import adds DefaultReviewerPersona", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      reviewColumn: "auto",
      autoAssignReviewer: true,
    });
    renderDefaults();
    await screen.findByTestId("cfg-personas-user");
    await user.click(screen.getByTestId("cfg-personas-user-manage"));
    const warning = screen.getByTestId("cfg-personas-user-warning");
    expect(warning).toHaveTextContent(/suppresses the built-in auto-reviewer persona/);
    await user.click(screen.getByTestId("cfg-personas-user-add-reviewer"));
    expect(screen.queryByTestId("cfg-personas-user-warning")).not.toBeInTheDocument();
    expect((screen.getByTestId("cfg-personas-user-in_review-name") as HTMLInputElement).value).toBe("reviewer");
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    const sent = (body as { stagePersonas?: Record<string, { name: string; instructions?: string }> })
      .stagePersonas!;
    expect(sent.in_review?.name).toBe("reviewer");
    expect(sent.in_review?.instructions).toContain("board_record_verdict");
  });

  it("no suppression warning when the composition is off (autoAssignReviewer=false)", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      reviewColumn: "auto",
      autoAssignReviewer: false,
    });
    renderDefaults();
    await screen.findByTestId("cfg-personas-user");
    await user.click(screen.getByTestId("cfg-personas-user-manage"));
    expect(screen.queryByTestId("cfg-personas-user-warning")).not.toBeInTheDocument();
  });

  it("removing the last stage keeps the map explicit ({}), and Revert un-sets it", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_review: { name: "auditor", instructions: "Verify." } },
      stagePersonasSet: true,
    });
    renderDefaults();
    await screen.findByTestId("cfg-personas-user-in_review-row");
    await user.click(screen.getByTestId("cfg-personas-user-in_review-remove"));
    expect(screen.getByTestId("cfg-personas-user-empty-note")).toBeInTheDocument();
    // Revert: the save carries NO stagePersonas key (the stored row is
    // deleted — REPLACE semantics; unset = the code-default posture).
    await user.click(screen.getByTestId("cfg-personas-user-revert"));
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    expect("stagePersonas" in body).toBe(false);
  });

  it("a failed global PUT renders the error and keeps the editor (no swallow)", async () => {
    vi.mocked(boardApi.setUserBoardConfig).mockRejectedValueOnce(new Error("400: unknown key"));
    const user = userEvent.setup();
    renderDefaults();
    const slots = await screen.findByTestId("cfg-defaultWorkerSlots");
    await user.clear(slots.querySelector("input")!);
    await user.type(slots.querySelector("input")!, "4");
    await user.click(screen.getByTestId("cfg-save-global"));
    expect(await screen.findByTestId("cfg-error-global")).toBeInTheDocument();
  });
});

describe("ProjectConfigDialog — the project tier, alongside the projects", () => {
  it("renders the effective config with provenance (override vs inherited)", async () => {
    const project = { ...PROJECT, short_id: "prj-aaaa1111" };
    renderProject(project);
    await screen.findByTestId("cfg-personas-prj-aaaa1111");
    expect(screen.getByTestId("cfg-provenance-prj-aaaa1111-reviewColumn").textContent).toContain("override");
    expect(screen.getByTestId("cfg-provenance-prj-aaaa1111-autoAssignReviewer").textContent).toContain("inherited");
    expect(screen.getByTestId("cfg-effective-prj-aaaa1111").textContent).toContain("effective: auto");
  });

  it("an override PUT carries ONLY the overridden key — absent = inherit", async () => {
    const user = userEvent.setup();
    renderProject();
    const sel = await screen.findByTestId("cfg-ov-prj-bbbb2222-autoAssignReviewer");
    await user.click(sel);
    await user.click(await screen.findByText("On"));
    await user.click(screen.getByTestId("cfg-save-prj-bbbb2222"));
    await waitFor(() => expect(boardApi.setProjectBoardConfig).toHaveBeenCalled());
    const [id, body] = vi.mocked(boardApi.setProjectBoardConfig).mock.calls[0]!;
    expect(id).toBe("prj-bbbb2222");
    expect(Object.keys(body)).toEqual(["autoAssignReviewer"]);
    expect((body as { autoAssignReviewer?: boolean }).autoAssignReviewer).toBe(true);
  });

  it("round-1 pin: saving the project's OTHER keys carries the stored stagePersonas back verbatim (REPLACE never wipes personas)", async () => {
    const user = userEvent.setup();
    const project = { ...PROJECT, short_id: "prj-cccc3333" };
    renderProject(project);
    const sel = await screen.findByTestId("cfg-ov-prj-cccc3333-reviewColumn");
    await user.click(sel);
    await user.click(await screen.findByRole("option", { name: "manual" }));
    await user.click(screen.getByTestId("cfg-save-prj-cccc3333"));
    await waitFor(() => expect(boardApi.setProjectBoardConfig).toHaveBeenCalled());
    const [id, body] = vi.mocked(boardApi.setProjectBoardConfig).mock.calls[0]!;
    expect(id).toBe("prj-cccc3333");
    expect((body as { reviewColumn?: string }).reviewColumn).toBe("manual");
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({
      in_review: { name: "auditor", instructions: "Verify." },
    });
    expect(Object.keys(body).sort()).toEqual(["reviewColumn", "stagePersonas"]);
  });

  it("Inherit-everything sends {} (full inheritance)", async () => {
    const user = userEvent.setup();
    const project = { ...PROJECT, short_id: "prj-aaaa1111" };
    renderProject(project);
    const sel = await screen.findByTestId("cfg-ov-prj-aaaa1111-reviewColumn");
    await user.click(sel);
    const inheritOption = (await screen.findAllByRole("option", { name: "Inherit" }))[0]!;
    await user.click(inheritOption);
    await user.click(screen.getByTestId("cfg-save-prj-aaaa1111"));
    await waitFor(() => expect(boardApi.setProjectBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setProjectBoardConfig).mock.calls[0]![1];
    expect(body).toEqual({});
  });

  it("worker slots ride the PROJECT PATCH (not the config override)", async () => {
    const user = userEvent.setup();
    renderProject();
    const slots = await screen.findByTestId("cfg-slots-prj-bbbb2222");
    await user.clear(slots.querySelector("input")!);
    await user.type(slots.querySelector("input")!, "3");
    await user.click(screen.getByTestId("cfg-save-prj-bbbb2222"));
    await waitFor(() => expect(boardApi.updateProject).toHaveBeenCalled());
    expect(boardApi.updateProject).toHaveBeenCalledWith("prj-bbbb2222", { workerSlots: 3 });
    // Slots are NOT a config key: the override PUT fires too (nothing
    // config-dirty → it must NOT fire) — slots-only save patches only.
    expect(boardApi.setProjectBoardConfig).not.toHaveBeenCalled();
  });

  it("the {}-opt-out affordance sends stagePersonas: {} (no personas for this project)", async () => {
    const user = userEvent.setup();
    renderProject();
    await screen.findByTestId("cfg-personas-prj-bbbb2222");
    await user.click(screen.getByTestId("cfg-personas-prj-bbbb2222-optout"));
    await user.click(screen.getByTestId("cfg-save-prj-bbbb2222"));
    await waitFor(() => expect(boardApi.setProjectBoardConfig).toHaveBeenCalled());
    const [id, body] = vi.mocked(boardApi.setProjectBoardConfig).mock.calls[0]!;
    expect(id).toBe("prj-bbbb2222");
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({});
  });

  it("effective merged view — an inherited user-tier persona renders with the inherited badge", async () => {
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_review: { name: "auditor", instructions: "Verify." } },
      stagePersonasSet: true,
    });
    renderProject();
    const status = await screen.findByTestId("cfg-personas-prj-bbbb2222-in_review-status");
    expect(status).toHaveTextContent(/inherited: “auditor”/);
    // in_progress is not listed in the inherited explicit map → the
    // explicit-key-owns-all-stages semantics applies one tier up.
    expect(screen.getByTestId("cfg-personas-prj-bbbb2222-in_progress-status")).toHaveTextContent(/no persona/);
  });

  it("inherited-kind suppression warning when overriding a user-tier in_review persona", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_review: { name: "auditor", instructions: "Verify." } },
      stagePersonasSet: true,
    });
    renderProject();
    await screen.findByTestId("cfg-personas-prj-bbbb2222");
    await user.click(screen.getByTestId("cfg-personas-prj-bbbb2222-manage"));
    const warning = screen.getByTestId("cfg-personas-prj-bbbb2222-warning");
    expect(warning).toHaveTextContent(/suppresses the inherited in_review persona/);
    expect(warning).toHaveTextContent(/auditor/);
    // The one-click import restores the INHERITED persona, not the
    // default reviewer.
    await user.click(screen.getByTestId("cfg-personas-prj-bbbb2222-add-reviewer"));
    expect((screen.getByTestId("cfg-personas-prj-bbbb2222-in_review-name") as HTMLInputElement).value).toBe(
      "auditor",
    );
  });

  it("a stored override map renders editable and saves edited personas", async () => {
    const user = userEvent.setup();
    const project = { ...PROJECT, short_id: "prj-cccc3333" };
    renderProject(project);
    const name = await screen.findByTestId("cfg-personas-prj-cccc3333-in_review-name");
    expect((name as HTMLInputElement).value).toBe("auditor");
    await user.clear(name);
    await user.type(name, "auditor-2");
    await user.click(screen.getByTestId("cfg-save-prj-cccc3333"));
    await waitFor(() => expect(boardApi.setProjectBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setProjectBoardConfig).mock.calls[0]![1];
    expect((body as { stagePersonas?: Record<string, { name: string }> }).stagePersonas?.in_review?.name).toBe(
      "auditor-2",
    );
  });

  it("Inherit drops the explicit override — the save sends no stagePersonas key", async () => {
    const user = userEvent.setup();
    const project = { ...PROJECT, short_id: "prj-cccc3333" };
    renderProject(project);
    await screen.findByTestId("cfg-personas-prj-cccc3333-in_review-row");
    await user.click(screen.getByTestId("cfg-personas-prj-cccc3333-revert"));
    await user.click(screen.getByTestId("cfg-save-prj-cccc3333"));
    await waitFor(() => expect(boardApi.setProjectBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setProjectBoardConfig).mock.calls[0]![1];
    expect("stagePersonas" in body).toBe(false);
  });

  it("regression pin: an untouched open→save never fires a write (no accidental REPLACE)", async () => {
    renderProject();
    await screen.findByTestId("cfg-personas-prj-bbbb2222");
    const save = screen.getByTestId("cfg-save-prj-bbbb2222") as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(boardApi.setProjectBoardConfig).not.toHaveBeenCalled();
    expect(boardApi.updateProject).not.toHaveBeenCalled();
  });
});
