import { useEffect, useRef, useState } from "react";
import type { KeyboardEvent } from "react";
import { ChevronDown, ChevronRight, FileText, Loader2, Paperclip, RotateCcw, Send, Square, X } from "lucide-react";
import { Button } from "../ui/Button";
import { Tooltip } from "../ui/Tooltip";
import { cn } from "../../lib/utils";
import { setUserSetting, useUserSetting } from "../../hooks/useUserSettings";
import { useIsMobile } from "../../hooks/useMediaQuery";
import { getCursorLineInfo } from "../../lib/composerHistory";
import { formatBytes } from "../../lib/format";
import { ModelSelector } from "./ModelSelector";
import { RoleSelector } from "./RoleSelector";
import type { PendingAttachment } from "../../hooks/useComposerAttachments";

interface Props {
  onSend: (text: string, files: string[]) => void;
  onAbort?: () => void;
  disabled?: boolean;
  streaming?: boolean;
  placeholder?: string;
  /**
   * Newest-first list of the user's previous message texts in the
   * current session, used for Up/Arrow history navigation on desktop.
   * Empty array (or omitted) disables navigation. Mobile never
   * navigates history regardless of this prop.
   */
  userMessageHistory?: string[];
  /** Workspace scope for the attach button, uploads, and drawer selectors. */
  workspaceId?: string;
  orgId?: string;
  /** Workspace-scoped pending attachments (chips) rendered above the textarea. */
  attachments?: PendingAttachment[];
  capViolation?: boolean;
  onAddFiles?: (files: File[]) => void;
  onRemoveAttachment?: (id: string) => void;
  onRetryAttachment?: (id: string) => void;
  onDismissCapViolation?: () => void;
}

/** Sentinel: no pending cursor move. */
const NO_PENDING_CURSOR = -1;

const CHIP_BASE = "inline-flex max-w-full items-center gap-1 rounded-md border px-2 py-1 text-xs min-h-[32px]";
const CHIP_STYLES: Record<PendingAttachment["status"], string> = {
  uploading: "border-border bg-muted/60 text-muted-foreground",
  attached: "border-border bg-muted text-foreground",
  error: "border-destructive/50 bg-destructive/10 text-destructive",
};

function ComposerChip({ chip, onRemove, onRetry }: {
  chip: PendingAttachment;
  onRemove?: (id: string) => void;
  onRetry?: (id: string) => void;
}) {
  return (
    <span
      data-testid={`composer-chip-${chip.id}`}
      data-status={chip.status}
      className={cn(CHIP_BASE, CHIP_STYLES[chip.status], chip.status === "uploading" && "animate-pulse")}
    >
      {chip.status === "uploading" ? (
        <Loader2 className="h-3 w-3 shrink-0 animate-spin" aria-hidden="true" />
      ) : (
        <FileText className="h-3 w-3 shrink-0" aria-hidden="true" />
      )}
      <span className="max-w-[160px] truncate font-medium" title={chip.name}>{chip.name}</span>
      <span className="shrink-0 text-muted-foreground">{formatBytes(chip.size)}</span>
      {chip.status === "error" && chip.error && (
        <span className="max-w-[160px] truncate" title={chip.error}>{chip.error}</span>
      )}
      {chip.status === "error" && (
        <button
          type="button"
          aria-label={`Retry upload ${chip.name}`}
          onClick={() => onRetry?.(chip.id)}
          className="ml-0.5 shrink-0 rounded p-0.5 hover:bg-destructive/20"
        >
          <RotateCcw className="h-3 w-3" aria-hidden="true" />
        </button>
      )}
      <button
        type="button"
        aria-label={`Remove attachment ${chip.name}`}
        onClick={() => onRemove?.(chip.id)}
        className="ml-0.5 shrink-0 rounded p-0.5 hover:bg-muted-foreground/20"
      >
        <X className="h-3 w-3" aria-hidden="true" />
      </button>
    </span>
  );
}

