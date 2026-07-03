import type { Message, MessagePart } from "../../api/types";
import type { ModelInfo } from "../../api/workspaces";
import { MessageList } from "./MessageList";
import { Composer } from "./Composer";
import { ReadOnlyBanner } from "./ReadOnlyBanner";
import { StreamingIndicator } from "./StreamingIndicator";
import { MessageBubble } from "./MessageBubble";
import { QueueSection } from "./QueueSection";
import { useIsMobile } from "../../hooks/useMediaQuery";
import type { QueuedMessage } from "../../hooks/useMessageQueue";

interface StreamingPart {
  type: "thinking" | "text" | "tool";
  text: string;
  toolState?: string;
  toolCallID?: string;
  toolInput?: unknown;
  toolOutput?: string;
  /**
   * The opencode messageID this part belongs to. Opencode partitions an
   * assistant turn into multiple messages (each terminated by a tool call);
   * to match how the transcript renders after history refresh, the streaming
   * view must render one bubble per messageID. Parts without a messageID
   * (older code paths, tests) collapse into a single default bubble.
   */
  messageID?: string;
}

interface Props {
  messages: Message[];
  streaming: boolean;
  streamParts: StreamingPart[];
  disabled: boolean;
  onSend: (text: string) => void;
  onAbort: () => void;
  prompts?: React.ReactNode;
  onLoadEarlier?: () => void;
  hasOlderMessages?: boolean;
  loadingOlder?: boolean;
  queuedMessages?: QueuedMessage[];
  onQueueRetry?: (id: string) => void;
  onQueueDismiss?: (id: string) => void;
  models?: ModelInfo[];
  lastSeenAt?: string;
  /**
   * When true, the chat is read-only: the composer and message queue are
   * hidden and a view-only banner is rendered in their place. Used for
   * subagent/subtask sessions, which are driven by their parent session and
   * must not be chatted with directly (helps enforce max session limits).
   */
  viewOnly?: boolean;
  viewOnlyMessage?: string;
}

const DEFAULT_STREAM_BUBBLE_KEY = "__stream_default__";

function partitionStreamPartsByMessage(streamParts: StreamingPart[]): Array<{ key: string; parts: MessagePart[] }> {
  const order: string[] = [];
  const groups = new Map<string, MessagePart[]>();
  for (const p of streamParts) {
    const key = p.messageID ?? DEFAULT_STREAM_BUBBLE_KEY;
    if (!groups.has(key)) {
      groups.set(key, []);
      order.push(key);
    }
    groups.get(key)!.push({
      type: p.type === "tool" ? ("tool_use" as const) : p.type,
      text: p.text,
      ...(p.toolState ? { toolState: p.toolState } : {}),
      ...(p.toolInput != null ? { input: p.toolInput } : {}),
      ...(p.toolOutput ? { toolOutput: p.toolOutput } : {}),
    });
  }
  return order.map((key) => ({ key, parts: groups.get(key)! }));
}

export function ChatView({ messages, streaming, streamParts, disabled, onSend, onAbort, prompts, onLoadEarlier, hasOlderMessages, loadingOlder, queuedMessages = [], onQueueRetry, onQueueDismiss, models, lastSeenAt, viewOnly = false, viewOnlyMessage }: Props) {
  const isMobile = useIsMobile();
  const streamedBubbles = partitionStreamPartsByMessage(streamParts);
  const hasStreamedContent = streamedBubbles.length > 0;

  return (
    <div className="flex h-full flex-col">
      <div className="flex flex-1 flex-col overflow-hidden">
        <MessageList
          messages={messages}
          streaming={streaming}
          models={models}
          streamingBubble={
            streaming && hasStreamedContent ? (
              <>
                {streamedBubbles.map((b) => (
                  <MessageBubble
                    key={b.key}
                    message={{ id: `streaming-${b.key}`, role: "assistant", parts: b.parts }}
                    isStreaming
                  />
                ))}
              </>
            ) : undefined
          }
          trailingPrompts={prompts}
          onLoadEarlier={onLoadEarlier}
          hasOlderMessages={hasOlderMessages}
          loadingOlder={loadingOlder}
          lastSeenAt={lastSeenAt}
        />
        {streaming && <StreamingIndicator />}
      </div>

      {viewOnly ? (
        <ReadOnlyBanner message={viewOnlyMessage} />
      ) : (
        <>
          {onQueueRetry && onQueueDismiss && (
            <QueueSection
              messages={queuedMessages}
              onRetry={onQueueRetry}
              onDismiss={onQueueDismiss}
              isMobile={isMobile}
            />
          )}

          <Composer onSend={onSend} onAbort={onAbort} disabled={disabled} streaming={streaming} />
        </>
      )}
    </div>
  );
}
