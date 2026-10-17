import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, Fragment } from "react";
import type { ReactNode } from "react";
import type { Message } from "../../api/types";
import type { ModelInfo } from "../../api/workspaces";
import { MessageBubble } from "./MessageBubble";
import { ArrowDown } from "lucide-react";
import { Spinner } from "../ui/Spinner";

interface Props {
  messages: Message[];
  streaming?: boolean;
  streamingBubble?: ReactNode;
  trailingPrompts?: ReactNode;
  onLoadEarlier?: () => void;
  hasOlderMessages?: boolean;
  loadingOlder?: boolean;
  models?: ModelInfo[];
  lastSeenAt?: string;
}

const SCROLL_THRESHOLD = 60;
const CLOCK_SKEW_BUFFER_MS = 1000;

const MemoizedBubble = memo(MessageBubble);

export function MessageList({ messages, streaming, streamingBubble, trailingPrompts, onLoadEarlier, hasOlderMessages, loadingOlder, models, lastSeenAt }: Props) {
  const modelMap = useMemo(() => new Map(models?.map(m => [m.id, m.name])), [models]);
  const scrollRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const [showJumpButton, setShowJumpButton] = useState(false);
  const stickToBottom = useRef(true);
  // #1720 R4: our own programmatic scroll assignments fire `scroll`
  // events just like user scrolls do. This guard marks them so the
  // user-intent detector (checkIfAtBottom) can tell them apart — a
  // programmatic snap must never arm, disarm, or otherwise feed the
  // near-bottom heuristic.
  const programmaticScrollRef = useRef(false);
  // Mirror of the `streaming` prop so the stable scroll handler can read
  // it without re-subscribing listeners on every flip.
  const streamingRef = useRef(streaming);

  // #1720 R4: perform every programmatic scroll through here. A no-op
  // assignment (already at the target) fires NO scroll event and must
  // not mark the guard — a lingering mark would swallow the next
  // genuine user scroll event. A real assignment fires exactly one
  // scroll event (dispatched before the next rAF in the same rendering
  // update); the mark is consumed by that event and self-clears on the
  // next frame as a backstop.
  const setProgrammaticScrollTop = useCallback((el: HTMLDivElement, target: number) => {
    if (el.scrollTop === target) return;
    programmaticScrollRef.current = true;
    el.scrollTop = target;
    requestAnimationFrame(() => {
      programmaticScrollRef.current = false;
    });
  }, []);

  useEffect(() => {
    streamingRef.current = streaming;
  }, [streaming]);

  const dividerIndex = useMemo(() => {
    if (!lastSeenAt) return -1;
    const threshold = new Date(lastSeenAt).getTime() - CLOCK_SKEW_BUFFER_MS;
    return messages.findIndex(
      (msg) => msg.createdAt && new Date(msg.createdAt).getTime() > threshold
    );
  }, [messages, lastSeenAt]);

  const rafId = useRef(0);
  const checkIfAtBottom = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    // #1720 R4: scroll events originating from OUR programmatic snaps
    // are not user intent. Consume the guard and bail — they must never
    // arm, disarm, or re-run the near-bottom heuristic (the post-snap
    // near-bottom read is exactly what silently re-armed the tail and
    // produced the snap-back loop).
    if (programmaticScrollRef.current) {
      programmaticScrollRef.current = false;
      return;
    }
    // Record the user's intent SYNCHRONOUSLY, not in a deferred rAF. During
    // streaming the MutationObserver auto-scrolls to the bottom on every
    // token; if this update were deferred to a rAF, a token whose observer
    // rAF runs first would pull the viewport back to the bottom before the
    // user's scroll-up was ever observed — trapping the user at the tail and
    // making earlier content unreadable while waiting for a response. Reading
    // scrollTop here reflects the position at the moment of the scroll event,
    // before any later mutation can act on a stale value.
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < SCROLL_THRESHOLD;
    if (!nearBottom) {
      // #1720 R1: any genuine user scroll away disarms tailing,
      // durably — until the explicit resume control re-arms it.
      stickToBottom.current = false;
    } else if (!streamingRef.current && !stickToBottom.current) {
      // #1720 R3: re-arming from a near-bottom read is legal ONLY while
      // idle (existing convention: scroll back down → follow again).
      // While a stream is active it is FORBIDDEN — that heuristic is
      // the snap-back loop. Mid-stream, the ONLY re-arm is the explicit
      // "Resume tailing" control.
      stickToBottom.current = true;
    }
    // The React state update (which drives a re-render) is cheap to coalesce,
    // so it stays behind a rAF to batch rapid scroll events.
    if (rafId.current) return;
    rafId.current = requestAnimationFrame(() => {
      rafId.current = 0;
      setShowJumpButton((prev) => {
        const shouldShow = !stickToBottom.current;
        return prev === shouldShow ? prev : shouldShow;
      });
    });
  }, []);

  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    el.addEventListener("scroll", checkIfAtBottom, { passive: true });
    return () => {
      el.removeEventListener("scroll", checkIfAtBottom);
      if (rafId.current) cancelAnimationFrame(rafId.current);
    };
  }, [checkIfAtBottom]);

  const scrollToBottom = useCallback(() => {
    const el = scrollRef.current;
    if (el) setProgrammaticScrollTop(el, el.scrollHeight);
    // #1720 R3: this is the EXPLICIT re-arm path (resume control / jump
    // button) — the only arm that is legal while a stream is active.
    stickToBottom.current = true;
    setShowJumpButton(false);
  }, [setProgrammaticScrollTop]);

  const hasTrailingPrompts = trailingPrompts != null;

  // Scroll-anchoring state for "Load earlier messages". When older messages
  // are prepended, the browser keeps the same pixel scrollTop but the content
  // above that offset has grown — so the user's viewport silently shifts to
  // different (newer) content than what they were reading, which feels
  // broken. We capture the previous first-message id and scrollHeight, and
  // on the next layout effect detect "prepend" (first id changed while
  // stickToBottom is false) by adding the height delta to scrollTop, which
  // keeps the user's visual anchor in the same screen position.
  const prevFirstIdRef = useRef<string | undefined>(undefined);
  const prevScrollHeightRef = useRef(0);

  // Initialize the refs on mount so subsequent renders have a real
  // "previous" baseline. Without this, the first content change after
  // mount would see prevScrollHeight=0 and skip anchoring.
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    prevFirstIdRef.current = messages[0]?.id;
    prevScrollHeightRef.current = el.scrollHeight;
    // We do NOT scroll to bottom here — the existing layout effect below
    // owns that. This effect runs once on mount only.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const newFirstId = messages[0]?.id;
    const newScrollHeight = el.scrollHeight;
    const prevFirstId = prevFirstIdRef.current;
    const prevScrollHeight = prevScrollHeightRef.current;

    if (stickToBottom.current) {
      // Existing behavior: jump to bottom on new content.
      setProgrammaticScrollTop(el, newScrollHeight);
    } else if (
      prevFirstId !== undefined &&
      newFirstId !== undefined &&
      prevFirstId !== newFirstId
    ) {
      // Scroll anchoring: content was prepended (Load earlier completed).
      // Restore the visual position by adding the height delta so the
      // message the user was reading stays at the same screen position.
      setProgrammaticScrollTop(el, el.scrollTop + (newScrollHeight - prevScrollHeight));
    }

    prevFirstIdRef.current = newFirstId;
    prevScrollHeightRef.current = newScrollHeight;
    // Deps are [messages.length, hasTrailingPrompts] rather than [messages]
    // because TanStack Query's select returns a new array reference on every
    // emit; depending on `messages` would run this effect on every render.
    // Length + the firstId check inside captures every meaningful change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [messages.length, hasTrailingPrompts]);

  useEffect(() => {
    // #1720 R2/R3: a stream starting continues an armed tail but NEVER
    // re-arms a disarmed one — the user's free scroll persists until
    // they explicitly resume. scrollToBottom() inside this guard arms
    // only when already armed; when disarmed it must not run at all.
    if (streaming && stickToBottom.current) {
      scrollToBottom();
    }
  }, [streaming, scrollToBottom]);

  useEffect(() => {
    if (!streaming || !stickToBottom.current) return;
    const el = scrollRef.current;
    if (!el) return;
    let frameId = 0;
    const observer = new MutationObserver(() => {
      if (!stickToBottom.current || frameId) return;
      frameId = requestAnimationFrame(() => {
        frameId = 0;
        if (stickToBottom.current && el) {
          setProgrammaticScrollTop(el, el.scrollHeight);
        }
      });
    });
    observer.observe(el, { childList: true, subtree: true, characterData: true });
    return () => {
      observer.disconnect();
      if (frameId) cancelAnimationFrame(frameId);
    };
  }, [streaming, setProgrammaticScrollTop]);

  if (messages.length === 0 && !streamingBubble && !trailingPrompts) {
    return (
      <div className="flex flex-1 items-center justify-center text-muted-foreground">
        <p className="text-sm">Send a message to start the conversation</p>
      </div>
    );
  }

  return (
    <div className="relative flex-1 overflow-hidden">
      <div
        ref={scrollRef}
        className="h-full overflow-y-auto overflow-x-hidden overscroll-contain"
        role="log"
        aria-live="polite"
        aria-label="Chat messages"
      >
        <div className="flex flex-col gap-2 p-2">
          {hasOlderMessages && (
            <div
              data-testid="load-earlier-anchor"
              className="sticky top-0 z-10 flex justify-center py-3 bg-background/80 backdrop-blur-sm"
            >
              {loadingOlder ? (
                <Spinner size="sm" />
              ) : (
                <button
                  onClick={onLoadEarlier}
                  aria-label="Load earlier messages"
                  className="rounded-md border border-border bg-background px-3 py-1.5 text-xs text-muted-foreground hover:bg-accent transition-colors"
                >
                  Load earlier messages
                </button>
              )}
            </div>
          )}

          {messages.map((msg, i) => (
            <Fragment key={msg.id}>
              {i === dividerIndex && (
                <div role="separator" aria-label="New messages" className="flex items-center gap-2 py-2 my-1">
                  <div className="flex-1 border-t border-border" />
                  <span className="text-xs text-muted-foreground font-medium whitespace-nowrap px-2">New messages</span>
                  <div className="flex-1 border-t border-border" />
                </div>
              )}
              <MemoizedBubble
                message={msg}
                modelName={msg.modelID ? (modelMap.get(msg.modelID) || msg.modelID.split("/").pop()) : undefined}
              />
            </Fragment>
          ))}

          {streamingBubble && streamingBubble}

          {trailingPrompts}

          <div ref={bottomRef} />
        </div>
      </div>

      {showJumpButton && (
        <button
          onClick={scrollToBottom}
          className="absolute bottom-4 left-1/2 -translate-x-1/2 flex items-center gap-1.5 rounded-full bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground shadow-lg hover:bg-primary/90 transition-opacity"
          aria-label="Scroll to bottom"
        >
          <ArrowDown className="h-3.5 w-3.5" />
          {streaming ? "Resume tailing" : "Jump to bottom"}
        </button>
      )}
    </div>
  );
}
