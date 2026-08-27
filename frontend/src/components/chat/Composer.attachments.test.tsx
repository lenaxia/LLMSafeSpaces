import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useReducer, useEffect } from "react";
import { render } from "../../test/utils";
import { Composer } from "./Composer";
import type { PendingAttachment } from "../../hooks/useComposerAttachments";

// Settings: reactive store mock — useUserSetting re-renders on writes just
// like the real useSyncExternalStore-backed module, so drawer toggles
// propagate without depending on the real storage/API machinery.
let mockSettings: Record<string, unknown> = {};
const settingsListeners = new Set<() => void>();
let settingsVersion = 0;
const setUserSettingMock = vi.fn((key: string, value: unknown) => {
  mockSettings[key] = value;
  settingsVersion++;
  settingsListeners.forEach((l) => l());
  return Promise.resolve();
});
vi.mock("../../hooks/useUserSettings", () => ({
  useUserSetting: vi.fn(<T,>(key: string, defaultValue: T): T => {
    const [, force] = useReducer((x: number) => x + 1, 0);
    useEffect(() => {
      settingsListeners.add(force);
      return () => {
        settingsListeners.delete(force);
      };
    }, [force]);
    void settingsVersion;
    return (mockSettings[key] as T) ?? defaultValue;
  }),
  setUserSetting: (key: string, value: unknown) => setUserSettingMock(key, value),
}));

// Selectors: stubbed — their own behavior is covered by their own tests.
// The stubs record props so U1.6.2 can assert the reused components receive
// the same workspaceId/orgId wiring the header row used to pass.
const modelSelectorProps: Array<Record<string, unknown>> = [];
const roleSelectorProps: Array<Record<string, unknown>> = [];
vi.mock("./ModelSelector", () => ({
  ModelSelector: (props: Record<string, unknown>) => {
    modelSelectorProps.push(props);
    return <div data-testid="model-selector-stub" />;
  },
}));
vi.mock("./RoleSelector", () => ({
  RoleSelector: (props: Record<string, unknown>) => {
    roleSelectorProps.push(props);
    return <div data-testid="role-selector-stub" />;
  },
}));

function setMobileMatchMedia(isMobile: boolean) {
  vi.spyOn(window, "matchMedia").mockImplementation((query) => {
    const isMinWidthQuery = query.includes("min-width");
    return {
      matches: isMinWidthQuery ? !isMobile : false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    } as unknown as MediaQueryList;
  });
}

function chip(partial: Partial<PendingAttachment> & { id: string }): PendingAttachment {
  return { name: partial.id, size: 1024, status: "attached", ...partial };
}

describe("Composer drawer (D12)", () => {
  beforeEach(() => {
    mockSettings = {};
    setUserSettingMock.mockClear();
    modelSelectorProps.length = 0;
    roleSelectorProps.length = 0;
    setMobileMatchMedia(false);
  });
  afterEach(() => vi.restoreAllMocks());

  it("desktop defaults the drawer open; chevron collapses it and persists (U1.6.1)", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" orgId="org-1" />);
    expect(screen.getByTestId("model-selector-stub")).toBeInTheDocument();
    expect(screen.getByTestId("role-selector-stub")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Toggle composer options" }));
    expect(screen.queryByTestId("model-selector-stub")).not.toBeInTheDocument();
    expect(setUserSettingMock).toHaveBeenCalledWith("composerDrawerOpen", "collapsed");
  });

  it("re-opening persists 'open'", async () => {
    mockSettings = { composerDrawerOpen: "collapsed" };
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" />);
    expect(screen.queryByTestId("model-selector-stub")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Toggle composer options" }));
    expect(screen.getByTestId("model-selector-stub")).toBeInTheDocument();
    expect(setUserSettingMock).toHaveBeenCalledWith("composerDrawerOpen", "open");
  });

  it("mobile defaults collapsed (media-query-aware default, U1.6.1)", () => {
    setMobileMatchMedia(true);
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" />);
    expect(screen.queryByTestId("model-selector-stub")).not.toBeInTheDocument();
  });

  it("an explicit 'open' preference wins over the mobile default", () => {
    setMobileMatchMedia(true);
    mockSettings = { composerDrawerOpen: "open" };
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" />);
    expect(screen.getByTestId("model-selector-stub")).toBeInTheDocument();
  });

  it("selectors receive the workspaceId/orgId wiring the header row passed (U1.6.2)", () => {
    render(<Composer onSend={vi.fn()} workspaceId="ws-9" orgId="org-9" />);
    expect(modelSelectorProps[0]).toMatchObject({ workspaceId: "ws-9" });
    expect(roleSelectorProps[0]).toMatchObject({ workspaceId: "ws-9", orgId: "org-9" });
  });

  it("chevron carries aria-expanded and is keyboard operable (U1.6.20)", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" />);
    const chevron = screen.getByRole("button", { name: "Toggle composer options" });
    expect(chevron).toHaveAttribute("aria-expanded", "true");
    chevron.focus();
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Toggle composer options" })).toHaveAttribute("aria-expanded", "false"),
    );
    await user.keyboard(" ");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Toggle composer options" })).toHaveAttribute("aria-expanded", "true"),
    );
  });

  it("drawer state is read from the shared user setting (not per workspace/session, U1.6.12)", () => {
    mockSettings = { composerDrawerOpen: "collapsed" };
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" />);
    expect(screen.queryByTestId("model-selector-stub")).not.toBeInTheDocument();
  });

  it("toggles without clobbering textarea content (U1.6.21)", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "draft survives");
    await user.click(screen.getByRole("button", { name: "Toggle composer options" }));
    await user.click(screen.getByRole("button", { name: "Toggle composer options" }));
    expect(textarea).toHaveValue("draft survives");
  });
});

