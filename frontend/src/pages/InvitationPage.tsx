import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { useAuth } from "../providers/AuthProvider";
import { orgsApi, type InvitationDetail } from "../api/orgs";
import { ApiClientError } from "../api/client";
import { Button } from "../components/ui/Button";
import { Spinner } from "../components/ui/Spinner";

type PageState =
  | { tag: "loading" }
  | { tag: "error"; message: string }
  | {
      tag: "detail";
      invitation: InvitationDetail;
      token: string;
      actionError: string;
    }
  | { tag: "accepting"; invitation: InvitationDetail; token: string }
  | { tag: "accepted"; orgName: string }
  | { tag: "declining"; invitation: InvitationDetail; token: string }
  | { tag: "declined" }
  | { tag: "alreadyHandled"; reason: string };

function roleLabel(role: string): string {
  return role === "admin" ? "Admin" : "Member";
}

function expiresText(expiresAt: string): string {
  return new Date(expiresAt).toLocaleDateString(undefined, {
    year: "numeric",
    month: "long",
    day: "numeric",
  });
}

function InvitationShell({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="flex min-h-screen flex-col items-center justify-center gap-4 px-4">
      <div className="w-full max-w-md rounded border border-border bg-card p-6">
        <h1 className="mb-4 text-xl font-semibold">{title}</h1>
        {children}
      </div>
    </div>
  );
}

function InvitationFields({ invitation }: { invitation: InvitationDetail }) {
  return (
    <div className="space-y-3 text-sm">
      <div>
        <span className="text-muted-foreground">Organisation:</span>{" "}
        <span className="font-medium">{invitation.orgName}</span>
      </div>
      <div>
        <span className="text-muted-foreground">Invited by:</span>{" "}
        <span>{invitation.inviterName}</span>
      </div>
      <div>
        <span className="text-muted-foreground">Role:</span>{" "}
        <span>{roleLabel(invitation.role)}</span>
      </div>
      <div>
        <span className="text-muted-foreground">Expires:</span>{" "}
        <span>{expiresText(invitation.expiresAt)}</span>
      </div>
    </div>
  );
}

function AuthActions({
  token,
  state,
  actionError,
  onAccept,
  onDecline,
}: {
  token: string;
  state: PageState;
  actionError: string;
  onAccept: () => void;
  onDecline: () => void;
}) {
  const { user } = useAuth();

  if (!user) {
    return (
      <div className="mt-6 text-center">
        <p className="mb-3 text-sm text-muted-foreground">
          Sign in or create an account to accept this invitation.
        </p>
        <div className="flex gap-3">
          <Link
            to={`/login?return_to=${encodeURIComponent(`/invitations/${token}`)}`}
            className="flex-1"
          >
            <Button variant="default" className="w-full">
              Sign in
            </Button>
          </Link>
          <Link
            to={`/register?return_to=${encodeURIComponent(`/invitations/${token}`)}`}
            className="flex-1"
          >
            <Button variant="outline" className="w-full">
              Create account
            </Button>
          </Link>
        </div>
      </div>
    );
  }

  const isActing = state.tag === "accepting" || state.tag === "declining";

  return (
    <>
      {actionError && (
        <p className="mt-3 text-sm text-red-500">{actionError}</p>
      )}
      <div className="mt-4 flex gap-3">
        <Button
          variant="default"
          className="flex-1"
          onClick={onAccept}
          disabled={isActing}
        >
          Accept
        </Button>
        <Button
          variant="outline"
          className="flex-1"
          onClick={onDecline}
          disabled={isActing}
        >
          Decline
        </Button>
      </div>
    </>
  );
}

