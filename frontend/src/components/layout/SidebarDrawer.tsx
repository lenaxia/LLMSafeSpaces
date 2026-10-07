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
  const { isMobile, open, setOpen, overlayRef, sidebarRef, handleRef, sidebarWidth } = state;
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
      {isMobile && !open && (
        // #1623: the inset drag-handle affordance — swipe-to-open engages
        // for touches starting inside this strip's rect (28px wide, 16px
        // in from the safe-area inset), offset CLEAR of the OS
        // back-gesture zone. The strip is pointer-events:none — purely
        // VISUAL plus the coordinate region the hook checks — so taps
        // under it pass through to the real targets (the r2 review's
        // occlusion fix: the hamburger must keep working), vertical
        // scrolls over it reach the content beneath, and the gesture is
        // claimed only at the horizontal-move stage. Rendered only when
        // closed; swipe-to-close lives on the drawer surface.
        <div
          ref={handleRef}
          data-sidebar-handle=""
          aria-hidden="true"
          className="pointer-events-none fixed inset-y-0 z-40 w-7 left-[calc(env(safe-area-inset-left,0px)+16px)]"
        >
          <div className="absolute left-1/2 top-1/2 h-12 w-1 -translate-x-1/2 -translate-y-1/2 rounded-full bg-foreground/25" />
        </div>
      )}
    </>
  );
}
