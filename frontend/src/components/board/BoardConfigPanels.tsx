// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from "react";
import * as Dialog from "@radix-ui/react-dialog";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Settings2, X } from "lucide-react";
import {
  boardApi,
  BOARD_REVIEW_COLUMNS,
  type BoardConfig,
  type BoardPersonaConfig,
  type BoardProject,
  type ProjectBoardConfigOverrideInput,
} from "../../api/board";
import { agentRolesApi, type AgentRole } from "../../api/agentRoles";
import { Select } from "../ui/Select";
import { NumberInput } from "../ui/NumberInput";
import { Button } from "../ui/Button";
import {
  DEFAULT_REVIEWER_PERSONA,
  PERSONA_ELIGIBLE_STAGES,
  STAGE_LABELS,
} from "../../lib/boardRules";

// BoardConfigPanels.tsx — the board's configuration surfaces, ON the
// board (owner ruling 2026-10-28: "The taskboard settings should not
// be in 'Settings' — that doesn't make sense for something as dynamic
// as the board. Configuration should be alongside the projects and on
// the board itself."). Two panels:
//
//   - BoardDefaultsDialog — the user-tier board-wide defaults
//     (reviewColumn, autoAssignReviewer, defaultWorkerSlots, stage
//     personas), opened from the board header;
//   - ProjectConfigTrigger — the per-project gear on every swimlane
//     header (active lanes AND the archived drawer): worker slots
//     (the project PATCH) + the project-tier config overrides +
//     the persona editor with the effective merged view.
//
// Config semantics are the #1832 item 6 contract: REPLACE with
// absent = inherit, the stagePersonas set-ness rule (never
// materialize {} when nothing was stored; never silently wipe a
// stored map — the round-1 wipe-protection, preserved verbatim in
// the save bodies below), the {} opt-out, and the
// legacy-reviewer-suppression warning with its one-click
// default-reviewer import.

/** The persona editor's draft: whether an explicit stagePersonas key
 * will be sent (the set-ness surface — sending {} is the opt-out,
 * sending nothing inherits), and the map itself. */
export type PersonaDraft = { explicit: boolean; map: Record<string, BoardPersonaConfig> };

const EMPTY_PERSONA_MAP: Record<string, BoardPersonaConfig> = {};

function personaDraftOf(stored: Record<string, BoardPersonaConfig> | undefined, explicitStored: boolean): PersonaDraft {
  return { explicit: explicitStored, map: stored ?? EMPTY_PERSONA_MAP };
}

function personaDraftDirty(draft: PersonaDraft, stored: PersonaDraft): boolean {
  return draft.explicit !== stored.explicit || JSON.stringify(draft.map) !== JSON.stringify(stored.map);
}

// --- Role-ONLY persona editing (owner ruling 2026-10-29, round 2) --------------
//
// "We can remove any editor feature for the DX. It's not role-first,
// it's role-ONLY." A persona IS a reference to a platform agent role
// (the member-readable GET /me/agent-roles vocabulary) — the role's
// model/tools/system-prompt are the single source of truth. The
// editor offers exactly ONE authoring surface per stage: the role
// select (+ Remove). The legacy name input and the instructions
// override textarea are GONE from the DOM — instructions are not an
// editor surface anymore (the backend's default-reviewer composition
// and the stored-shape decode are the only writers/readers of them;
// the editor never shows them). There is no "no role" option: a
// picker row with no role chosen is not persisted (dropped on save).

/** The role picker's placeholder — what an unpicked row's select
 * shows (NOT an option; nothing role-less can be authored). */
const PERSONA_PICK_ROLE = "Pick a role…";

/** The role-only persistence rule (applied at BOTH tiers' save
 * bodies): an entry with no role reference is not a persona — an
 * added-but-unpicked row, or a stored legacy {name, instructions}
 * entry, is dropped from the outgoing map. Entries carrying a roleId
 * ride verbatim (name/instructions included — the stored shape is
 * untouched; only the editor stopped authoring them). */
