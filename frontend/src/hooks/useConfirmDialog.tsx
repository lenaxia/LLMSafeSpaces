import { useCallback, useState, type ReactNode } from "react";
import { ConfirmDialog } from "../components/ui/ConfirmDialog";

interface ConfirmOptions {
  title: string;
  description: string;
  note?: string;
  confirmLabel?: string;
  cancelLabel?: string;
  destructive?: boolean;
  onConfirm: () => void;
}

/**
 * useConfirmDialog provides an imperative confirm API backed by the
 * accessible ConfirmDialog (Radix DOM portal) instead of window.confirm.
 *
 * Usage:
 *   const { confirm, dialog } = useConfirmDialog();
 *
 *   // In an event handler:
 *   confirm({
 *     title: "Delete session?",
 *     description: "This cannot be undone.",
 *     confirmLabel: "Delete",
 *     destructive: true,
 *     onConfirm: () => doDelete(id),
 *   });
 *
 *   // At the bottom of the component's JSX:
 *   return (<>{content}{dialog}</>);
 *
 * Unlike window.confirm, the Radix Dialog renders inside the DOM so it
 * works in sandboxed iframes / permissions-restricted contexts where
 * window.confirm is blocked entirely.
 */
export function useConfirmDialog(): {
  confirm: (opts: ConfirmOptions) => void;
  dialog: ReactNode;
} {
  const [state, setState] = useState<ConfirmOptions | null>(null);

  const confirm = useCallback((opts: ConfirmOptions) => {
    setState(opts);
  }, []);

  const dialog = (
    <ConfirmDialog
      open={!!state}
      onOpenChange={(o: boolean) => { if (!o) setState(null); }}
      title={state?.title ?? ""}
      description={state?.description ?? ""}
      note={state?.note}
      confirmLabel={state?.confirmLabel ?? "Confirm"}
      cancelLabel={state?.cancelLabel}
      destructive={state?.destructive ?? false}
      onConfirm={() => {
        const cb = state?.onConfirm;
        setState(null);
        cb?.();
      }}
    />
  );

  return { confirm, dialog };
}