export function InvitationPage() {
  const { token } = useParams<{ token: string }>();
  const { loading: authLoading } = useAuth();
  const [state, setState] = useState<PageState>({ tag: "loading" });

  useEffect(() => {
    if (!token) {
      setState({ tag: "error", message: "Missing invitation token." });
      return;
    }

    let cancelled = false;

    (async () => {
      try {
        const invitation = await orgsApi.getInvitationByToken(token);
        if (cancelled) return;
        setState({ tag: "detail", invitation, token, actionError: "" });
      } catch (err) {
        if (cancelled) return;
        if (err instanceof ApiClientError && err.status === 404) {
          setState({ tag: "error", message: "Invitation not found or has expired." });
        } else if (err instanceof ApiClientError) {
          setState({ tag: "error", message: err.body?.error ?? "Failed to load invitation." });
        } else {
          setState({ tag: "error", message: "Something went wrong. Please try again." });
        }
      }
    })();

    return () => { cancelled = true; };
  }, [token]);

  const handleAccept = async () => {
    if (state.tag !== "detail") return;
    const inv = state.invitation;
    const tok = state.token;
    setState({ tag: "accepting", invitation: inv, token: tok });
    try {
      await orgsApi.acceptInvitation(tok);
      setState({ tag: "accepted", orgName: inv.orgName });
    } catch (err) {
      if (err instanceof ApiClientError) {
        switch (err.status) {
          case 403:
            setState({
              tag: "detail",
              invitation: inv,
              token: tok,
              actionError:
                "This invitation was sent to a different email address. Sign in with the invited email to accept.",
            });
            return;
          case 409:
            setState({
              tag: "alreadyHandled",
              reason:
                err.body?.error ??
                "You have already accepted or declined this invitation.",
            });
            return;
          case 410:
            setState({ tag: "error", message: "This invitation has expired." });
            return;
          default:
            setState({
              tag: "detail",
              invitation: inv,
              token: tok,
              actionError: err.body?.error ?? "Failed to accept invitation.",
            });
            return;
        }
      }
      setState({
        tag: "detail",
        invitation: inv,
        token: tok,
        actionError: "Something went wrong. Please try again.",
      });
    }
  };

  const handleDecline = async () => {
    if (state.tag !== "detail") return;
    const inv = state.invitation;
    const tok = state.token;
    setState({ tag: "declining", invitation: inv, token: tok });
    try {
      await orgsApi.declineInvitation(tok);
      setState({ tag: "declined" });
    } catch (err) {
      const msg =
        err instanceof ApiClientError
          ? (err.body?.error ?? "Failed to decline invitation.")
          : "Something went wrong. Please try again.";
      setState({ tag: "detail", invitation: inv, token: tok, actionError: msg });
    }
  };

  // --- Loading ---
  if (state.tag === "loading" || authLoading) {
    return (
      <div className="flex min-h-screen items-center justify-center">
        <Spinner size="lg" />
      </div>
    );
  }

  // --- Fatal error ---
  if (state.tag === "error") {
    return (
      <InvitationShell title="Invitation">
        <p className="text-muted-foreground">{state.message}</p>
        <Link to="/chat" className="mt-4 inline-block">
          <Button variant="outline">Go to chat</Button>
        </Link>
      </InvitationShell>
    );
  }

  // --- Already accepted / declined ---
  if (state.tag === "alreadyHandled") {
    return (
      <InvitationShell title="Invitation">
        <p className="text-muted-foreground">{state.reason}</p>
        <Link to="/chat" className="mt-4 inline-block">
          <Button variant="outline">Go to chat</Button>
        </Link>
      </InvitationShell>
    );
  }

  // --- Accepted ---
  if (state.tag === "accepted") {
    return (
      <InvitationShell title="Invitation accepted!">
        <p className="text-muted-foreground">
          You are now a member of{" "}
          <span className="font-medium">{state.orgName}</span>.
        </p>
        <Link to="/chat" className="mt-4 inline-block">
          <Button>Go to chat</Button>
        </Link>
      </InvitationShell>
    );
  }

  // --- Declined ---
  if (state.tag === "declined") {
    return (
      <InvitationShell title="Invitation declined">
        <p className="text-muted-foreground">
          You have declined this invitation.
        </p>
        <Link to="/chat" className="mt-4 inline-block">
          <Button variant="outline">Go to chat</Button>
        </Link>
      </InvitationShell>
    );
  }

  // All remaining states (detail, accepting, declining) carry invitation + token.
  // Destructure before passing to sub-components.
  const inv =
    state.tag === "detail"
      ? state.invitation
      : (state as { invitation: InvitationDetail }).invitation;
  const tok =
    state.tag === "detail"
      ? state.token
      : (state as { token: string }).token;
  const actionErr = state.tag === "detail" ? state.actionError : "";

  return (
    <InvitationShell title="Organisation Invitation">
      <InvitationFields invitation={inv} />
      <AuthActions
        token={tok}
        state={state}
        actionError={actionErr}
        onAccept={handleAccept}
        onDecline={handleDecline}
      />
    </InvitationShell>
  );
}
