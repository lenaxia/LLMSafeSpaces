import type { ReactNode } from "react";
import type { CollapsibleSidebarState } from "../../hooks/useCollapsibleSidebar";

interface SidebarDrawerProps {
  state: CollapsibleSidebarState;
  children: ReactNode;
  ariaLabel?: string;
  desktopClassName?: string;
}

export function SidebarDrawer({
  state,
  children,
  ariaLabel = "Navigation",
  desktopClassName = "relative",
}: SidebarDrawerProps) {
  const { isMobile, open, setOpen, overlayRef, sidebarRef, sidebarWidth } = state;
  return (
    <>
      {isMobile && (
        <div
          ref={overlayRef}
          className={`fixed inset-0 z-30 bg-black/50 transition-opacity duration-200 ${
            open ? "opacity-100 pointer-events-auto" : "opacity-0 pointer-events-none"
          }`}
          onClick={() => setOpen(false)}
          aria-hidden="true"
        />
      )}
      <div
        ref={sidebarRef}
        style={isMobile ? { width: `${sidebarWidth}px` } : undefined}
        className={
          isMobile
            ? `fixed inset-y-0 left-0 z-40 transform transition-transform duration-200 ${
                open ? "translate-x-0" : "-translate-x-full"
              }`
            : desktopClassName
        }
        aria-label={ariaLabel}
      >
        {children}
      </div>
    </>
  );
}