function persistablePersonaMap(map: Record<string, BoardPersonaConfig>): Record<string, BoardPersonaConfig> {
  const out: Record<string, BoardPersonaConfig> = {};
  for (const [stage, p] of Object.entries(map)) {
    if (p.roleId) out[stage] = p;
  }
  return out;
}

/** One role's dropdown label — the display the editor and the status
 * lines render (name, disambiguated by scope). */
function roleLabel(r: AgentRole): string {
  return `${r.name} (${r.scope})`;
}

/** The role a persona's roleId addresses (id or slug — the platform
 * accepts both), or null for a role-less/legacy persona. */
function personaRole(p: BoardPersonaConfig | undefined, roles: AgentRole[]): AgentRole | null {
  if (!p?.roleId) return null;
  return roles.find((r) => r.id === p.roleId || r.slug === p.roleId) ?? null;
}

/** A persona's display label for status lines: the role's name when
 * referenced, else the legacy name, else the raw reference. */
function personaLabel(p: BoardPersonaConfig | undefined, roles: AgentRole[]): string {
  if (!p) return "";
  const r = personaRole(p, roles);
  if (r) return r.name;
  return p.name || p.roleId || "";
}

/** The one-click reviewer import (the suppression warning's button):
 * the reviewer AGENT ROLE when the vocabulary carries one (the
 * default-reviewer composition points at it — ruling 2026-10-29),
 * with the mirrored charter prose riding the stored shape (the
 * editor never shows it — role-ONLY). When no reviewer role exists
 * the legacy name+instructions shape is returned as an UNPICKED
 * PICKER ROW (there is no legacy lane to display it in — role-only):
 * it carries no roleId, so a save drops it, and because the guards
 * below read the PERSISTABLE map the warning stays up until a role
 * is actually picked — never a silent suppression. */
function reviewerImport(roles: AgentRole[]): BoardPersonaConfig {
  const role = roles.find((r) => r.slug === "reviewer" || r.name === "reviewer");
  if (role) {
    return { roleId: role.id, name: DEFAULT_REVIEWER_PERSONA.name, instructions: DEFAULT_REVIEWER_PERSONA.instructions };
  }
  return { name: DEFAULT_REVIEWER_PERSONA.name, instructions: DEFAULT_REVIEWER_PERSONA.instructions };
}

// The legacy-composition hazard, shared by both tiers: an explicit map
// without an in_review entry suppresses whatever in_review runs on
// today — the built-in auto-reviewer persona when the composition is
// live (reviewColumn=auto + autoAssignReviewer), or the tier-above
// persona when an explicit map is inherited. One click restores it.
function PersonaSuppressWarning(props: {
  testPrefix: string;
  kind: "composition" | "inherited";
  inheritedPersona?: BoardPersonaConfig;
  roles: AgentRole[];
  onImport: () => void;
}) {
  return (
    <div
      className="rounded-md border border-amber-500/40 bg-amber-500/10 p-2.5 text-sm"
      data-testid={`${props.testPrefix}-warning`}
    >
      <p>
        {props.kind === "composition" ? (
          <>
            Saving this explicit map without an <strong>in_review</strong> entry suppresses the built-in
            auto-reviewer persona (reviewColumn=auto + autoAssignReviewer composition) — explicit maps own every
            stage, listed or not.
          </>
        ) : (
          <>
            Saving this override without an <strong>in_review</strong> entry suppresses the inherited in_review
            persona <strong>{personaLabel(props.inheritedPersona, props.roles)}</strong> — an explicit override map
            owns every stage, listed or not.
          </>
        )}
      </p>
      <Button
        variant="outline"
        size="sm"
        className="mt-2"
        data-testid={`${props.testPrefix}-add-reviewer`}
        onClick={props.onImport}
      >
        Add the{" "}
        {props.kind === "composition"
          ? "default reviewer persona"
          : `inherited “${personaLabel(props.inheritedPersona, props.roles)}” persona`}
      </Button>
    </div>
  );
}