describe("Composer attach button (D12/U1.6.3)", () => {
  beforeEach(() => {
    mockSettings = {};
    setMobileMatchMedia(false);
  });
  afterEach(() => vi.restoreAllMocks());

  it("is always visible in the composer row and opens the native picker", () => {
    const onAddFiles = vi.fn();
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" onAddFiles={onAddFiles} />);
    const input = screen.getByTestId("composer-file-input") as HTMLInputElement;
    const clickSpy = vi.spyOn(input, "click");
    fireEvent.click(screen.getByRole("button", { name: "Attach files" }));
    expect(clickSpy).toHaveBeenCalled();
  });

  it("picker selection calls onAddFiles once with the files (U1.6.3)", () => {
    const onAddFiles = vi.fn();
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" onAddFiles={onAddFiles} />);
    const input = screen.getByTestId("composer-file-input");
    const f1 = new File(["a"], "notes.txt");
    fireEvent.change(input, { target: { files: [f1] } });
    expect(onAddFiles).toHaveBeenCalledTimes(1);
    expect(onAddFiles).toHaveBeenCalledWith([f1]);
  });

  it("multi-select passes every picked file (U1.6.15)", () => {
    const onAddFiles = vi.fn();
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" onAddFiles={onAddFiles} />);
    const files = Array.from({ length: 5 }, (_, i) => new File(["x"], `m${i}.txt`));
    fireEvent.change(screen.getByTestId("composer-file-input"), { target: { files } });
    expect(onAddFiles).toHaveBeenCalledWith(files);
  });

  it("picker cancel (no selection) is a no-op (U1.6.14)", () => {
    const onAddFiles = vi.fn();
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" onAddFiles={onAddFiles} />);
    fireEvent.change(screen.getByTestId("composer-file-input"), { target: { files: [] } });
    expect(onAddFiles).not.toHaveBeenCalled();
  });

  it("the same file can be re-picked (input value resets, U1.6.16 wiring)", () => {
    const onAddFiles = vi.fn();
    render(<Composer onSend={vi.fn()} workspaceId="ws-1" onAddFiles={onAddFiles} />);
    const input = screen.getByTestId("composer-file-input") as HTMLInputElement;
    const dup = new File(["a"], "dup.txt");
    fireEvent.change(input, { target: { files: [dup] } });
    expect(input.value).toBe("");
    fireEvent.change(input, { target: { files: [dup] } });
    expect(onAddFiles).toHaveBeenCalledTimes(2);
  });

  it("renders chips between the drawer row and the textarea with name + human size (U1.6.3)", () => {
    render(
      <Composer
        onSend={vi.fn()}
        attachments={[chip({ id: "c1", name: "notes.txt", size: 2048 })]}
      />,
    );
    expect(screen.getByText("notes.txt")).toBeInTheDocument();
    expect(screen.getByText("2 KB")).toBeInTheDocument();
  });
});

