import { describe, expect, it, beforeEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { _resetSessionDraftStoreForTests, loadSessionDraft } from "../lib/sessionDrafts";
import { useSessionDraft } from "./useSessionDraft";

// The hook's pair surfaced through a controlled textarea (module-level
// reassignment would trip react-hooks/globals).
function Harness({ ws, sid }: { ws?: string; sid?: string }) {
  const [text, setText] = useSessionDraft(ws, sid);
  return (
    <input
      data-testid="draft"
      value={text}
      onChange={(e) => setText(e.target.value)}
    />
  );
}

const box = () => screen.getByTestId("draft") as HTMLInputElement;

function edit(value: string) {
  act(() => {
    fireEvent.change(box(), { target: { value } });
  });
}

describe("useSessionDraft", () => {
  beforeEach(() => {
    localStorage.clear();
    _resetSessionDraftStoreForTests();
  });

  it("mounts with the session's stored draft (the restore path)", () => {
    localStorage.setItem(
      "llmsafespaces_session_drafts",
      JSON.stringify({ "ws-1": { "sess-1": { text: "typed earlier", updatedAt: Date.now() } } }),
    );
    render(<Harness ws="ws-1" sid="sess-1" />);
    expect(box().value).toBe("typed earlier");
  });

  it("write-through: an edit persists; clearing to \"\" consumes the draft", () => {
    const r = render(<Harness ws="ws-1" sid="sess-1" />);
    edit("a drafted reply");
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("a drafted reply");
    edit("");
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("");
    r.unmount();
  });

  it("session switch WITHOUT remount: loads the incoming session's draft and never cross-saves", () => {
    localStorage.setItem(
      "llmsafespaces_session_drafts",
      JSON.stringify({
        "ws-1": {
          "sess-1": { text: "draft for one", updatedAt: Date.now() },
          "sess-2": { text: "draft for two", updatedAt: Date.now() },
        },
      }),
    );
    const r = render(<Harness ws="ws-1" sid="sess-1" />);
    expect(box().value).toBe("draft for one");

    // Switch props (same component — the defensive no-remount path).
    r.rerender(<Harness ws="ws-1" sid="sess-2" />);
    expect(box().value).toBe("draft for two");
    // The owner guard held: sess-1's draft was NOT overwritten by the
    // switch, and sess-2's draft was not clobbered by sess-1's text.
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("draft for one");
    expect(loadSessionDraft("ws-1", "sess-2")).toBe("draft for two");
    r.unmount();
  });

  it("typing in the incoming session saves under the incoming key only", () => {
    const r = render(<Harness ws="ws-1" sid="sess-1" />);
    edit("belongs to one");
    r.rerender(<Harness ws="ws-1" sid="sess-2" />);
    edit("belongs to two");
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("belongs to one");
    expect(loadSessionDraft("ws-1", "sess-2")).toBe("belongs to two");
    r.unmount();
  });

  it("workspace switch is atomic with the session switch (no ws-crossed write)", () => {
    const r = render(<Harness ws="ws-1" sid="sess-1" />);
    edit("ws-1 draft");
    r.rerender(<Harness ws="ws-2" sid="sess-1" />);
    edit("ws-2 draft");
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("ws-1 draft");
    expect(loadSessionDraft("ws-2", "sess-1")).toBe("ws-2 draft");
    r.unmount();
  });

  it("no session: plain state, nothing persisted", () => {
    const r = render(<Harness />);
    edit("unkeyed");
    expect(box().value).toBe("unkeyed");
    expect(localStorage.getItem("llmsafespaces_session_drafts")).toBe(null);
    // Gaining a session hydrates its (empty) draft.
    r.rerender(<Harness ws="ws-1" sid="sess-new" />);
    expect(box().value).toBe("");
    r.unmount();
  });
});
