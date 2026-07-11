import { useEffect, useState } from "react";
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
      // Non-fatal action error shown inline — user can retry accept/decline.
      actionError: string;
    }
  | { tag: "accepting" }
  | { tag: "accepted"; orgName: string }
  | { tag: "declining" }
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

export function InvitationPage() {
  const { token } = useParams<{ token: string }>();
  const { user, loading: authLoading } = useAuth();
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
    setState({ tag: "accepting" });
    try {
      await orgsApi.acceptInvitation(tok);
      setState({ tag: "accepted", orgName: inv.orgName });
    } catch (err) {
      if (err instanceof ApiClientError) {
        switch (err.status) {
          case 403:
            // Wrong email — user signed in with a different account than the
            // one the invitation was sent to. Show inline so they can log out
            // and re-authenticate with the correct account.
            setState({
              tag: "detail",
              invitation: inv,
              token: tok,
              actionError: "This invitation was sent to a different email address. Sign in with the invited email to accept.",
            });
            return;
          case 409:
            setState({ tag: "alreadyHandled", reason: err.body?.error ?? "You have already accepted or declined this invitation." });
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
    setState({ tag: "declining" });
    try {
      await orgsApi.declineInvitation(tok);
      setState({ tag: "declined" });
    } catch (err) {
      const msg = err instanceof ApiClientError
        ? (err.body?.error ?? "Failed to decline invitation.")
        : "Something went wrong. Please try again.";
      setState({ tag: "detail", invitation: inv, token: tok, actionError: msg });
    }
  };

  // --- Auth-loading / initial-loading ---
  if (state.tag === "loading" || authLoading) {
    return (
      <div className="flex min-h-screen items-center justify-center">
        <Spinner size="lg" />
      </div>
    );
  }

  // --- Shared card shell ---
  const Shell = ({ title, children }: { title: string; children: React.ReactNode }) => (
    <div className="flex min-h-screen flex-col items-center justify-center gap-4 px-4">
      <div className="w-full max-w-md rounded border border-border bg-card p-6">
        <h1 className="mb-4 text-xl font-semibold">{title}</h1>
        {children}
      </div>
    </div>
  );

  // --- Fatal error ---
  if (state.tag === "error") {
    return (
      <Shell title="Invitation">
        <p className="text-muted-foreground">{state.message}</p>
        <Link to="/chat" className="mt-4 inline-block">
          <Button variant="outline">Go to chat</Button>
        </Link>
      </Shell>
    );
  }

  // --- Already accepted / declined ---
  if (state.tag === "alreadyHandled") {
    return (
      <Shell title="Invitation">
        <p className="text-muted-foreground">{state.reason}</p>
        <Link to="/chat" className="mt-4 inline-block">
          <Button variant="outline">Go to chat</Button>
        </Link>
      </Shell>
    );
  }

  // --- Accepted ---
  if (state.tag === "accepted") {
    return (
      <Shell title="Invitation accepted!">
        <p className="text-muted-foreground">
          You are now a member of <span className="font-medium">{state.orgName}</span>.
        </p>
        <Link to="/chat" className="mt-4 inline-block">
          <Button>Go to chat</Button>
        </Link>
      </Shell>
    );
  }

  // --- Declined ---
  if (state.tag === "declined") {
    return (
      <Shell title="Invitation declined">
        <p className="text-muted-foreground">You have declined this invitation.</p>
        <Link to="/chat" className="mt-4 inline-block">
          <Button variant="outline">Go to chat</Button>
        </Link>
      </Shell>
    );
  }

  // --- Detail (main state) ---
  const s = state as Extract<PageState, { tag: "detail" }>;
  const { invitation, token: invToken, actionError } = s;
  const isActing = state.tag !== "detail" && (state.tag === "accepting" || state.tag === "declining");

  return (
    <Shell title="Organisation Invitation">
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

      {user ? (
        <>
          {actionError && (
            <p className="mt-3 text-sm text-red-500">{actionError}</p>
          )}
          <div className="mt-4 flex gap-3">
            <Button
              variant="default"
              className="flex-1"
              onClick={handleAccept}
              disabled={isActing}
            >
              Accept
            </Button>
            <Button
              variant="outline"
              className="flex-1"
              onClick={handleDecline}
              disabled={isActing}
            >
              Decline
            </Button>
          </div>
        </>
      ) : (
        <div className="mt-6 text-center">
          <p className="mb-3 text-sm text-muted-foreground">
            Sign in or create an account to accept this invitation.
          </p>
          <div className="flex gap-3">
            <Link
              to={`/login?return_to=${encodeURIComponent(`/invitations/${invToken}`)}`}
              className="flex-1"
            >
              <Button variant="default" className="w-full">
                Sign in
              </Button>
            </Link>
            <Link
              to={`/register?return_to=${encodeURIComponent(`/invitations/${invToken}`)}`}
              className="flex-1"
            >
              <Button variant="outline" className="w-full">
                Create account
              </Button>
            </Link>
          </div>
        </div>
      )}
    </Shell>
  );
}
