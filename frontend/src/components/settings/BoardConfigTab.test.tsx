import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render as rtlRender } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { BoardConfigTab } from "./BoardConfigTab";
import { boardApi, type BoardConfig, type ProjectBoardConfig } from "../../api/board";

vi.mock("../../api/board", () => ({
  boardApi: {
    listProjects: vi.fn(),
    getUserBoardConfig: vi.fn(),
    setUserBoardConfig: vi.fn(),
    getProjectBoardConfig: vi.fn(),
    setProjectBoardConfig: vi.fn(),
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
// override — a Settings save of the OTHER keys must carry it back
// verbatim (the server PUT is REPLACE: absent keys are deletes).
const PC_PERSONAS: ProjectBoardConfig = {
  project_id: "prj-cccc3333",
  overrides: { stagePersonas: { in_review: { name: "auditor", instructions: "Verify." } } },
  effective: { ...USER, stagePersonas: { in_review: { name: "auditor", instructions: "Verify." } } },
};

function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return rtlRender(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <BoardConfigTab />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(boardApi.getUserBoardConfig).mockResolvedValue(USER);
  vi.mocked(boardApi.listProjects).mockResolvedValue([
    { id: "p1", short_id: "prj-aaaa1111", name: "Apollo", status: "active", tags: [], created_at: "", updated_at: "", worker_slots: 2, target_workspace_id: "ws-1" },
    { id: "p2", short_id: "prj-bbbb2222", name: "Borealis", status: "active", tags: [], created_at: "", updated_at: "", worker_slots: 2, target_workspace_id: "ws-1" },
    { id: "p3", short_id: "prj-cccc3333", name: "Cerberus", status: "active", tags: [], created_at: "", updated_at: "", worker_slots: 2, target_workspace_id: "ws-1" },
  ] as never);
  vi.mocked(boardApi.getProjectBoardConfig).mockImplementation(async (id: string) =>
    id === "prj-aaaa1111" ? PC_A : id === "prj-cccc3333" ? PC_PERSONAS : PC_B,
  );
  vi.mocked(boardApi.setUserBoardConfig).mockResolvedValue(USER);
  vi.mocked(boardApi.setProjectBoardConfig).mockImplementation(async (id: string) => (id === "prj-aaaa1111" ? PC_A : PC_B));
});

describe("BoardConfigTab — #1832 item 6 (2/2)", () => {
  it("renders the global defaults from GET /me/board/config", async () => {
    renderTab();
    expect(await screen.findByText("Board defaults")).toBeInTheDocument();
    expect((await screen.findByTestId("cfg-reviewColumn") as HTMLButtonElement).textContent).toContain("manual");
  });

  it("Save writes the full config through the API (typed body, requestId present)", async () => {
    const user = userEvent.setup();
    renderTab();
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
    renderTab();
    await screen.findByTestId("cfg-save-global");
    const slots = screen.getByTestId("cfg-defaultWorkerSlots");
    await user.type(slots.querySelector("input")!, "4");
    await user.click(screen.getByTestId("cfg-save-global"));
    await waitFor(() => expect(boardApi.setUserBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setUserBoardConfig).mock.calls[0]![0];
    expect(body.stagePersonas).toEqual({ in_review: { name: "auditor", instructions: "Verify." } });
  });

  it("client validation mirrors the server: slots < 1 blocks the send (no PUT fires)", async () => {
    const user = userEvent.setup();
    renderTab();
    const slots = await screen.findByTestId("cfg-defaultWorkerSlots");
    await user.clear(slots.querySelector("input")!);
    await user.type(slots.querySelector("input")!, "0");
    const save = screen.getByTestId("cfg-save-global") as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(boardApi.setUserBoardConfig).not.toHaveBeenCalled();
  });

  it("a project row shows the effective config with provenance (override vs inherited)", async () => {
    renderTab();
    expect(await screen.findByText("Apollo")).toBeInTheDocument();
    expect(screen.getByTestId("cfg-provenance-prj-aaaa1111-reviewColumn").textContent).toContain("override");
    expect(screen.getByTestId("cfg-provenance-prj-aaaa1111-autoAssignReviewer").textContent).toContain("inherited");
    expect(screen.getByTestId("cfg-provenance-prj-bbbb2222-reviewColumn").textContent).toContain("inherited");
  });

  it("a project override PUT carries ONLY the overridden key — absent = inherit, and defaultWorkerSlots is never sent", async () => {
    const user = userEvent.setup();
    renderTab();
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

  it("round-1 pin: saving a project's OTHER keys carries the stored stagePersonas override back verbatim (REPLACE never silently wipes personas)", async () => {
    const user = userEvent.setup();
    renderTab();
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
    // The toggles-only save carries personas verbatim; nothing else rides.
    expect(Object.keys(body).sort()).toEqual(["reviewColumn", "stagePersonas"]);
  });

  it("Inherit-everything sends {} (full inheritance)", async () => {
    const user = userEvent.setup();
    renderTab();
    await screen.findByText("Apollo");
    const sel = await screen.findByTestId("cfg-ov-prj-aaaa1111-reviewColumn");
    await user.click(sel);
    const inheritOption = (await screen.findAllByRole("option", { name: "Inherit" }))[0]!;
    await user.click(inheritOption);
    await user.click(screen.getByTestId("cfg-save-prj-aaaa1111"));
    await waitFor(() => expect(boardApi.setProjectBoardConfig).toHaveBeenCalled());
    const body = vi.mocked(boardApi.setProjectBoardConfig).mock.calls[0]![1];
    expect(body).toEqual({});
  });

  it("a failed global PUT renders the error and keeps the editor (no swallow)", async () => {
    vi.mocked(boardApi.setUserBoardConfig).mockRejectedValueOnce(new Error("400: unknown key"));
    const user = userEvent.setup();
    renderTab();
    const slots = await screen.findByTestId("cfg-defaultWorkerSlots");
    await user.clear(slots.querySelector("input")!);
    await user.type(slots.querySelector("input")!, "4");
    await user.click(screen.getByTestId("cfg-save-global"));
    expect(await screen.findByTestId("cfg-error-global")).toBeInTheDocument();
  });
});
