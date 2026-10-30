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
import {
  boardApi,
  type BoardConfig,
  type BoardPersonaConfig,
  type BoardProject,
  type ProjectBoardConfig,
} from "../../api/board";
import { agentRolesApi } from "../../api/agentRoles";

// BoardConfigPanels — the relocated board config surfaces (owner
// ruling 2026-10-28: config ON the board, alongside the projects).
// These tests pin the ROLE-ONLY persona editor (owner ruling
// 2026-10-29, round 2: "It's not role-first, it's role-ONLY") at BOTH
// tiers: the set-ness round-trip (never materialize {}, never
// silently wipe a stored map), the {} opt-out, the drop-on-save rule
// for role-less entries, the legacy-reviewer-suppression warning with
// its one-click reviewer-role import, the effective merged view with
// inherited/override badges, and the worker-slots PATCH seam. The
// editor's ONLY per-stage authoring surface is the role select +
// Remove — no name input, no instructions textarea, no "no role"
// option anywhere. The suppression warning, its import's dismissal,
// and the empty-map note all read the PERSISTABLE map (the
// role-filtered map a save actually sends — review F1–F3 on
// PR #1988): a role-less entry can never mask the suppression.

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

// The persona editor's role vocabulary (owner ruling 2026-10-29:
// personas REFERENCE the platform's agent roles — the picker's
// source).
vi.mock("../../api/agentRoles", () => ({
  agentRolesApi: {
    listForMe: vi.fn(),
  },
}));

/** The persona map the save-body assertions read (bare role refs). */
type PersonaMapShape = Record<string, BoardPersonaConfig>;