/** The stage → persona editor, both tiers. ROLE-ONLY (owner ruling
 * 2026-10-29, round 2): a persona IS a referenced agent role, and the
 * editor's single authoring surface per listed stage is the role
 * select + Remove — no name input, no instructions textarea, no
 * role-less option anywhere. "Add persona" opens the role picker
 * directly; a row with no role chosen is not persisted (dropped on
 * save). An explicit "no persona" row renders when the map does not
 * list the stage (the explicit-key-owns-all-stages semantics made
 * visible); what the stage runs on TODAY (inherited persona, default
 * reviewer via the composition, or nothing) renders as context while
 * editing. */
export function PersonaEditor(props: {
  tier: "user" | "project";
  testPrefix: string;
  draft: PersonaDraft;
  onDraft: (d: PersonaDraft) => void;
  /** Whether the legacy auto-review composition would render the
   * default reviewer for in_review absent any explicit map. */
  compositionActive: boolean;
  /** The tier-above explicit map (project tier: the user's stored
   * stagePersonas when their map is explicit; null = no explicit map
   * above → the composition is what in_review runs on). */
  inherited: Record<string, BoardPersonaConfig> | null;
  /** The selectable agent-role vocabulary (GET /me/agent-roles). May
   * be empty (load failure / none defined) — editing degrades to the
   * legacy lane. */
  roles: AgentRole[];
}) {
  const { draft, onDraft } = props;
  const roles = props.roles;
  const setMap = (map: Record<string, BoardPersonaConfig>) => onDraft({ ...draft, map });

  const effectiveInherited = (stage: string): BoardPersonaConfig | undefined =>
    props.inherited !== null ? props.inherited[stage] : undefined;
  const inheritedReviewer = effectiveInherited("in_review");
  // The guards read the PERSISTABLE map — exactly what a save sends
  // (persistablePersonaMap drops role-less entries at both save
  // bodies) — NEVER the raw draft: a role-less in_review entry (a
  // stored legacy {name, instructions} shape, an added-but-unpicked
  // row, or the import's no-reviewer-role fallback) does NOT count
  // as an in_review persona. Reading the raw map masked the
  // suppression the warning exists to catch (review F1–F3, PR #1988:
  // warning hidden, save sends stagePersonas: {}).
  const persistable = persistablePersonaMap(draft.map);
  // The suppression hazard: an explicit map about to be saved with
  // no in_review entry, while in_review currently runs on SOMETHING.
  const suppressWarning =
    draft.explicit && !persistable["in_review"] && (props.compositionActive || inheritedReviewer !== undefined);

  // The picker's option list: every available role, labeled for
  // display. There is NO role-less option (role-ONLY). An
  // unresolvable stored reference rides as its own option so the
  // value stays visible and preserved.
  const roleOptions = (current?: string): string[] => {
    const opts = roles.map(roleLabel);
    if (current && !opts.includes(current)) opts.push(current);
    return opts;
  };
  // "" = unpicked (the placeholder renders; the entry will not
  // persist until a role is chosen).
  const roleSelectValue = (p: BoardPersonaConfig): string => {
    const r = personaRole(p, roles);
    if (r) return roleLabel(r);
    return p.roleId ?? "";
  };
  const onRoleSelect = (stage: string, p: BoardPersonaConfig, label: string) => {
    const r = roles.find((role) => roleLabel(role) === label);
    if (r) setMap({ ...draft.map, [stage]: { ...p, roleId: r.id, name: r.name } });
  };

  return (
    <div className="rounded-md border border-border p-3 space-y-3" data-testid={props.testPrefix}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 className="text-sm font-semibold">Stage personas</h3>
          <p className="text-xs text-muted-foreground">
            {props.tier === "user"
              ? "Your board-wide stage → persona map. Projects inherit it unless they override."
              : "This project's override map. An explicit map replaces the inherited one wholesale."}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {!draft.explicit ? (
            <>
              {props.tier === "project" && (
                <Button
                  variant="outline"
                  size="sm"
                  data-testid={`${props.testPrefix}-optout`}
                  title="override with empty map = no personas for this project"
                  onClick={() => onDraft({ explicit: true, map: {} })}
                >
                  Suppress inherited personas
                </Button>
              )}
              <Button
                variant="outline"
                size="sm"
                data-testid={`${props.testPrefix}-manage`}
                onClick={() => onDraft({ explicit: true, map: {} })}
              >
                Manage personas
              </Button>
            </>
          ) : (
            <Button
              variant="outline"
              size="sm"
              data-testid={`${props.testPrefix}-revert`}
              onClick={() => onDraft({ explicit: false, map: {} })}
            >
              {props.tier === "user" ? "Revert to defaults" : "Inherit"}
            </Button>
          )}
        </div>
      </div>

      {/* The note describes what SAVING materializes: a map whose
       * entries are all role-less persists as {} (the drop rule) —
       * keyed on the persistable map, same as the save bodies. */}
      {draft.explicit && Object.keys(persistable).length === 0 && (
        <p className="text-xs text-amber-600 dark:text-amber-400" data-testid={`${props.testPrefix}-empty-note`}>
          {props.tier === "project"
            ? "Override with empty map = no personas for this project (the inherited map is suppressed)."
            : "Explicit empty map = no personas for any inheriting project (the built-in auto-reviewer is suppressed too)."}
        </p>
      )}

      {roles.length === 0 && (
        <p className="text-xs text-muted-foreground" data-testid={`${props.testPrefix}-no-roles`}>
          No agent roles are available to select — personas reference agent roles, so define the roles first.
        </p>
      )}

      <p className="text-xs text-muted-foreground" data-testid={`${props.testPrefix}-role-only-note`}>
        Each configured stage runs a referenced agent role — the role's model, tools, and system prompt are the single
        source of truth. A row with no role picked is not saved.
      </p>

      <div className="space-y-2">
        {PERSONA_ELIGIBLE_STAGES.map((stage) => {
          const persona = draft.map[stage];
          if (!draft.explicit) {
            // What the stage runs on today, read-only.
            const inh = effectiveInherited(stage);
            let status: string;
            if (inh) {
              status = `inherited: “${personaLabel(inh, roles)}”`;
            } else if (stage === "in_review" && props.compositionActive) {
              status = "default reviewer persona (auto-review composition)";
            } else {
              status = "no persona";
            }
            return (
              <div key={stage} className="flex items-center justify-between gap-2 text-sm">
                <span>
                  <span className="font-medium">{STAGE_LABELS[stage]}</span>{" "}
                  <span className="text-muted-foreground" data-testid={`${props.testPrefix}-${stage}-status`}>
                    — {status}
                  </span>
                </span>
                {props.tier === "project" && <span className="text-xs text-muted-foreground">inherited</span>}
              </div>
            );
          }
          if (!persona) {
            // The explicit-key-owns-all-stages semantics, VISIBLE: a
            // stage the map does not list has NO persona — never a
            // silent inheritance.
            return (
              <div key={stage} className="flex items-center justify-between gap-2 text-sm">
                <span>
                  <span className="font-medium">{STAGE_LABELS[stage]}</span>{" "}
                  <span className="text-muted-foreground" data-testid={`${props.testPrefix}-${stage}-none`}>
                    — no persona (the explicit map owns every stage it does not list)
                  </span>
                </span>
                <Button
                  variant="outline"
                  size="sm"
                  data-testid={`${props.testPrefix}-${stage}-add`}
                  onClick={() => setMap({ ...draft.map, [stage]: {} })}
                >
                  Add persona
                </Button>
              </div>
            );
          }
          return (
            <div key={stage} className="flex flex-wrap items-center gap-2 rounded-md border border-border p-2.5" data-testid={`${props.testPrefix}-${stage}-row`}>
              <label className="flex flex-wrap items-center gap-2">
                <span className="text-sm font-medium">{STAGE_LABELS[stage]}</span>
                <span data-testid={`${props.testPrefix}-${stage}-role`}>
                  <Select
                    value={roleSelectValue(persona)}
                    onValueChange={(v) => onRoleSelect(stage, persona, v)}
                    options={roleOptions(roleSelectValue(persona))}
                    placeholder={PERSONA_PICK_ROLE}
                  />
                </span>
              </label>
              <Button
                variant="outline"
                size="sm"
                data-testid={`${props.testPrefix}-${stage}-remove`}
                onClick={() => {
                  const next = { ...draft.map };
                  delete next[stage];
                  setMap(next);
                }}
              >
                Remove
              </Button>
            </div>
          );
        })}
      </div>

      {suppressWarning && (
        <PersonaSuppressWarning
          testPrefix={props.testPrefix}
          kind={inheritedReviewer !== undefined ? "inherited" : "composition"}
          inheritedPersona={inheritedReviewer}
          roles={roles}
          onImport={() =>
            onDraft({
              ...draft,
              map: {
                ...draft.map,
                in_review: inheritedReviewer !== undefined ? { ...inheritedReviewer } : reviewerImport(roles),
              },
            })
          }
        />
      )}
    </div>
  );
}


