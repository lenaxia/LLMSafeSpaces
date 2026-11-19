// entityLinks.test.ts — born-red pins for the conservative entity
// auto-linker (tsk-vibrant-cadet-pearls): candidate vocabulary,
// guarded regions (code/URLs/link syntax), resolution gating (no
// pattern-only links), idempotency, and the batched lazy resolver's
// caching posture.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../api/board", () => ({
  boardApi: {
    getTask: vi.fn().mockRejectedValue(new Error("no net")),
    getEpic: vi.fn().mockRejectedValue(new Error("no net")),
    getProject: vi.fn().mockRejectedValue(new Error("no net")),
  },
}));
vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getSession: vi.fn().mockRejectedValue(new Error("no net")),
  },
}));

import { boardApi } from "../api/board";
import {
  entityAriaLabel,
  entityHref,
  findEntityCandidates,
  getEntityRegistryVersion,
  linkifyMarkdown,
  lookupEntity,
  publishEntities,
  requestEntityResolution,
  resetEntityLinkState,
  setEntityResolveDelayForTests,
  setSessionWorkspaceScope,
  type EntityRef,
  type ResolveEntity,
} from "./entityLinks";
import { learnSessionAlias, resetSessionAliases } from "../session/sessionAliases";

const mockedGetTask = vi.mocked(boardApi.getTask);

function getVersion(): number {
  return getEntityRegistryVersion();
}

function taskRef(id: string, title?: string): EntityRef {
  return { kind: "task", key: `task:${id}`, id, title };
}

describe("findEntityCandidates", () => {
  it("matches board short ids, raw ses ids, and 3-word phrases in prose", () => {
    const text =
      "Story tsk-yeasty-magnet-scores and epic epc-quiet-harbor-9 plus prj-alpha-1; session merry-finch-loses and raw ses_Ab12Cd here.";
    const kinds = Object.fromEntries(
      findEntityCandidates(text).map((c) => [c.raw, c.kind]),
    );
    expect(kinds["tsk-yeasty-magnet-scores"]).toBe("task");
    expect(kinds["epc-quiet-harbor-9"]).toBe("epic");
    expect(kinds["prj-alpha-1"]).toBe("project");
    expect(kinds["merry-finch-loses"]).toBe("session");
    expect(kinds["ses_Ab12Cd"]).toBe("session");
  });

  it("never matches inside code spans or fenced code blocks", () => {
    const text = [
      "see `tsk-yeasty-magnet-scores` inline",
      "```",
      "epc-quiet-harbor-9 in a fence",
      "```",
      "prj-alpha-1 after",
    ].join("\n");
    const raws = findEntityCandidates(text).map((c) => c.raw);
    expect(raws).not.toContain("tsk-yeasty-magnet-scores");
    expect(raws).not.toContain("epc-quiet-harbor-9");
    expect(raws).toContain("prj-alpha-1");
  });

  it("never matches inside URLs, autolinks, or markdown link syntax", () => {
    const text = [
      "https://example.com/tsk-yeasty-magnet-scores stays flat,",
      "<https://example.com/merry-finch-loses> too,",
      "and [see tsk-yeasty-magnet-scores](https://example.com/x) also.",
    ].join(" ");
    expect(findEntityCandidates(text)).toHaveLength(0);
  });

  it("rejects kebab fragments: 4-word kebabs, ids inside longer tokens, domain-ish text", () => {
    const text = "a-b-c-d and footsk-yeasty-magnet-scores and example.com-foo-bar-baz end";
    expect(findEntityCandidates(text)).toHaveLength(0);
  });

  it("allows sentence punctuation around a candidate", () => {
    const raws = findEntityCandidates("Done in tsk-yeasty-magnet-scores. (nice)").map((c) => c.raw);
    expect(raws).toContain("tsk-yeasty-magnet-scores");
  });
});

describe("linkifyMarkdown", () => {
  const resolveKnown: ResolveEntity = (raw, kind) =>
    raw === "tsk-yeasty-magnet-scores" && kind === "task"
      ? taskRef(raw, "Fix yeasty magnet scoring")
      : undefined;

  it("links ONLY resolved candidates; unresolved stay verbatim", () => {
    const out = linkifyMarkdown(
      "tracked in tsk-yeasty-magnet-scores and merry-finch-loses today",
      resolveKnown,
    );
    expect(out).toContain("[tsk-yeasty-magnet-scores](/board?story=tsk-yeasty-magnet-scores");
    expect(out).toContain("Board story tsk-yeasty-magnet-scores — Fix yeasty magnet scoring");
    expect(out).toContain("and merry-finch-loses today");
  });

  it("is idempotent: its own output re-transforms to itself", () => {
    const once = linkifyMarkdown("see tsk-yeasty-magnet-scores now", resolveKnown);
    const twice = linkifyMarkdown(once, resolveKnown);
    expect(twice).toBe(once);
  });

  it("leaves text with no candidates untouched", () => {
    expect(linkifyMarkdown("plain prose, nothing to link", resolveKnown)).toBe(
      "plain prose, nothing to link",
    );
  });

  it("escapes quotes in entity titles", () => {
    const ref = taskRef("tsk-x-y-z", 'the "quoted" title');
    const out = linkifyMarkdown("tsk-x-y-z", (raw) => (raw === "tsk-x-y-z" ? ref : undefined));
    expect(out).not.toContain('"quoted" title")');
    expect(out).toContain('\\"quoted\\" title');
  });

  it("never linkifies code spans even when the entity is resolved", () => {
    const out = linkifyMarkdown("run `tsk-yeasty-magnet-scores` now", resolveKnown);
    expect(out).not.toContain("[tsk-yeasty-magnet-scores](");
    expect(out).toContain("`tsk-yeasty-magnet-scores`");
  });
});