const REVIEWER_ROLE = {
  id: "11111111-1111-1111-1111-111111111111",
  scope: "platform",
  name: "Code Reviewer",
  slug: "reviewer",
  description: "",
  isDefault: false,
  config: {},
  createdAt: "",
  updatedAt: "",
};
const BUILD_ROLE = {
  id: "22222222-2222-2222-2222-222222222222",
  scope: "org",
  name: "Builder",
  slug: "build",
  description: "",
  isDefault: false,
  config: {},
  createdAt: "",
  updatedAt: "",
};

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
// A project carrying a STORED stagePersonas override (bare role
// references — the canonical wire form) — a save of the OTHER keys
// must carry it back verbatim (the server PUT is REPLACE: absent keys
// are deletes).
const PC_PERSONAS: ProjectBoardConfig = {
  project_id: "prj-cccc3333",
  overrides: {
    stagePersonas: { in_review: REVIEWER_ROLE.id },
  },
  effective: {
    ...USER,
    stagePersonas: { in_review: REVIEWER_ROLE.id },
  },
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

/** The persona section must carry NO text inputs of any kind — the
 * role select + Remove is the whole authoring surface (role-ONLY). */
function expectNoTextInputs(section: HTMLElement) {
  expect(section.querySelectorAll("input, textarea")).toHaveLength(0);
}

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
  vi.mocked(agentRolesApi.listForMe).mockResolvedValue([REVIEWER_ROLE, BUILD_ROLE]);
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

  it("round-2 pin: an explicitly-SET stagePersonas map of role references is carried back verbatim (REPLACE never wipes it)", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: {
        in_review: REVIEWER_ROLE.id,
        in_progress: { roleId: BUILD_ROLE.id },
      },
      stagePersonasSet: true,
    });
    renderDefaults();
    const slots = await screen.findByTestId("cfg-defaultWorkerSlots");
    await user.type(slots.querySelector("input")!, "4");
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    // Every stored role reference rides the save — canonicalized to
    // the bare-string form (the {roleId} object decodes to the same
    // reference; the canonical form is what the editor writes).
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({
      in_review: REVIEWER_ROLE.id,
      in_progress: BUILD_ROLE.id,
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

  it("role-only pin: a STORED explicit map renders role selects ONLY — no name input, no instructions textarea, no no-role option", async () => {
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: {
        in_review: REVIEWER_ROLE.id,
        in_progress: BUILD_ROLE.id,
      },
      stagePersonasSet: true,
    });
    renderDefaults();
    await screen.findByTestId("cfg-personas-user-in_review-row");
    expect(screen.getByTestId("cfg-personas-user-in_review-role").textContent).toContain("Code Reviewer");
    expect(screen.getByTestId("cfg-personas-user-in_progress-role").textContent).toContain("Builder");
    // ROLE-ONLY (owner ruling 2026-10-29 round 2): no text authoring
    // surface of any kind in the persona section.
    expectNoTextInputs(screen.getByTestId("cfg-personas-user"));
    expect(screen.queryByTestId("cfg-personas-user-in_review-name")).not.toBeInTheDocument();
    expect(screen.queryByTestId("cfg-personas-user-in_review-instructions")).not.toBeInTheDocument();
    expect(screen.getByTestId("cfg-personas-user-role-only-note")).toHaveTextContent(/single source of truth/);
  });

  it("unlisted stages render explicitly as no persona while the map is explicit", async () => {
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_progress: BUILD_ROLE.id },
      stagePersonasSet: true,
    });
    renderDefaults();
    expect(await screen.findByTestId("cfg-personas-user-in_progress-row")).toBeInTheDocument();
    const none = screen.getByTestId("cfg-personas-user-in_review-none");
    expect(none).toHaveTextContent(/no persona/);
    expect(none).toHaveTextContent(/owns every stage it does not list/);
  });

  it("Add persona opens the role picker directly; picking a role and saving carries the role reference only", async () => {
    const user = userEvent.setup();
    renderDefaults();
    await screen.findByTestId("cfg-personas-user");
    await user.click(screen.getByTestId("cfg-personas-user-manage"));
    await user.click(screen.getByTestId("cfg-personas-user-in_progress-add"));
    // The added row IS a picker (placeholder, no picked value) — no
    // name/instructions inputs ever render.
    const row = screen.getByTestId("cfg-personas-user-in_progress-row");
    expect(row.textContent).toContain("Pick a role…");
    expectNoTextInputs(screen.getByTestId("cfg-personas-user"));
    await user.click(screen.getByTestId("cfg-personas-user-in_progress-role"));
    await user.click(await screen.findByRole("option", { name: "Builder (org)" }));
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    expect((body as { stagePersonas?: Record<string, unknown> }).stagePersonas).toEqual({
      in_progress: BUILD_ROLE.id,
    });
  });

  it("role-only drop rule: an added row left unpicked is NOT persisted (save drops it)", async () => {
    const user = userEvent.setup();
    renderDefaults();
    await screen.findByTestId("cfg-personas-user");
    await user.click(screen.getByTestId("cfg-personas-user-manage"));
    await user.click(screen.getByTestId("cfg-personas-user-in_progress-add"));
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({});
  });

  it("a STALE stored reference (role deleted post-storage) rides as its own option and saves verbatim — the drift stays visible", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_review: "gone-role-id" },
      stagePersonasSet: true,
    });
    renderDefaults();
    const row = await screen.findByTestId("cfg-personas-user-in_review-row");
    // The unresolvable reference renders as its own option — visible
    // and preserved, never silently re-pointed or dropped.
    expect(row.textContent).toContain("gone-role-id");
    expectNoTextInputs(screen.getByTestId("cfg-personas-user"));
    const slots = screen.getByTestId("cfg-defaultWorkerSlots");
    await user.type(slots.querySelector("input")!, "4");
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({ in_review: "gone-role-id" });
  });

  it("F3 pin: an explicit draft whose every entry is UNPICKED renders the empty-map note (the save materializes {})", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      reviewColumn: "auto",
      autoAssignReviewer: false,
    });
    renderDefaults();
    await screen.findByTestId("cfg-personas-user");
    await user.click(screen.getByTestId("cfg-personas-user-manage"));
    // One added-but-unpicked row: the raw draft carries a key, but
    // the note keys on the PERSISTABLE map (role-less entries drop) —
    // under role-only this draft shape is the ONLY role-less form
    // left (a stored one cannot exist on the wire).
    await user.click(screen.getByTestId("cfg-personas-user-in_progress-add"));
    expect(screen.getByTestId("cfg-personas-user-in_progress-row").textContent).toContain("Pick a role…");
    expect(screen.queryByTestId("cfg-personas-user-warning")).not.toBeInTheDocument();
    expect(screen.getByTestId("cfg-personas-user-empty-note")).toHaveTextContent(/no personas/);
    // And the note is honest: the save really materializes the
    // explicit {} opt-out (the unpicked row never rides).
    const slots = screen.getByTestId("cfg-defaultWorkerSlots");
    await user.type(slots.querySelector("input")!, "4");
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({});
  });

  it("an unresolvable stored roleId rides as its own select option and persists verbatim (never stripped, never lost)", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_review: { roleId: "ghost-role-id" } },
      stagePersonasSet: true,
    });
    renderDefaults();
    const row = await screen.findByTestId("cfg-personas-user-in_review-row");
    // Not in the vocabulary → the raw reference stays visible as the
    // select's own option (no phantom, no wipe)…
    expect(row.textContent).toContain("ghost-role-id");
    const slots = screen.getByTestId("cfg-defaultWorkerSlots");
    await user.type(slots.querySelector("input")!, "4");
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    // …and a role-BEARING entry rides verbatim even when the role
    // cannot be resolved client-side (the platform validates) — as
    // the canonical BARE-STRING reference (role-only wire).
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({
      in_review: "ghost-role-id",
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

  it("suppression warning fires when the composition is live and no in_review entry exists; the one-click import adds the reviewer ROLE persona", async () => {
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
    // ROLE-ONLY (owner ruling 2026-10-29 round 2): the import is the
    // reviewer ROLE — the select shows it, NO name input and NO
    // instructions textarea render (the mirrored charter prose rides
    // the STORED shape unseen; the save carries the role id).
    expect(screen.getByTestId("cfg-personas-user-in_review-role").textContent).toContain("Code Reviewer");
    expectNoTextInputs(screen.getByTestId("cfg-personas-user"));
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    // The import saves the reviewer ROLE reference (its canonical id)
    // — the verdict-channel charter is the role's system prompt, never
    // a persona payload.
    const sent = (body as { stagePersonas?: PersonaMapShape }).stagePersonas!;
    expect(sent.in_review).toBe(REVIEWER_ROLE.id);
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

  it("F1 pin: an added-but-UNPICKED in_review row with the composition LIVE fires the warning — the raw draft cannot mask what the save drops to {}", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      reviewColumn: "auto",
      autoAssignReviewer: true,
    });
    renderDefaults();
    // The draft's in_review entry is "" (added, no role picked): it is
    // NOT an in_review persona for warning purposes (the save drops
    // it) — the warning must fire, not be masked by the raw draft's
    // truthy key. Under role-only this draft shape is the ONLY
    // role-less form left (a stored one cannot exist on the wire).
    await screen.findByTestId("cfg-personas-user");
    await user.click(screen.getByTestId("cfg-personas-user-manage"));
    expect(screen.getByTestId("cfg-personas-user-warning")).toBeInTheDocument();
    await user.click(screen.getByTestId("cfg-personas-user-in_review-add"));
    expect(screen.getByTestId("cfg-personas-user-in_review-row").textContent).toContain("Pick a role…");
    expect(screen.getByTestId("cfg-personas-user-warning")).toBeInTheDocument();
    // And the warning is honest: saving (dirtied via slots) really
    // does send the explicit {} opt-out.
    const slots = screen.getByTestId("cfg-defaultWorkerSlots");
    await user.type(slots.querySelector("input")!, "4");
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({});
  });

  it("F2 pin: importing with NO reviewer role in the client vocabulary lands the SEEDED reviewer slug — a role-bearing ref, the platform validates it", async () => {
    const user = userEvent.setup();
    vi.mocked(agentRolesApi.listForMe).mockResolvedValue([BUILD_ROLE]);
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      reviewColumn: "auto",
      autoAssignReviewer: true,
    });
    renderDefaults();
    await screen.findByTestId("cfg-personas-user");
    await user.click(screen.getByTestId("cfg-personas-user-manage"));
    expect(screen.getByTestId("cfg-personas-user-warning")).toBeInTheDocument();
    await user.click(screen.getByTestId("cfg-personas-user-add-reviewer"));
    // The import landed the built-in reviewer role's SLUG (the
    // platform seeds the role at startup, so the slug always
    // addresses it) — a role-BEARING entry: the row shows the raw
    // slug as its own option (the client vocabulary is stale, not
    // the entry), the warning legitimately dismisses, and the save
    // persists the reference (the server validates against the real
    // vocabulary — never a silent suppression).
    expect(screen.getByTestId("cfg-personas-user-in_review-row").textContent).toContain("reviewer");
    expect(screen.queryByTestId("cfg-personas-user-warning")).not.toBeInTheDocument();
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    const sent = (body as { stagePersonas?: PersonaMapShape }).stagePersonas!;
    expect(sent.in_review).toBe("reviewer");
    // The re-pick flow (a vocabulary role replacing the raw reference)
    // is pinned by the project-tier "saves a RE-PICKED role" test.
  });

  it("removing the last stage keeps the map explicit ({}), and Revert un-sets it", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_review: REVIEWER_ROLE.id },
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

  it("round-1 pin: saving the project's OTHER keys carries the stored role-bearing stagePersonas back verbatim (REPLACE never wipes personas)", async () => {
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
      in_review: REVIEWER_ROLE.id,
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

  it("effective merged view — an inherited user-tier role persona renders with the inherited badge", async () => {
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_review: REVIEWER_ROLE.id },
      stagePersonasSet: true,
    });
    renderProject();
    const status = await screen.findByTestId("cfg-personas-prj-bbbb2222-in_review-status");
    expect(status).toHaveTextContent(/inherited: “Code Reviewer”/);
    // in_progress is not listed in the inherited explicit map → the
    // explicit-key-owns-all-stages semantics applies one tier up.
    expect(screen.getByTestId("cfg-personas-prj-bbbb2222-in_progress-status")).toHaveTextContent(/no persona/);
  });

  it("inherited-kind suppression warning when overriding a user-tier in_review role persona; the import restores the ROLE", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_review: REVIEWER_ROLE.id },
      stagePersonasSet: true,
    });
    renderProject();
    await screen.findByTestId("cfg-personas-prj-bbbb2222");
    await user.click(screen.getByTestId("cfg-personas-prj-bbbb2222-manage"));
    const warning = screen.getByTestId("cfg-personas-prj-bbbb2222-warning");
    expect(warning).toHaveTextContent(/suppresses the inherited in_review persona/);
    expect(warning).toHaveTextContent(/Code Reviewer/);
    // The one-click import restores the INHERITED role persona (the
    // select shows it; nothing else renders in the row).
    await user.click(screen.getByTestId("cfg-personas-prj-bbbb2222-add-reviewer"));
    expect(screen.getByTestId("cfg-personas-prj-bbbb2222-in_review-role").textContent).toContain("Code Reviewer");
    expectNoTextInputs(screen.getByTestId("cfg-personas-prj-bbbb2222"));
  });

  it("F2 project-tier pin: importing an inherited UNRESOLVABLE reviewer ref copies the reference verbatim — it rides, never stripped", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue({
      ...USER,
      stagePersonas: { in_review: "auditor-role-id" },
      stagePersonasSet: true,
    });
    renderProject();
    await screen.findByTestId("cfg-personas-prj-bbbb2222");
    await user.click(screen.getByTestId("cfg-personas-prj-bbbb2222-manage"));
    const warning = screen.getByTestId("cfg-personas-prj-bbbb2222-warning");
    expect(warning).toHaveTextContent(/suppresses the inherited in_review persona/);
    // The inherited ref is a role reference the client vocabulary
    // cannot resolve — under role-only that is the closest live shape
    // to the old role-less entry: the warning names the raw ref.
    expect(warning).toHaveTextContent(/auditor-role-id/);
    await user.click(screen.getByTestId("cfg-personas-prj-bbbb2222-add-reviewer"));
    // The import copies the REFERENCE into the draft (a role-bearing
    // entry): the row shows the raw ref as its own option, the
    // warning legitimately dismisses, and the save persists the ref
    // verbatim — the server validates against the real vocabulary.
    expect(screen.getByTestId("cfg-personas-prj-bbbb2222-in_review-row").textContent).toContain(
      "auditor-role-id",
    );
    expect(screen.queryByTestId("cfg-personas-prj-bbbb2222-warning")).not.toBeInTheDocument();
  });

  it("a stored override map renders as role selects and saves a RE-PICKED role", async () => {
    const user = userEvent.setup();
    const project = { ...PROJECT, short_id: "prj-cccc3333" };
    renderProject(project);
    const role = await screen.findByTestId("cfg-personas-prj-cccc3333-in_review-role");
    expect(role.textContent).toContain("Code Reviewer");
    expectNoTextInputs(screen.getByTestId("cfg-personas-prj-cccc3333"));
    await user.click(role);
    await user.click(await screen.findByRole("option", { name: "Builder (org)" }));
    await user.click(screen.getByTestId("cfg-save-prj-cccc3333"));
    await waitFor(() => expect(boardApi.setProjectBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setProjectBoardConfig).mock.calls[0]![1];
    expect((body as { stagePersonas?: PersonaMapShape }).stagePersonas?.in_review).toBe(BUILD_ROLE.id);
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

  it("role-only editing — a slug-referenced persona renders the role select ONLY (no textareas, no per-row note); re-picking saves the new reference", async () => {
    const user = userEvent.setup();
    // A stored persona as the bare SLUG reference (the platform
    // accepts id or slug — both address the same role).
    vi.mocked(boardApi.getProjectBoardConfig).mockImplementation(async () => ({
      project_id: "prj-dddd4444",
      overrides: {
        stagePersonas: { in_review: "reviewer" },
      },
      effective: {
        ...USER,
        stagePersonasSet: true,
        stagePersonas: { in_review: "reviewer" },
      },
    }));
    renderProject();
    const row = await screen.findByTestId("cfg-personas-prj-bbbb2222-in_review-row");
    // (the select renders the referenced role's label)
    expect(row.textContent).toContain("Code Reviewer (platform)");
    expect(screen.queryByTestId("cfg-personas-prj-bbbb2222-in_review-name")).not.toBeInTheDocument();
    expect(screen.queryByTestId("cfg-personas-prj-bbbb2222-in_review-instructions")).not.toBeInTheDocument();
    expect(screen.queryByTestId("cfg-personas-prj-bbbb2222-in_review-role-note")).not.toBeInTheDocument();
    expectNoTextInputs(screen.getByTestId("cfg-personas-prj-bbbb2222"));
    // Re-pick a different role → save carries the new reference (a
    // role-bearing entry is never stripped).
    await user.click(screen.getByTestId("cfg-personas-prj-bbbb2222-in_review-role"));
    await user.click(await screen.findByRole("option", { name: "Builder (org)" }));
    await user.click(screen.getByTestId("cfg-save-prj-bbbb2222"));
    await waitFor(() => expect(boardApi.setProjectBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setProjectBoardConfig).mock.calls[0]![1];
    const sent = (body as { stagePersonas?: PersonaMapShape }).stagePersonas!;
    expect(sent.in_review).toBe(BUILD_ROLE.id);
  });

  it("a stale project-tier reference rides as its own option (the drift stays visible) and carries back verbatim", async () => {
    const user = userEvent.setup();
    vi.mocked(boardApi.getProjectBoardConfig).mockImplementation(async () => ({
      project_id: "prj-eeee5555",
      overrides: {
        stagePersonas: { in_review: "deleted-role" },
      },
      effective: {
        ...USER,
        stagePersonasSet: true,
        stagePersonas: { in_review: "deleted-role" },
      },
    }));
    renderProject();
    const row = await screen.findByTestId("cfg-personas-prj-bbbb2222-in_review-row");
    // The unresolvable reference renders as its own option — visible
    // and preserved (the config view renders the drift; the owner
    // fixes it by re-picking or removing).
    expect(row.textContent).toContain("deleted-role");
    expectNoTextInputs(screen.getByTestId("cfg-personas-prj-bbbb2222"));
    // Dirty an unrelated key so the save fires; the stored reference
    // carries back verbatim (REPLACE never silently wipes personas).
    await user.click(screen.getByTestId("cfg-ov-prj-bbbb2222-reviewColumn"));
    await user.click(await screen.findByRole("option", { name: "manual" }));
    await user.click(screen.getByTestId("cfg-save-prj-bbbb2222"));
    await waitFor(() => expect(boardApi.setProjectBoardConfig).toHaveBeenCalled());
    const [id, body] = vi.mocked(boardApi.setProjectBoardConfig).mock.calls[0]!;
    expect(id).toBe("prj-bbbb2222");
    expect((body as { reviewColumn?: string }).reviewColumn).toBe("manual");
    expect((body as { stagePersonas?: unknown }).stagePersonas).toEqual({ in_review: "deleted-role" });
  });
});