/**
 * Composer — the chat input box.
 *
 * Send-key behavior:
 *   - Desktop, default mode (sendOnEnter=false):
 *       Enter = newline, Ctrl/Cmd+Enter = send, Shift+Enter = newline
 *   - Desktop, legacy mode (sendOnEnter=true):
 *       Enter = send, Shift+Enter = newline, Ctrl/Cmd+Enter = send
 *   - Mobile: Enter = newline; only the send button sends. The
 *       sendOnEnter setting is ignored on mobile because mobile
 *       keyboards do not reliably produce modifier keys.
 *
 * All key paths are guarded against IME composition: while a CJK IME
 * is mid-composition (isComposing=true or keyCode===229), Enter and
 * Ctrl/Cmd+Enter are allowed to finalize the candidate rather than
 * send the message.
 *
 * History navigation (desktop only): Up on the first line walks back
 * through `userMessageHistory` (newest-first); Down on the last line
 * walks forward. The pre-browse draft is restored when navigating
 * Down past the newest entry. Mirrors the opencode TUI semantics
 * (packages/tui/src/prompt/history.tsx + component/prompt/index.tsx),
 * adapted to a DOM textarea via getCursorLineInfo.
 *
 * Attachments (Epic 67): the "+" button is always visible in the input
 * row; chips render between the options drawer and the textarea; send
 * is blocked while any chip is uploading (D17) and carries only settled
 * paths — the text is never mutated client-side (D11). The options
 * drawer (D12) holds the model + persona selectors; its open state is
 * the persisted user preference `composerDrawerOpen` ("auto" default
 * expands on desktop and collapses on mobile).
 */