describe("entity registry", () => {
  beforeEach(() => {
    resetEntityLinkState();
    resetSessionAliases();
    vi.clearAllMocks();
  });
  afterEach(() => {
    resetEntityLinkState();
    resetSessionAliases();
  });

  it("publishing is idempotent — version only moves on real change", () => {
    publishEntities([taskRef("tsk-a-b-c", "T1")]);
    const v1 = getVersion();
    publishEntities([taskRef("tsk-a-b-c", "T1")]);
    expect(getVersion()).toBe(v1);
    publishEntities([taskRef("tsk-a-b-c", "T1-renamed")]);
    expect(getVersion()).toBe(v1 + 1);
  });

  it("lookup resolves ses_* raw ids through learned aliases", () => {
    learnSessionAlias("ws-1", "merry-finch-loses", "ses_raw123");
    publishEntities([
      { kind: "session", key: "session:merry-finch-loses", id: "merry-finch-loses", title: "S", workspaceId: "ws-1" },
    ]);
    // The alias table alone is not a loaded index — publish the scope.
    setSessionWorkspaceScope(["ws-1"]);
    expect(lookupEntity("ses_raw123", "session")?.id).toBe("merry-finch-loses");
  });

  it("session hrefs require a workspace; board hrefs mint deep-link params", () => {
    expect(entityHref({ kind: "session", key: "k", id: "merry-finch-loses" })).toBe("");
    expect(
      entityHref({ kind: "session", key: "k", id: "merry-finch-loses", workspaceId: "ws 1" }),
    ).toBe("/chat/ws%201/merry-finch-loses");
    expect(entityHref(taskRef("tsk-a-b-c"))).toBe("/board?story=tsk-a-b-c");
    expect(entityHref({ kind: "epic", key: "k", id: "epc-1", title: "E" })).toBe("/board?epic=epc-1");
    expect(entityHref({ kind: "project", key: "k", id: "prj-1" })).toBe("/board?project=prj-1");
    expect(entityAriaLabel(taskRef("tsk-a-b-c", "T"))).toBe("Board story tsk-a-b-c — T");
  });
});

describe("lazy resolver", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetEntityLinkState();
    resetSessionAliases();
    vi.clearAllMocks();
    setEntityResolveDelayForTests(10);
  });
  afterEach(() => {
    vi.useRealTimers();
    resetEntityLinkState();
    resetSessionAliases();
    setEntityResolveDelayForTests(250);
  });

  it("probes unknown candidates batched, caches results, and reuses the cache", async () => {
    mockedGetTask.mockResolvedValue({
      id: "uuid-1",
      short_id: "tsk-unknown-story",
      title: "A title",
    } as Awaited<ReturnType<typeof boardApi.getTask>>);

    linkifyMarkdown("x tsk-unknown-story y", () => undefined, (c) => {
      requestEntityResolution([{ raw: c.raw, kind: c.kind }]);
    });
    expect(mockedGetTask).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(15);
    // Resolved: a second linkify pass finds it in the registry.
    const out = linkifyMarkdown("x tsk-unknown-story y", (raw, kind) => lookupEntity(raw, kind));
    expect(out).toContain("/board?story=tsk-unknown-story");

    // Same candidate again: served from the registry — no new probe.
    linkifyMarkdown("again tsk-unknown-story", (raw, kind) => lookupEntity(raw, kind), (c) => {
      requestEntityResolution([{ raw: c.raw, kind: c.kind }]);
    });
    await vi.advanceTimersByTimeAsync(30);
    expect(mockedGetTask).toHaveBeenCalledTimes(1);
  });

  it("caches misses (rejected probes) so unresolved text never re-probes within the TTL", async () => {
    mockedGetTask.mockRejectedValue(new Error("404"));
    for (let i = 0; i < 3; i++) {
      linkifyMarkdown("nope tsk-missing-story", () => undefined, (c) => {
        requestEntityResolution([{ raw: c.raw, kind: c.kind }]);
      });
    }
    await vi.advanceTimersByTimeAsync(50);
    expect(mockedGetTask).toHaveBeenCalledTimes(1);
    // And the text stays plain.
    const out = linkifyMarkdown("nope tsk-missing-story", (raw, kind) => lookupEntity(raw, kind));
    expect(out).toBe("nope tsk-missing-story");
  });
});