describe("Composer chips (D17)", () => {
  beforeEach(() => {
    mockSettings = {};
    setMobileMatchMedia(false);
  });
  afterEach(() => vi.restoreAllMocks());

  it("attached chip is removable by mouse and keyboard (U1.6.20)", async () => {
    const user = userEvent.setup();
    const onRemove = vi.fn();
    render(
      <Composer
        onSend={vi.fn()}
        attachments={[chip({ id: "c1", name: "notes.txt" })]}
        onRemoveAttachment={onRemove}
      />,
    );
    await user.click(screen.getByRole("button", { name: 'Remove attachment notes.txt' }));
    expect(onRemove).toHaveBeenCalledWith("c1");

    render(
      <Composer
        onSend={vi.fn()}
        attachments={[chip({ id: "c2", name: "b.txt" })]}
        onRemoveAttachment={onRemove}
      />,
    );
    const btn = screen.getByRole("button", { name: "Remove attachment b.txt" });
    btn.focus();
    await user.keyboard("{Enter}");
    expect(onRemove).toHaveBeenCalledWith("c2");
  });

  it("error chip shows the error and offers retry + remove (U1.6.4)", async () => {
    const user = userEvent.setup();
    const onRetry = vi.fn();
    const onRemove = vi.fn();
    render(
      <Composer
        onSend={vi.fn()}
        attachments={[chip({ id: "e1", name: "bad.txt", status: "error", error: "507 insufficient storage" })]}
        onRetryAttachment={onRetry}
        onRemoveAttachment={onRemove}
      />,
    );
    const chipEl = screen.getByTestId("composer-chip-e1");
    expect(chipEl).toHaveTextContent(/507 insufficient storage/);
    await user.click(within(chipEl).getByRole("button", { name: "Retry upload bad.txt" }));
    expect(onRetry).toHaveBeenCalledWith("e1");
    await user.click(screen.getByRole("button", { name: "Remove attachment bad.txt" }));
    expect(onRemove).toHaveBeenCalledWith("e1");
  });

  it("surfaces an upload-failure toast when a chip errors (U1.6.4)", () => {
    render(
      <Composer
        onSend={vi.fn()}
        attachments={[chip({ id: "e9", name: "bad.txt", status: "error", error: "507 insufficient storage" })]}
      />,
    );
    expect(screen.getByRole("status", { name: "upload-error-notice" })).toHaveTextContent(/bad\.txt/);
    expect(screen.getByRole("status", { name: "upload-error-notice" })).toHaveTextContent(/507 insufficient storage/);
  });

  it("send is disabled while any chip is uploading; uploading chip is visually distinct (U1.6.13)", async () => {
    const onSend = vi.fn();
    const user = userEvent.setup();
    render(
      <Composer
        onSend={onSend}
        attachments={[chip({ id: "u1", name: "slow.bin", status: "uploading" })]}
      />,
    );
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "ready to send");
    const sendBtn = screen.getByRole("button", { name: "Send message" });
    expect(sendBtn).toBeDisabled();
    await user.click(sendBtn);
    expect(onSend).not.toHaveBeenCalled();
    expect(screen.getByTestId("composer-chip-u1")).toHaveAttribute("data-status", "uploading");
  });

  it("send enabled again once uploads settle; failed chip does NOT block send", async () => {
    const onSend = vi.fn();
    const user = userEvent.setup();
    render(
      <Composer
        onSend={onSend}
        attachments={[chip({ id: "e1", name: "bad.txt", status: "error", error: "x" })]}
      />,
    );
    await user.type(screen.getByPlaceholderText("Type a message..."), "sending anyway");
    expect(screen.getByRole("button", { name: "Send message" })).not.toBeDisabled();
  });

  it("send carries the settled paths and the exact, unmutated text (U1.6.7)", async () => {
    const onSend = vi.fn();
    const user = userEvent.setup();
    render(
      <Composer
        onSend={onSend}
        attachments={[
          chip({ id: "a1", name: "a.txt", path: "/workspace/uploads/11111111-2222-3333-4444-555555555555-a.txt" }),
          chip({ id: "e1", name: "e.txt", status: "error" }),
        ]}
      />,
    );
    await user.type(screen.getByPlaceholderText("Type a message..."), "  exact text  ");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    expect(onSend).toHaveBeenCalledWith("exact text", ["/workspace/uploads/11111111-2222-3333-4444-555555555555-a.txt"]);
  });

  it("send with zero chips calls onSend with an empty files array (U1.6.5)", async () => {
    const onSend = vi.fn();
    const user = userEvent.setup();
    render(<Composer onSend={onSend} attachments={[]} />);
    await user.type(screen.getByPlaceholderText("Type a message..."), "no files");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    expect(onSend).toHaveBeenCalledWith("no files", []);
  });

  it("chip changes do not clobber textarea content (U1.6.21)", async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <Composer onSend={vi.fn()} attachments={[chip({ id: "a1", name: "a.txt" })]} />,
    );
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "keep me");
    rerender(
      <Composer
        onSend={vi.fn()}
        attachments={[chip({ id: "a1", name: "a.txt" }), chip({ id: "a2", name: "b.txt" })]}
      />,
    );
    rerender(<Composer onSend={vi.fn()} attachments={[]} />);
    expect(textarea).toHaveValue("keep me");
  });

  it("shows a dismissible cap-violation toast (U1.6.6)", async () => {
    const user = userEvent.setup();
    const onDismiss = vi.fn();
    render(<Composer onSend={vi.fn()} capViolation onDismissCapViolation={onDismiss} />);
    expect(screen.getByText(/up to 10 files/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Dismiss attachment notice" }));
    expect(onDismiss).toHaveBeenCalled();
  });
});