export function Composer({
  onSend,
  onAbort,
  disabled,
  streaming,
  placeholder = "Type a message...",
  userMessageHistory = [],
  workspaceId,
  orgId,
  attachments = [],
  capViolation = false,
  onAddFiles,
  onRemoveAttachment,
  onRetryAttachment,
  onDismissCapViolation,
}: Props) {
  const [text, setText] = useState("");
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const sendOnEnter = useUserSetting("sendOnEnter", false);
  const isMobile = useIsMobile();

  // Upload-failure toast (U1.6.4): fires once per failed chip (tracked by
  // id so retry→re-fail re-toasts), auto-dismisses after 4s.
  const [uploadErrorNotice, setUploadErrorNotice] = useState<string | null>(null);
  const toastedErrorIdsRef = useRef(new Set<string>());
  useEffect(() => {
    for (const chip of attachments) {
      if (chip.status === "error" && !toastedErrorIdsRef.current.has(chip.id)) {
        toastedErrorIdsRef.current.add(chip.id);
        setUploadErrorNotice(
          chip.error ? `Upload failed: ${chip.name} — ${chip.error}` : `Upload failed: ${chip.name}`,
        );
      }
    }
  }, [attachments]);
  useEffect(() => {
    if (!uploadErrorNotice) return;
    const t = setTimeout(() => setUploadErrorNotice(null), 4000);
    return () => clearTimeout(t);
  }, [uploadErrorNotice]);

  const drawerPref = useUserSetting<string>("composerDrawerOpen", "auto");
  const drawerOpen = drawerPref === "auto" ? !isMobile : drawerPref === "open";
  const toggleDrawer = () => {
    void setUserSetting("composerDrawerOpen", drawerOpen ? "collapsed" : "open").catch(() => {});
  };

  const anyUploading = attachments.some((a) => a.status === "uploading");

  // History-browsing state. historyCursor === -1 means "not browsing".
  // 0..N-1 indexes into userMessageHistory (already newest-first).
  // savedDraft snapshots the textarea content at the moment the user
  // first pressed Up from a non-empty draft, so Down-past-newest can
  // restore it. historyCursor/savedDraft are reset together whenever
  // the userMessageHistory reference changes (e.g. on history refetch)
  // to avoid stranding the cursor at a stale index (F4 fix).
  const [historyCursor, setHistoryCursor] = useState(-1);
  const [savedDraft, setSavedDraft] = useState<string | null>(null);

  // After loading a history entry, the cursor must move to start (Up)
  // or end (Down) of the new text so the next Up/Down correctly detects
  // the line boundary. We use a ref + useEffect instead of queueMicrotask
  // because useEffect is guaranteed to fire after the DOM update and
  // before the next event handler — queueMicrotask is not.
  //
  // navTick is bumped on every navigation. The effect depends on it (not
  // just `text`) so it fires even when the loaded entry happens to equal
  // the current draft — React would otherwise bail out of the
  // identical-primitive setText and the cursor wouldn't move.
  const pendingCursor = useRef(NO_PENDING_CURSOR);
  const [navTick, setNavTick] = useState(0);

  useEffect(() => {
    if (pendingCursor.current === NO_PENDING_CURSOR) return;
    const el = textareaRef.current;
    if (!el) return;
    const pos = pendingCursor.current;
    pendingCursor.current = NO_PENDING_CURSOR;
    el.selectionStart = pos;
    el.selectionEnd = pos;
  }, [navTick, text]);

  useEffect(() => {
    setHistoryCursor(-1);
    setSavedDraft(null);
  }, [userMessageHistory]);

  const handleSubmit = (e?: { preventDefault: () => void }) => {
    e?.preventDefault();
    const trimmed = text.trim();
    if (!trimmed || disabled || anyUploading) return;
    onSend(trimmed, attachments.filter((a) => a.status === "attached" && a.path).map((a) => a.path!));
    setText("");
    setHistoryCursor(-1);
    setSavedDraft(null);
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  };

  const isIMEComposing = (e: KeyboardEvent): boolean =>
    e.nativeEvent.isComposing || e.keyCode === 229;

  const navigateHistory = (direction: "up" | "down"): boolean => {
    // Returns true if the event was handled (caller should preventDefault).
    const el = textareaRef.current;
    if (!el) return false;
    if (isMobile) return false;
    if (userMessageHistory.length === 0) return false;

    const info = getCursorLineInfo(el.value, el.selectionStart);

    if (direction === "up") {
      if (!info.onFirstLine) return false;
      // Entering browse mode: snapshot the draft if non-empty.
      if (historyCursor === -1 && text !== "") {
        setSavedDraft(text);
      }
      const next = Math.min(historyCursor + 1, userMessageHistory.length - 1);
      const entry = userMessageHistory[next];
      if (entry === undefined) return false; // defensive; length check above guarantees this
      // Reload the entry even if already at the oldest index — this
      // discards any edits the user made to the loaded text (F5 fix:
      // edits don't strand the snapshot; Up always reloads the original).
      setHistoryCursor(next);
      setText(entry);
      pendingCursor.current = 0;
      setNavTick((t) => t + 1);
      return true;
    } else {
      // Down: only acts when already browsing.
      if (historyCursor === -1) return false;
      if (!info.onLastLine) return false;
      if (historyCursor > 0) {
        const next = historyCursor - 1;
        const entry = userMessageHistory[next];
        if (entry === undefined) return false;
        setHistoryCursor(next);
        setText(entry);
        pendingCursor.current = entry.length;
        setNavTick((t) => t + 1);
        return true;
      }
      // historyCursor === 0: navigate forward past newest → restore draft.
      const restored = savedDraft ?? "";
      setHistoryCursor(-1);
      setText(restored);
      setSavedDraft(null);
      pendingCursor.current = restored.length;
      setNavTick((t) => t + 1);
      return true;
    }
  };

  const handleKeyDown = (e: KeyboardEvent) => {
    if (isIMEComposing(e)) return;

    // History navigation (desktop only). Evaluated before send logic so
    // that ArrowUp/ArrowDown never collide with submit.
    if (e.key === "ArrowUp") {
      if (navigateHistory("up")) {
        e.preventDefault();
      }
      return;
    }
    if (e.key === "ArrowDown") {
      if (navigateHistory("down")) {
        e.preventDefault();
      }
      return;
    }

    if (e.key !== "Enter") return;

    if (isMobile) {
      // Mobile: Enter always adds a newline; never sends.
      return;
    }

    if (sendOnEnter) {
      // Legacy desktop mode: Enter sends, Shift+Enter is newline.
      // Ctrl/Cmd+Enter also sends (extra affordance, harmless).
      if (!e.shiftKey) {
        e.preventDefault();
        handleSubmit(e);
      }
    } else {
      // Default desktop mode: Enter is newline, Ctrl/Cmd+Enter sends,
      // Shift+Enter is newline.
      if (e.ctrlKey || e.metaKey) {
        e.preventDefault();
        handleSubmit(e);
      }
    }
  };

  const handleInput = () => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`;
  };

  const canSend = text.trim().length > 0 && !disabled && !anyUploading;

  const sendShortcutLabel = isMobile
    ? null
    : sendOnEnter
      ? "Send (Enter)"
      : "Send (Ctrl+Enter)";

  const sendButton = (
    <Button
      type="submit"
      size="icon"
      disabled={!canSend}
      aria-label="Send message"
      className="min-h-[44px] min-w-[44px]"
    >
      <Send className="h-4 w-4" />
    </Button>
  );

  const handleFileChange = (e: { target: { files: FileList | null; value: string } }) => {
    const files = Array.from(e.target.files ?? []);
    if (files.length > 0) onAddFiles?.(files);
    e.target.value = "";
  };

  return (
    <form onSubmit={handleSubmit} className="border-t border-border p-4">
      {workspaceId && drawerOpen && (
        <div
          id="composer-options-drawer"
          data-testid="composer-options-drawer"
          className="flex flex-wrap items-center gap-2 pb-2"
        >
          <ModelSelector workspaceId={workspaceId} disabled={disabled} />
          <RoleSelector workspaceId={workspaceId} orgId={orgId} disabled={disabled} />
        </div>
      )}

      {attachments.length > 0 && (
        <div className="flex flex-wrap gap-1.5 pb-2">
          {attachments.map((chip) => (
            <ComposerChip key={chip.id} chip={chip} onRemove={onRemoveAttachment} onRetry={onRetryAttachment} />
          ))}
        </div>
      )}

      {uploadErrorNotice && (
        <div role="status" aria-label="upload-error-notice" className="mb-2 rounded-md border border-destructive/40 bg-destructive/10 px-2 py-1 text-xs text-destructive">
          {uploadErrorNotice}
        </div>
      )}

      {capViolation && (
        <div role="status" className="mb-2 flex items-center justify-between gap-2 rounded-md border border-yellow-500/40 bg-yellow-500/10 px-2 py-1 text-xs text-yellow-800 dark:text-yellow-200">
          <span>Attachment limit reached — up to 10 files per message.</span>
          <button
            type="button"
            aria-label="Dismiss attachment notice"
            onClick={onDismissCapViolation}
            className="shrink-0 underline hover:no-underline"
          >
            Dismiss
          </button>
        </div>
      )}

      <div className="flex items-end gap-2">
        {workspaceId && (
          <>
            <Button
              type="button"
              size="icon"
              variant="ghost"
              className="min-h-[44px] min-w-[44px]"
              aria-label="Toggle composer options"
              aria-expanded={drawerOpen}
              aria-controls="composer-options-drawer"
              onClick={toggleDrawer}
            >
              {drawerOpen ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
            </Button>
            <input
              ref={fileInputRef}
              type="file"
              multiple
              hidden
              data-testid="composer-file-input"
              onChange={handleFileChange}
            />
            <Button
              type="button"
              size="icon"
              variant="ghost"
              className="min-h-[44px] min-w-[44px]"
              aria-label="Attach files"
              onClick={() => fileInputRef.current?.click()}
            >
              <Paperclip className="h-4 w-4" />
            </Button>
          </>
        )}
        <textarea
          ref={textareaRef}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={handleKeyDown}
          onInput={handleInput}
          placeholder={placeholder}
          disabled={disabled}
          rows={1}
          className={cn(
            "min-h-[44px] flex-1 resize-none rounded-md border border-input bg-background px-3 py-2 text-base placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50",
          )}
        />
        {sendShortcutLabel ? (
          <Tooltip content={sendShortcutLabel} side="top">
            {sendButton}
          </Tooltip>
        ) : (
          sendButton
        )}
        {streaming && onAbort && (
          <Button
            type="button"
            size="icon"
            variant="destructive"
            className="min-h-[44px] min-w-[44px]"
            aria-label="Stop generating"
            onClick={(e) => { e.preventDefault(); onAbort(); }}
          >
            <Square className="h-4 w-4" />
          </Button>
        )}
      </div>
    </form>
  );
}
