// EntityLink — an internal app link to an entity view (board story/
// epic/project, session chat), used by the chat linkify pipeline and
// the agent-origin header (tsk-vibrant-cadet-pearls).
//
// Renders a real react-router <Link> (SPA navigation, keyboard-focus
// order for free) when a router context exists; a plain <a> when it
// does not (bare unit-test renders), so the component never crashes
// outside the router. Accessible label carries entity type + title
// (a11y: no raw-id soup for screen readers when a title is known).

import { Link, useInRouterContext } from "react-router-dom";

interface Props {
  href: string;
  /** Accessible label — entity type + title (+ id). */
  label?: string;
  /** Hover tooltip (e.g. the full raw id). */
  title?: string;
  className?: string;
  testId?: string;
  children: React.ReactNode;
}

export function EntityLink({ href, label, title, className, testId, children }: Props) {
  const inRouter = useInRouterContext();
  const ariaProps = label ? { "aria-label": label } : {};
  if (inRouter) {
    return (
      <Link to={href} title={title} data-testid={testId} className={className} {...ariaProps}>
        {children}
      </Link>
    );
  }
  return (
    <a href={href} title={title} data-testid={testId} className={className} {...ariaProps}>
      {children}
    </a>
  );
}
