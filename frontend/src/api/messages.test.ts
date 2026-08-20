import { describe, expect, it } from "vitest";
import { transformHistory } from "./messages";

describe("transformHistory", () => {
  it("extracts createdAt from contract message (ISO string)", () => {
    const iso = "2026-08-10T12:00:00.000Z";
    const raw = [
      {
        id: "msg_1",
        type: "user",
        createdAt: iso,
        parts: [{ type: "text", text: "hello" }],
      },
    ];
    const result = transformHistory(raw);
    expect(result).toHaveLength(1);
    expect(result[0]!.createdAt).toEqual(iso);
  });

  it("extracts modelID from model.id on assistant messages", () => {
    const raw = [
      {
        id: "msg_2",
        type: "assistant",
        createdAt: "2026-08-10T12:00:00.000Z",
        model: { id: "gpt-4o", provider: "openai" },
        parts: [{ type: "text", text: "hi there" }],
      },
    ];
    const result = transformHistory(raw);
    expect(result).toHaveLength(1);
    expect(result[0]!.modelID).toBe("gpt-4o");
  });

  it("omits modelID on user messages", () => {
    const raw = [
      {
        id: "msg_1",
        type: "user",
        createdAt: "2026-08-10T12:00:00.000Z",
        parts: [{ type: "text", text: "hello" }],
      },
    ];
    const result = transformHistory(raw);
    expect(result[0]!.modelID).toBeUndefined();
  });

  it("omits createdAt when absent", () => {
    const raw = [
      {
        id: "msg_1",
        type: "user",
        parts: [{ type: "text", text: "hello" }],
      },
    ];
    const result = transformHistory(raw);
    expect(result[0]!.createdAt).toBeUndefined();
  });

  it("omits modelID when model is absent", () => {
    const raw = [
      {
        id: "msg_2",
        type: "assistant",
        createdAt: "2026-08-10T12:00:00.000Z",
        parts: [{ type: "text", text: "response" }],
      },
    ];
    const result = transformHistory(raw);
    expect(result[0]!.modelID).toBeUndefined();
  });

  it("handles reasoning parts", () => {
    const raw = [
      {
        id: "msg_1",
        type: "assistant",
        createdAt: "2026-08-10T12:00:00.000Z",
        model: { id: "claude-3.5-sonnet" },
        parts: [{ type: "reasoning", reasoning: "step by step" }],
      },
    ];
    const result = transformHistory(raw);
    expect(result).toHaveLength(1);
    expect(result[0]!.parts[0]).toEqual({ type: "reasoning", text: "step by step" });
  });

  it("handles tool parts from contract shape", () => {
    const raw = [
      {
        id: "msg_1",
        type: "assistant",
        parts: [{
          type: "tool",
          tool: {
            name: "bash",
            input: { command: "ls -la" },
            output: "file.go",
            state: { status: "completed" },
          },
        }],
      },
    ];
    const result = transformHistory(raw);
    expect(result).toHaveLength(1);
    expect(result[0]!.parts[0]!.type).toBe("tool_use");
    expect(result[0]!.parts[0]!.text).toBe("bash");
    expect(result[0]!.parts[0]!.toolState).toBe("completed");
    expect(result[0]!.parts[0]!.input).toEqual({ command: "ls -la" });
    expect(result[0]!.parts[0]!.toolOutput).toBe("file.go");
  });

  it("preserves part id and tool callId — the reconnect boundary gate keys on them", () => {
    // ChatPage.historyPartIds (US-15.4) matches live SSE part.updated
    // events against history part identities. Dropping them makes the
    // gate a no-op and duplicates in-flight parts after an SSE reconnect.
    const raw = [
      {
        id: "msg_1",
        type: "assistant",
        parts: [{
          type: "tool",
          id: "prt_running",
          tool: {
            name: "bash",
            callId: "call_running",
            input: { command: "ls -la" },
            output: "file.go",
            state: { status: "running" },
          },
        }],
      },
    ];
    const result = transformHistory(raw);
    const part = result[0]!.parts[0]!;
    expect(part.id).toBe("prt_running");
    expect(part.toolCallId).toBe("call_running");
  });

  it("handles file_change parts", () => {
    const raw = [
      {
        id: "msg_1",
        type: "assistant",
        parts: [{
          type: "file_change",
          fileChange: { path: "foo.go", patch: "diff", status: "modified" },
        }],
      },
    ];
    const result = transformHistory(raw);
    expect(result).toHaveLength(1);
    expect(result[0]!.parts[0]!.type).toBe("file_change");
  });

  it("handles multiple messages with mixed metadata", () => {
    const raw = [
      {
        id: "msg_1",
        type: "user",
        createdAt: "2026-08-10T12:00:00.000Z",
        parts: [{ type: "text", text: "hi" }],
      },
      {
        id: "msg_2",
        type: "assistant",
        createdAt: "2026-08-10T12:00:05.000Z",
        model: { id: "gpt-4o" },
        parts: [{ type: "text", text: "hello" }],
      },
      {
        id: "msg_3",
        type: "user",
        parts: [{ type: "text", text: "bye" }],
      },
    ];
    const result = transformHistory(raw);
    expect(result).toHaveLength(3);
    expect(result[0]!.createdAt).toBe("2026-08-10T12:00:00.000Z");
    expect(result[0]!.modelID).toBeUndefined();
    expect(result[1]!.createdAt).toBe("2026-08-10T12:00:05.000Z");
    expect(result[1]!.modelID).toBe("gpt-4o");
    expect(result[2]!.createdAt).toBeUndefined();
    expect(result[2]!.modelID).toBeUndefined();
  });

  it("filters out messages with no displayable parts", () => {
    const raw = [
      {
        id: "msg_1",
        type: "assistant",
        parts: [{ type: "file_change", fileChange: {} }], // has parts but... wait, file_change IS displayable
      },
      {
        id: "msg_2",
        type: "assistant",
        parts: [], // empty parts
      },
    ];
    const result = transformHistory(raw);
    // msg_1 has file_change part (displayable), msg_2 has no parts (filtered)
    expect(result).toHaveLength(1);
    expect(result[0]!.id).toBe("msg_1");
  });
});

// Design 0050 D5 (#896): the elapsed badge's start time must survive
// history transformation — deleting the threading line in messages.ts
// fails these tests (review round 1: the line had zero coverage).
describe("transformHistory tool start time (#892 D5)", () => {
  it("threads state.startedAt (legacy ≤1.15.x ISO shape) to toolStartedAt", () => {
    const iso = "2026-08-15T21:35:25.000Z";
    const raw = [
      {
        id: "msg_t1",
        type: "assistant",
        createdAt: "2026-08-15T21:35:00.000Z",
        parts: [
          {
            type: "tool",
            tool: { name: "bash", state: { status: "running", startedAt: iso } },
          },
        ],
      },
    ];
    const result = transformHistory(raw as Parameters<typeof transformHistory>[0]);
    const part = result[0]!.parts[0]!;
    expect(part.type).toBe("tool_use");
    expect(part.toolStartedAt).toEqual(iso);
  });

  it("omits toolStartedAt when the tool state has none (older API payloads)", () => {
    const raw = [
      {
        id: "msg_t2",
        type: "assistant",
        createdAt: "2026-08-15T21:35:00.000Z",
        parts: [{ type: "tool", tool: { name: "bash", state: { status: "running" } } }],
      },
    ];
    const result = transformHistory(raw as Parameters<typeof transformHistory>[0]);
    expect(result[0]!.parts[0]!.toolStartedAt).toBeUndefined();
  });
});
