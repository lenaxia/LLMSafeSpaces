import { useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { Link } from "react-router-dom";
import { authApi } from "../api/auth";
import { ApiClientError } from "../api/client";
import { sanitiseReturnTo } from "../lib/returnTo";
import { classifyTokenError, stripQueryParam } from "../lib/tokenResult";
import { AuthCard } from "../components/auth/AuthCard";
import { Button, Input } from "../components/ui";
import { Spinner } from "../components/ui/Spinner";

/**
 * #1651: interstitial for the emailed verification link
 * (/verify-email?token=…). The token is consumed via POST on mount — never a
 * consuming GET (US-49.9 scanner-defense invariant) — and every terminal
 * outcome has its own guidance. This page is the difference between a
 * locked-out registrant (login is 403-gated until verified) and a first
 * successful login.
 */
type VerifyState =
  | { tag: "missing" }
  | { tag: "verifying" }
  | { tag: "verified" }
  | { tag: "expired" }
  | { tag: "used" }
  | { tag: "not-found" }
  | { tag: "gone" }
  | { tag: "error" };

type ResendState =
  | { tag: "idle" }
  | { tag: "sending" }
  | { tag: "sent" }
  | { tag: "rate-limited" }
  | { tag: "error" };

export function VerifyEmailPage() {
  const [state, setState] = useState<VerifyState>({ tag: "verifying" });
  const [resend, setResend] = useState<ResendState>({ tag: "idle" });
  // Email context for the resend form: prefilled from ?email= when the link
  // producer knew it (login 403 flow, register check-email card), else typed.
  const [resendEmail, setResendEmail] = useState(
    () => new URLSearchParams(window.location.search).get("email") ?? "",
  );
  const [loginHref, setLoginHref] = useState("/login");

  // The token is read once from the URL and kept in state: terminal states
  // strip it from the address bar (refresh-safety for a single-use token),
  // but the error state's "Try again" still needs it.
  const tokenRef = useRef<string | null>(null);
  // StrictMode (dev) double-invokes the mount effect; the ref guard ensures
  // the token is consumed exactly once per page visit.
  const startedRef = useRef(false);

  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const token = params.get("token");
    tokenRef.current = token;
    const returnTo = sanitiseReturnTo(params.get("return_to") ?? "");
    if (returnTo) {
      setLoginHref(`/login?return_to=${encodeURIComponent(returnTo)}`);
    }
    if (!token) {
      setState({ tag: "missing" });
      return;
    }
    if (startedRef.current) return;
    startedRef.current = true;
    consume(token);
  }, []);

  const consume = (token: string) => {
    setState({ tag: "verifying" });
    authApi
      .verifyEmail(token)
      .then(() => {
        stripQueryParam("token");
        setState({ tag: "verified" });
      })
      .catch((err) => {
        const failure = classifyTokenError(err);
        if (failure !== "error") stripQueryParam("token");
        setState({ tag: failure });
      });
  };

  const handleResend = (e: FormEvent) => {
    e.preventDefault();
    setResend({ tag: "sending" });
    authApi
      .resendVerification(resendEmail)
      .then(() => setResend({ tag: "sent" }))
      .catch((err) => {
        if (err instanceof ApiClientError && err.status === 429) {
          setResend({ tag: "rate-limited" });
        } else {
          setResend({ tag: "error" });
        }
      });
  };

  // --- Missing / invalid token ---
  if (state.tag === "missing" || state.tag === "not-found") {
    return (
      <AuthCard title="Verify your email" description="Email verification">
        <ResendForm
          intro="This verification link is invalid or incomplete. Verification links are single-use and expire after 24 hours."
          resend={resend}
          email={resendEmail}
          onEmailChange={setResendEmail}
          onSubmit={handleResend}
          loginHref={loginHref}
        />
      </AuthCard>
    );
  }

  if (state.tag === "verifying") {
    return (
      <div className="flex min-h-dvh items-center justify-center" aria-label="Verifying your email">
        <Spinner size="lg" />
      </div>
    );
  }

  if (state.tag === "verified") {
    return (
      <AuthCard title="Email verified" description="Your email address has been verified">
        <p className="mb-4 text-sm text-muted-foreground">
          Your email address is now verified. You can sign in to your account.
        </p>
        <Link
          to={loginHref}
          className="block w-full text-center text-primary underline-offset-4 hover:underline"
        >
          Continue to sign in
        </Link>
      </AuthCard>
    );
  }

  if (state.tag === "expired") {
    return (
      <AuthCard title="Verify your email" description="Verification link expired">
        <ResendForm
          intro="This verification link has expired. Verification links are single-use and expire after 24 hours. Enter your email to receive a new link."
          resend={resend}
          email={resendEmail}
          onEmailChange={setResendEmail}
          onSubmit={handleResend}
          loginHref={loginHref}
        />
      </AuthCard>
    );
  }

  if (state.tag === "used") {
    // The token was consumed — the email IS verified. Guide to sign-in; a
    // resend is pointless here (and the backend no-ops for verified users).
    return (
      <AuthCard title="Verify your email" description="Link already used">
        <p className="mb-4 text-sm text-muted-foreground">
          This verification link has already been used. Your email address is
          verified — try signing in.
        </p>
        <Link
          to={loginHref}
          className="text-primary underline-offset-4 hover:underline"
        >
          Sign in
        </Link>
      </AuthCard>
    );
  }

  if (state.tag === "gone") {
    return (
      <AuthCard title="Verify your email" description="Link no longer valid">
        <p className="mb-4 text-sm text-muted-foreground">
          This verification link is no longer valid.
        </p>
        <ResendForm
          intro="If you still need to verify your email, enter it to receive a new link."
          resend={resend}
          email={resendEmail}
          onEmailChange={setResendEmail}
          onSubmit={handleResend}
          loginHref={loginHref}
        />
      </AuthCard>
    );
  }

  // state.tag === "error" — transport failure; the token is NOT terminal
  // (still in the URL), so offer a retry.
  return (
    <AuthCard title="Verify your email" description="Email verification">
      <p className="mb-4 text-sm text-muted-foreground">
        Something went wrong while verifying your email. Please try again.
      </p>
      <Button
        className="w-full"
        onClick={() => {
          const token = tokenRef.current;
          if (token) consume(token);
        }}
      >
        Try again
      </Button>
    </AuthCard>
  );
}

