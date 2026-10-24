import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  boardApi,
  BOARD_REVIEW_COLUMNS,
  type BoardConfig,
  type ProjectBoardConfigOverrideInput,
} from "../../api/board";
import { Select } from "../ui/Select";
import { NumberInput } from "../ui/NumberInput";
import { Button } from "../ui/Button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../ui/Card";

// #1832 item 6 (2/2): the board-wide Config section. Two tiers, mirroring
// the API exactly:
//   - user-level board defaults (all three keys, full editor);
//   - per-project overrides (reviewColumn + autoAssignReviewer ONLY —
//     defaultWorkerSlots is a user-level key the project tier rejects).
// Override semantics are REPLACE with absent = inherit: the project PUT
// carries ONLY the explicitly overridden keys; Inherit sends the key's
// absence. Client validation mirrors the server (enum, int >= 1) and
// blocks the send BEFORE any request fires.

type ProjectDraft = { reviewColumn: string; autoAssignReviewer: string }; // "" = inherit

export function BoardConfigTab() {
  const qc = useQueryClient();
  const userCfg = useQuery({ queryKey: ["board", "config"], queryFn: () => boardApi.getUserBoardConfig() });
  const projects = useQuery({ queryKey: ["board", "projects"], queryFn: () => boardApi.listProjects() });

  const [global, setGlobal] = useState<BoardConfig | null>(null);
  const [globalError, setGlobalError] = useState<string | null>(null);
  const [drafts, setDrafts] = useState<Record<string, ProjectDraft>>({});
  const [projErrors, setProjErrors] = useState<Record<string, string>>({});

  const g: BoardConfig | null = global ?? userCfg.data ?? null;
  const globalDirty = global !== null && JSON.stringify(global) !== JSON.stringify(userCfg.data);
  const slotsInvalid = g !== null && (!Number.isInteger(g.defaultWorkerSlots) || g.defaultWorkerSlots < 1);

  const saveGlobal = useMutation({
    mutationFn: (body: BoardConfig) =>
      boardApi.setUserBoardConfig({
        reviewColumn: body.reviewColumn,
        autoAssignReviewer: body.autoAssignReviewer,
        defaultWorkerSlots: body.defaultWorkerSlots,
      }),
    onSuccess: () => {
      setGlobalError(null);
      qc.invalidateQueries({ queryKey: ["board"] });
    },
    onError: (e: Error) => setGlobalError(e.message),
  });

  const saveProject = useMutation({
    mutationFn: ({ id, body }: { id: string; body: ProjectBoardConfigOverrideInput }) =>
      boardApi.setProjectBoardConfig(id, body),
    onSuccess: (_d, v) => {
      setProjErrors((p) => ({ ...p, [v.id]: "" }));
      qc.invalidateQueries({ queryKey: ["board"] });
    },
    onError: (e: Error, v) => setProjErrors((p) => ({ ...p, [v.id]: e.message })),
  });

  if (userCfg.isLoading) return <p className="text-sm text-muted-foreground">Loading board config…</p>;
  if (userCfg.isError) return <p className="text-sm text-destructive">Failed to load board config.</p>;

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>Board defaults</CardTitle>
          <CardDescription>
            Your board-wide defaults. Projects inherit these unless they carry an override.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-center gap-6">
            <label className="flex items-center gap-2 text-sm">
              Review column
              <span data-testid="cfg-reviewColumn">
                <Select
                  value={g?.reviewColumn ?? "auto"}
                  onValueChange={(v) => setGlobal({ ...(g as BoardConfig), reviewColumn: v })}
                  options={[...BOARD_REVIEW_COLUMNS]}
                />
              </span>
            </label>
            <label className="flex items-center gap-2 text-sm">
              Auto-assign reviewer
              <span data-testid="cfg-autoAssignReviewer">
                <Select
                  value={(g?.autoAssignReviewer ?? true) ? "on" : "off"}
                  onValueChange={(v) => setGlobal({ ...(g as BoardConfig), autoAssignReviewer: v === "on" })}
                  options={["on", "off"]}
                />
              </span>
            </label>
            <label className="flex items-center gap-2 text-sm">
              Default worker slots
              <span data-testid="cfg-defaultWorkerSlots">
                <NumberInput
                  value={g?.defaultWorkerSlots ?? 2}
                  min={1}
                  onChange={(v) => setGlobal({ ...(g as BoardConfig), defaultWorkerSlots: Number(v) })}
                />
              </span>
            </label>
            <Button
              data-testid="cfg-save-global"
              disabled={!globalDirty || slotsInvalid || saveGlobal.isPending}
              onClick={() => g && saveGlobal.mutate(g)}
            >
              Save defaults
            </Button>
          </div>
          {slotsInvalid && (
            <p className="text-sm text-destructive" data-testid="cfg-invalid-slots">
              defaultWorkerSlots must be an integer ≥ 1 (the server rejects anything else).
            </p>
          )}
          {globalError && (
            <p className="text-sm text-destructive" data-testid="cfg-error-global">
              {globalError}
            </p>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Per-project overrides</CardTitle>
          <CardDescription>
            Absent keys inherit your defaults (REPLACE semantics — Save sends only the keys you override).
            defaultWorkerSlots stays board-wide.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {projects.data?.map((p) => (
            <ProjectRow
              key={p.id}
              shortId={p.short_id}
              name={p.name}
              userCfg={userCfg.data as BoardConfig}
              draft={drafts[p.short_id]}
              onDraft={(d) => setDrafts((prev) => ({ ...prev, [p.short_id]: d }))}
              error={projErrors[p.short_id]}
              pending={saveProject.isPending}
              onSave={(body) => saveProject.mutate({ id: p.short_id, body })}
            />
          ))}
          {projects.data?.length === 0 && (
            <p className="text-sm text-muted-foreground">No board projects yet.</p>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function ProjectRow(props: {
  shortId: string;
  name: string;
  userCfg: BoardConfig;
  draft: ProjectDraft | undefined;
  onDraft: (d: ProjectDraft) => void;
  error: string | undefined;
  pending: boolean;
  onSave: (body: ProjectBoardConfigOverrideInput) => void;
}) {
  const projCfg = useQuery({
    queryKey: ["board", "projectConfig", props.shortId],
    queryFn: () => boardApi.getProjectBoardConfig(props.shortId),
  });
  if (projCfg.isLoading) return <p className="text-sm text-muted-foreground">{props.name}: loading…</p>;
  if (projCfg.isError) return <p className="text-sm text-destructive">{props.name}: failed to load config.</p>;

  const pc = projCfg.data;
  if (!pc) return null;
  const d: ProjectDraft = props.draft ?? {
    reviewColumn: pc.overrides.reviewColumn ?? "",
    autoAssignReviewer: pc.overrides.autoAssignReviewer === undefined ? "" : pc.overrides.autoAssignReviewer ? "on" : "off",
  };
  const dirty = props.draft !== undefined;
  const body: ProjectBoardConfigOverrideInput = {
    ...(d.reviewColumn !== "" ? { reviewColumn: d.reviewColumn } : {}),
    ...(d.autoAssignReviewer !== "" ? { autoAssignReviewer: d.autoAssignReviewer === "on" } : {}),
  };
  const prov = (o: { k: string; v: boolean }) => (
    <span
      data-testid={`cfg-provenance-${props.shortId}-${o.k}`}
      className="text-xs text-muted-foreground"
    >
      {o.v ? "override" : "inherited"}
    </span>
  );

  return (
    <div className="rounded-md border border-border p-3 space-y-2">
      <div className="flex items-center justify-between gap-4">
        <span className="text-sm font-medium">{props.name}</span>
        <Button
          data-testid={`cfg-save-${props.shortId}`}
          disabled={!dirty || props.pending}
          onClick={() => props.onSave(body)}
        >
          Save overrides
        </Button>
      </div>
      <div className="flex flex-wrap items-center gap-6 text-sm">
        <label className="flex items-center gap-2">
          Review column
          <span data-testid={`cfg-ov-${props.shortId}-reviewColumn`}>
            <Select
              value={d.reviewColumn === "" ? "Inherit" : d.reviewColumn}
              onValueChange={(v) => props.onDraft({ ...d, reviewColumn: v === "Inherit" ? "" : v })}
              options={["Inherit", ...BOARD_REVIEW_COLUMNS]}
            />
          </span>
          {prov({ k: "reviewColumn", v: pc.overrides.reviewColumn !== undefined })}
        </label>
        <label className="flex items-center gap-2">
          Auto-assign reviewer
          <span data-testid={`cfg-ov-${props.shortId}-autoAssignReviewer`}>
            <Select
              value={d.autoAssignReviewer === "" ? "Inherit" : d.autoAssignReviewer === "on" ? "On" : "Off"}
              onValueChange={(v) => props.onDraft({ ...d, autoAssignReviewer: v === "Inherit" ? "" : v === "On" ? "on" : "off" })}
              options={["Inherit", "On", "Off"]}
            />
          </span>
          {prov({ k: "autoAssignReviewer", v: pc.overrides.autoAssignReviewer !== undefined })}
        </label>
        <span className="text-xs text-muted-foreground">
          effective: {pc.effective.reviewColumn} · {pc.effective.autoAssignReviewer ? "on" : "off"} · slots{" "}
          {pc.effective.defaultWorkerSlots}
        </span>
      </div>
      {props.error ? (
        <p className="text-sm text-destructive" data-testid={`cfg-error-${props.shortId}`}>
          {props.error}
        </p>
      ) : null}
    </div>
  );
}
