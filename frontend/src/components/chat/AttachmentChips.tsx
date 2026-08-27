import { FileText } from "lucide-react";
import type { Attachment } from "../../lib/attachments";

interface Props {
  attachments: Attachment[];
}

/** Read-only attachment chips rendered in user message bubbles (D11/U1.6.9). */
export function AttachmentChips({ attachments }: Props) {
  if (attachments.length === 0) return null;
  return (
    <div className="mt-1 flex flex-wrap gap-1.5">
      {attachments.map((a) => (
        <span
          key={`${a.path}-${a.name}`}
          data-testid="history-attachment-chip"
          className="inline-flex max-w-full items-center gap-1 rounded-md bg-primary-foreground/15 px-1.5 py-0.5 text-xs"
        >
          <FileText className="h-3 w-3 shrink-0" aria-hidden="true" />
          <span className="truncate">{a.name}</span>
        </span>
      ))}
    </div>
  );
}