/**
 * Resend sub-form shared by the non-terminal-verdict states. The copy is
 * deliberately neutral: resend is always-202 server-side, and the client
 * must not imply anything about account existence.
 */
function ResendForm({
  intro,
  resend,
  email,
  onEmailChange,
  onSubmit,
  loginHref,
}: {
  intro: string;
  resend: ResendState;
  email: string;
  onEmailChange: (v: string) => void;
  onSubmit: (e: FormEvent) => void;
  loginHref: string;
}) {
  return (
    <div className="flex flex-col gap-4">
      <p className="text-sm text-muted-foreground">{intro}</p>
      {resend.tag === "sent" ? (
        <p className="text-sm text-muted-foreground" data-testid="resend-sent">
          If that email needs verification, a new link has been sent. Please
          check your inbox.
        </p>
      ) : (
        <form onSubmit={onSubmit} className="flex flex-col gap-4">
          {resend.tag === "rate-limited" && (
            <p className="text-sm text-destructive">
              Too many attempts. Please try again in a minute.
            </p>
          )}
          {resend.tag === "error" && (
            <p className="text-sm text-destructive">
              Something went wrong. Please try again.
            </p>
          )}
          <Input
            type="email"
            placeholder="Email"
            value={email}
            onChange={(e) => onEmailChange(e.target.value)}
            required
            autoComplete="email"
          />
          <Button type="submit" disabled={resend.tag === "sending"}>
            {resend.tag === "sending" ? "Sending..." : "Send new link"}
          </Button>
        </form>
      )}
      <p className="text-center text-xs text-muted-foreground">
        Already verified?{" "}
        <Link to={loginHref} className="text-primary underline-offset-4 hover:underline">
          Sign in
        </Link>
      </p>
    </div>
  );
}