const dialogContentClass =
  "fixed left-1/2 top-1/2 z-50 flex max-h-[85vh] w-[36rem] max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col gap-3 overflow-y-auto rounded-md border border-border bg-background p-4 shadow-xl";
const dialogOverlayClass = "fixed inset-0 bg-black/40 z-50";

/** BoardDefaultsDialog — the user-tier board-wide defaults, ON the
 * board (opened from the board header; the Settings tab is gone).
 * Controlled: the board owns the open state (the /settings/board-config
 * redirect rides ?config=defaults to auto-open it). */
export function BoardDefaultsDialog(props: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const qc = useQueryClient();
  const userCfg = useQuery({
    queryKey: ["board", "config"],
    queryFn: () => boardApi.getUserBoardConfig(),
    enabled: props.open,
  });
  // The persona editor's role vocabulary (member-readable — platform +
  // the acting user's org roles). Load failures degrade to the legacy
  // editor lane (roles=[]).
  const rolesCfg = useQuery({
    queryKey: ["agent-roles", "me"],
    queryFn: () => agentRolesApi.listForMe(),
    enabled: props.open,
  });
  const [global, setGlobal] = useState<BoardConfig | null>(null);
  const [globalPersonas, setGlobalPersonas] = useState<PersonaDraft | null>(null);
  const [error, setError] = useState<string | null>(null);

  const g: BoardConfig | null = global ?? userCfg.data ?? null;
  // The persona draft starts from the STORED set-ness
  // (stagePersonasSet) and the stored map; untouched, a save is
  // byte-identical to the plain round-trip (set-ness is the only safe
  // rule — see the save mutation's comment).
  const storedUserPersonas = personaDraftOf(userCfg.data?.stagePersonas, userCfg.data?.stagePersonasSet ?? false);
  const userPersonas = globalPersonas ?? storedUserPersonas;
  const dirty =
    (global !== null && JSON.stringify(global) !== JSON.stringify(userCfg.data)) ||
    (globalPersonas !== null && personaDraftDirty(globalPersonas, storedUserPersonas));
  const slotsInvalid = g !== null && (!Number.isInteger(g.defaultWorkerSlots) || g.defaultWorkerSlots < 1);

  const save = useMutation({
    mutationFn: (body: { g: BoardConfig; personas: PersonaDraft }) =>
      boardApi.setUserBoardConfig({
        reviewColumn: body.g.reviewColumn,
        autoAssignReviewer: body.g.autoAssignReviewer,
        defaultWorkerSlots: body.g.defaultWorkerSlots,
        // The user PUT is REPLACE-semantics (absent key = revert to
        // the code default). Carry a stagePersonas key ONLY when it is
        // EXPLICIT — either stored (the set-ness rule: sending {} when
        // nothing was stored would MATERIALIZE an opt-out row and
        // silently suppress the flipped default's reviewer persona for
        // every inheriting project; sending nothing when something WAS
        // stored would delete it — the round-1 wipe) or deliberately
        // made explicit in the persona editor (including {} — the
        // visible, labeled opt-out). Role-ONLY: the outgoing map drops
        // every entry with no role reference (unpicked rows and legacy
        // stored shapes — owner ruling 2026-10-29 round 2).
        ...(body.personas.explicit ? { stagePersonas: persistablePersonaMap(body.personas.map) } : {}),
      }),
    onSuccess: () => {
      setError(null);
      setGlobalPersonas(null);
      qc.invalidateQueries({ queryKey: ["board"] });
    },
    onError: (e: Error) => setError(e.message),
  });

  return (
    <Dialog.Root open={props.open} onOpenChange={props.onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className={dialogOverlayClass} />
        <Dialog.Content className={dialogContentClass} data-testid="board-defaults-modal">
          <div className="flex items-center justify-between">
            <Dialog.Title className="text-sm font-semibold">Board defaults</Dialog.Title>
            <Dialog.Close asChild>
              <Button variant="ghost" size="sm" aria-label="Close" data-testid="board-defaults-close">
                <X className="h-4 w-4" aria-hidden />
              </Button>
            </Dialog.Close>
          </div>
          <Dialog.Description className="text-xs text-muted-foreground">
            Your board-wide defaults. Projects inherit these unless they carry an override.
          </Dialog.Description>
          {userCfg.isLoading ? (
            <p className="text-sm text-muted-foreground">Loading board config…</p>
          ) : userCfg.isError ? (
            <p className="text-sm text-destructive">Failed to load board config.</p>
          ) : (
            <>
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
              </div>
              {slotsInvalid && (
                <p className="text-sm text-destructive" data-testid="cfg-invalid-slots">
                  defaultWorkerSlots must be an integer ≥ 1 (the server rejects anything else).
                </p>
              )}
              {error && (
                <p className="text-sm text-destructive" data-testid="cfg-error-global">
                  {error}
                </p>
              )}
              <PersonaEditor
                tier="user"
                testPrefix="cfg-personas-user"
                draft={userPersonas}
                onDraft={setGlobalPersonas}
                // What in_review runs on today when NO explicit map
                // exists anywhere: the legacy auto-review composition.
                compositionActive={(g?.reviewColumn ?? "auto") === "auto" && (g?.autoAssignReviewer ?? true)}
                inherited={null}
                roles={rolesCfg.data ?? []}
              />
              <div className="flex justify-end">
                <Button
                  data-testid="cfg-save-global"
                  disabled={!dirty || slotsInvalid || save.isPending}
                  onClick={() => g && save.mutate({ g, personas: userPersonas })}
                >
                  Save defaults
                </Button>
              </div>
            </>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/** ProjectConfigTrigger — the per-project config gear on the board
 * (every swimlane header, active lanes AND the archived drawer —
 * "alongside the projects", owner ruling 2026-10-28). Self-contained:
 * owns its dialog state. */
export function ProjectConfigTrigger(props: { project: BoardProject }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button
        variant="outline"
        size="sm"
        className="h-6 px-2 text-[11px]"
        onClick={() => setOpen(true)}
        data-testid={`project-config-${props.project.short_id}`}
        title={`${props.project.name} configuration — worker slots, review column, stage personas`}
      >
        <Settings2 className="h-3.5 w-3.5" aria-hidden />
        Configure
      </Button>
      <ProjectConfigDialog project={props.project} open={open} onOpenChange={setOpen} />
    </>
  );
}

/** ProjectConfigDialog — one project's configuration: worker slots
 * (the project PATCH — slots are fixed on the project row, not a
 * config key) + the project-tier config overrides + the persona
 * editor, with the EFFECTIVE merged view (inherited + override
 * badges) from the GetProjectBoardConfig read. */
export function ProjectConfigDialog(props: {
  project: BoardProject;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const qc = useQueryClient();
  const projCfg = useQuery({
    queryKey: ["board", "projectConfig", props.project.short_id],
    queryFn: () => boardApi.getProjectBoardConfig(props.project.short_id),
    enabled: props.open,
  });
  const userCfg = useQuery({
    queryKey: ["board", "config"],
    queryFn: () => boardApi.getUserBoardConfig(),
    enabled: props.open,
  });
  // The persona editor's role vocabulary (shared query key with the
  // defaults dialog — one fetch serves both).
  const rolesCfg = useQuery({
    queryKey: ["agent-roles", "me"],
    queryFn: () => agentRolesApi.listForMe(),
    enabled: props.open,
  });
  const [slots, setSlots] = useState<number | null>(null);
  const [reviewColumn, setReviewColumn] = useState<string | null>(null);
  const [autoAssign, setAutoAssign] = useState<string | null>(null);
  const [personas, setPersonas] = useState<PersonaDraft | null>(null);
  const [error, setError] = useState<string | null>(null);

  const pc = projCfg.data;
  const storedPersonas = pc?.overrides.stagePersonas;
  const storedPersonasDraft = personaDraftOf(storedPersonas, storedPersonas !== undefined);
  const dReview = reviewColumn ?? pc?.overrides.reviewColumn ?? "";
  const dAuto = autoAssign ?? (pc?.overrides.autoAssignReviewer === undefined ? "" : pc.overrides.autoAssignReviewer ? "on" : "off");
  const dPersonas = personas ?? storedPersonasDraft;
  const slotsValue = slots ?? props.project.worker_slots;

  const configDirty =
    personas !== null
      ? personaDraftDirty(dPersonas, storedPersonasDraft) ||
        dReview !== (pc?.overrides.reviewColumn ?? "") ||
        dAuto !== (pc?.overrides.autoAssignReviewer === undefined ? "" : pc.overrides.autoAssignReviewer ? "on" : "off")
      : dReview !== (pc?.overrides.reviewColumn ?? "") || dAuto !== (pc?.overrides.autoAssignReviewer === undefined ? "" : pc.overrides.autoAssignReviewer ? "on" : "off");
  const slotsDirty = slots !== null && slots !== props.project.worker_slots;
  const slotsInvalid = !Number.isInteger(slotsValue) || slotsValue < 1;
  const dirty = configDirty || slotsDirty;

  // The project config PUT is REPLACE-semantics server-side (absent
  // keys are DELETED, not preserved): a save of the toggles must carry
  // the project's stored stagePersonas override back verbatim or
  // saving reviewColumn would silently wipe configured personas (the
  // blocking round-1 review finding). Absent stays absent (inherit) —
  // until the persona editor makes an override explicit. Role-ONLY:
  // the outgoing map drops every entry with no role reference
  // (unpicked rows and legacy stored shapes — owner ruling
  // 2026-10-29 round 2).
  const body: ProjectBoardConfigOverrideInput = {
    ...(dReview !== "" ? { reviewColumn: dReview } : {}),
    ...(dAuto !== "" ? { autoAssignReviewer: dAuto === "on" } : {}),
    ...(dPersonas.explicit ? { stagePersonas: persistablePersonaMap(dPersonas.map) } : {}),
  };

  const save = useMutation({
    mutationFn: async () => {
      // Worker slots ride the PROJECT PATCH (fixed on the project
      // row — not a config key); the config keys ride the override
      // PUT. Two writes, sequential; a partial failure surfaces.
      if (slotsDirty) {
        await boardApi.updateProject(props.project.short_id, { workerSlots: slotsValue });
      }
      if (configDirty) {
        await boardApi.setProjectBoardConfig(props.project.short_id, body);
      }
    },
    onSuccess: () => {
      setError(null);
      setSlots(null);
      setReviewColumn(null);
      setAutoAssign(null);
      setPersonas(null);
      qc.invalidateQueries({ queryKey: ["board"] });
    },
    onError: (e: Error) => setError(e.message),
  });

  // The user tier's explicit map is what this project inherits when
  // its own override is absent (null = no explicit map above: the
  // legacy composition is what in_review runs on).
  const userInherited: Record<string, BoardPersonaConfig> | null =
    userCfg.data?.stagePersonasSet ? (userCfg.data.stagePersonas ?? {}) : null;
  const prov = (k: string, v: boolean) => (
    <span data-testid={`cfg-provenance-${props.project.short_id}-${k}`} className="text-xs text-muted-foreground">
      {v ? "override" : "inherited"}
    </span>
  );

  return (
    <Dialog.Root open={props.open} onOpenChange={props.onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className={dialogOverlayClass} />
        <Dialog.Content className={dialogContentClass} data-testid={`project-config-modal-${props.project.short_id}`}>
          <div className="flex items-center justify-between">
            <Dialog.Title className="text-sm font-semibold">{props.project.name} configuration</Dialog.Title>
            <Dialog.Close asChild>
              <Button variant="ghost" size="sm" aria-label="Close">
                <X className="h-4 w-4" aria-hidden />
              </Button>
            </Dialog.Close>
          </div>
          <Dialog.Description className="text-xs text-muted-foreground">
            Absent config keys inherit your board defaults (REPLACE semantics — Save sends only the keys you override).
          </Dialog.Description>
          {projCfg.isLoading || userCfg.isLoading ? (
            <p className="text-sm text-muted-foreground">Loading {props.project.name} config…</p>
          ) : projCfg.isError ? (
            <p className="text-sm text-destructive">Failed to load config.</p>
          ) : (
            <>
              <div className="flex flex-wrap items-center gap-6 text-sm">
                <label className="flex items-center gap-2">
                  Worker slots
                  <span data-testid={`cfg-slots-${props.project.short_id}`}>
                    <NumberInput value={slotsValue} min={1} onChange={(v) => setSlots(Number(v))} />
                  </span>
                </label>
                <label className="flex items-center gap-2">
                  Review column
                  <span data-testid={`cfg-ov-${props.project.short_id}-reviewColumn`}>
                    <Select
                      value={dReview === "" ? "Inherit" : dReview}
                      onValueChange={(v) => setReviewColumn(v === "Inherit" ? "" : v)}
                      options={["Inherit", ...BOARD_REVIEW_COLUMNS]}
                    />
                  </span>
                  {prov("reviewColumn", pc?.overrides.reviewColumn !== undefined)}
                </label>
                <label className="flex items-center gap-2">
                  Auto-assign reviewer
                  <span data-testid={`cfg-ov-${props.project.short_id}-autoAssignReviewer`}>
                    <Select
                      value={dAuto === "" ? "Inherit" : dAuto === "on" ? "On" : "Off"}
                      onValueChange={(v) => setAutoAssign(v === "Inherit" ? "" : v === "On" ? "on" : "off")}
                      options={["Inherit", "On", "Off"]}
                    />
                  </span>
                  {prov("autoAssignReviewer", pc?.overrides.autoAssignReviewer !== undefined)}
                </label>
              </div>
              {pc && (
                <p className="text-xs text-muted-foreground" data-testid={`cfg-effective-${props.project.short_id}`}>
                  effective: {pc.effective.reviewColumn} · {pc.effective.autoAssignReviewer ? "on" : "off"} · slots{" "}
                  {pc.effective.defaultWorkerSlots}
                </p>
              )}
              <PersonaEditor
                tier="project"
                testPrefix={`cfg-personas-${props.project.short_id}`}
                draft={dPersonas}
                onDraft={setPersonas}
                // The legacy composition is what in_review runs on ONLY
                // when no explicit map exists anywhere above: a
                // user-tier explicit map (userInherited) owns the
                // stages outright and the composition arm is dead
                // regardless of the toggles.
                compositionActive={
                  userInherited === null &&
                  pc?.effective.reviewColumn === "auto" &&
                  (pc?.effective.autoAssignReviewer ?? false)
                }
                inherited={userInherited}
                roles={rolesCfg.data ?? []}
              />
              {error && (
                <p className="text-sm text-destructive" data-testid={`cfg-error-${props.project.short_id}`}>
                  {error}
                </p>
              )}
              <div className="flex justify-end">
                <Button
                  data-testid={`cfg-save-${props.project.short_id}`}
                  disabled={!dirty || slotsInvalid || save.isPending}
                  onClick={() => save.mutate()}
                >
                  Save
                </Button>
              </div>
            </>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
