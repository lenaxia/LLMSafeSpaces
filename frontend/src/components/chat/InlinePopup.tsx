// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useRef } from "react";
import { cn } from "../../lib/utils";

/**
 * InlinePopup — the shared palette machinery for the composer's slash
 * commands and @-prompt recall (#1496). One component family: a
 * keyboard-driven listbox rendered above the textarea. Keys arrive on
 * the focused textarea and are routed here by the composer's key
 * handler; this component renders, tracks the active item, scrolls it
 * into view, and handles pointer selection + outside-click dismissal.
 */
export interface InlinePopupProps<T> {
  items: T[];
  /** Index of the keyboard-active item (parent-owned state). */
  activeIndex: number;
  onActiveChange: (index: number) => void;
  /** Item chosen (Enter or click). */
  onSelect: (item: T, index: number) => void;
  onDismiss: () => void;
  renderItem: (item: T, active: boolean) => React.ReactNode;
  testId: string;
  ariaLabel: string;
  /** Rendered when items is empty (the caller decides whether to show at all). */
  emptyLabel?: string;
}

export function InlinePopup<T>({
  items,
  activeIndex,
  onActiveChange,
  onSelect,
  onDismiss,
  renderItem,
  testId,
  ariaLabel,
  emptyLabel,
}: InlinePopupProps<T>) {
  const rootRef = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  // Outside click / focus loss dismisses (the textarea keeps focus for
  // typing; a click landing outside both the popup and the textarea is
  // a real blur of intent).
  useEffect(() => {
    const onPointerDown = (e: PointerEvent) => {
      const target = e.target as Node;
      if (rootRef.current?.contains(target)) return;
      const textarea = target instanceof Element && target.closest("textarea");
      if (textarea) return; // still typing — not a dismissal
      onDismiss();
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [onDismiss]);

  // Keep the active item visible when arrowing through long lists.
  useEffect(() => {
    const active = listRef.current?.querySelector<HTMLElement>(`[data-index="${activeIndex}"]`);
    active?.scrollIntoView({ block: "nearest" });
  }, [activeIndex]);

  return (
    <div
      ref={rootRef}
      data-testid={testId}
      className="absolute bottom-full left-0 z-30 mb-1 w-80 max-w-full"
    >
      <div
        className="max-h-64 overflow-y-auto rounded-md border border-border bg-popover shadow-md"
        role="listbox"
        aria-label={ariaLabel}
      >
        <div ref={listRef}>
          {items.length === 0 && emptyLabel !== undefined ? (
            <div className="px-3 py-2 text-xs text-muted-foreground" role="option" aria-selected={false} aria-disabled="true">
              {emptyLabel}
            </div>
          ) : (
            items.map((item, i) => (
              <div
                key={i}
                data-index={i}
                role="option"
                aria-selected={i === activeIndex}
                data-active={i === activeIndex}
                className={cn(
                  "cursor-pointer px-3 py-2 text-sm",
                  i === activeIndex ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
                )}
                onPointerDown={(e) => e.preventDefault()} // keep textarea focus
                onClick={() => onSelect(item, i)}
                onMouseEnter={() => onActiveChange(i)}
              >
                {renderItem(item, i === activeIndex)}
              </div>
            ))
          )}
        </div>
      </div>
    </div>
  );
}
