import { useCallback, useEffect, useRef, useState } from "react";
import type { Dispatch, RefObject, SetStateAction } from "react";
import { useLocation } from "react-router-dom";
import { useIsMobile } from "./useMediaQuery";
import { useSwipeableSidebar } from "./useSwipeableSidebar";

const DEFAULT_SIDEBAR_WIDTH = 256;

export interface CollapsibleSidebarOptions {
  sidebarWidth?: number;
}

export interface CollapsibleSidebarState {
  isMobile: boolean;
  open: boolean;
  setOpen: Dispatch<SetStateAction<boolean>>;
  close: () => void;
  containerRef: RefObject<HTMLDivElement | null>;
  sidebarRef: RefObject<HTMLDivElement | null>;
  overlayRef: RefObject<HTMLDivElement | null>;
  sidebarWidth: number;
}

export function useCollapsibleSidebar(
  options?: CollapsibleSidebarOptions,
): CollapsibleSidebarState {
  const sidebarWidth = options?.sidebarWidth ?? DEFAULT_SIDEBAR_WIDTH;
  const isMobile = useIsMobile();
  const [open, setOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const sidebarRef = useRef<HTMLDivElement>(null);
  const overlayRef = useRef<HTMLDivElement>(null);
  const location = useLocation();
  const isInitialMount = useRef(true);

  useSwipeableSidebar({
    containerRef,
    sidebarRef,
    overlayRef,
    isOpen: open,
    setIsOpen: setOpen,
    enabled: isMobile,
    sidebarWidth,
  });

  useEffect(() => {
    if (isInitialMount.current) {
      isInitialMount.current = false;
      return;
    }
    setOpen(false);
  }, [location.pathname]);

  const close = useCallback(() => setOpen(false), []);

  return {
    isMobile,
    open,
    setOpen,
    close,
    containerRef,
    sidebarRef,
    overlayRef,
    sidebarWidth,
  };
}
