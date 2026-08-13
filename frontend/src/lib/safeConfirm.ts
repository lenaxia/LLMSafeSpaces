/**
 * safeConfirm wraps window.confirm to fail CLOSED on exception.
 *
 * window.confirm can throw in sandboxed iframes, permissions-restricted
 * contexts, or headless environments. When it throws, the safe behavior
 * for a destructive action is to ABORT (return false), not proceed.
 *
 * The two production sites that previously wrapped confirm() in try/catch
 * (ChatPage session delete, Sidebar workspace delete) failed OPEN — the
 * catch block proceeded with deletion, causing data loss when the dialog
 * was blocked. This utility prevents that class of bug.
 *
 * All confirm() calls in the app should use safeConfirm instead.
 */
export function safeConfirm(message: string): boolean {
  try {
    return window.confirm(message);
  } catch {
    return false;
  }
}
