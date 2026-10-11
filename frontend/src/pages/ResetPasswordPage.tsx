import { useState } from "react";
import type { FormEvent } from "react";
import { Link } from "react-router-dom";
import { authApi } from "../api/auth";
import { ApiClientError } from "../api/client";
import { classifyTokenError, stripQueryParam } from "../lib/tokenResult";
import { AuthCard } from "../components/auth/AuthCard";
import { Button, Input } from "../components/ui";

/**
 * #1651: two-state interstitial for the password-reset flow.
 *
 * Request mode (/reset-password, entered from the login page's "Forgot your
 * password?"): collects an email and POSTs to the always-202 request
 * endpoint. The confirmation copy is fixed — the backend never reveals
 * whether the account exists, and neither does the UI.
 *
 * Confirm mode (/reset-password?token=…, from the emailed link): collects a
 * new password + confirmation and POSTs once. Success redirects to
 * /login?reset=success (the login page shows the success notice); expired or
 * already-used tokens offer a fresh request.
 */

type RequestState =
  | { tag: "form" }
  | { tag: "sending" }
  | { tag: "sent" };

type ConfirmState =
  | { tag: "form" }
  | { tag: "submitting" }
  | { tag: "done" }
  | { tag: "expired" }
  | { tag: "used" }
  | { tag: "not-found" }
  | { tag: "gone" }
  | { tag: "error" };

export function ResetPasswordPage() {
  const initialToken = new URLSearchParams(window.location.search).get("token");
  const [mode, setMode] = useState<"request" | "confirm">(
    initialToken ? "confirm" : "request",
  );
  const [token] = useState(initialToken);

  // Request-mode state
  const [email, setEmail] = useState("");
  const [request, setRequest] = useState<RequestState>({ tag: "form" });
  const [requestError, setRequestError] = useState("");

  // Confirm-mode state
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [confirm, setConfirm] = useState<ConfirmState>({ tag: "form" });
  const [confirmError, setConfirmError] = useState("");

  const submitRequest = (e: FormEvent) => {
    e.preventDefault();
    setRequestError("");
    setRequest({ tag: "sending" });
    authApi
      .requestPasswordReset(email)
      .then(() => setRequest({ tag: "sent" }))
      .catch((err) => {
        setRequest({ tag: "form" });
        if (err instanceof ApiClientError && err.status === 429) {
          setRequestError("Too many attempts. Please try again in a minute.");
        } else {
          setRequestError("Something went wrong. Please try again.");
        }
      });
  };

  const submitConfirm = (e: FormEvent) => {
    e.preventDefault();
    setConfirmError("");
    if (newPassword.length < 8) {
      setConfirmError("Password must be at least 8 characters.");
      return;
    }
    if (newPassword !== confirmPassword) {
      setConfirmError("Passwords do not match.");
      return;
    }
    doConfirm();
  };

  const doConfirm = () => {
    setConfirm({ tag: "submitting" });
    authApi
      .confirmPasswordReset(token ?? "", newPassword)
      .then(() => {
        stripQueryParam("token");
        setConfirm({ tag: "done" });
      })
      .catch((err) => {
        const failure = classifyTokenError(err);
        if (failure !== "error") stripQueryParam("token");
        setConfirm({ tag: failure });
      });
  };

  // === Request mode ===
  if (mode === "request") {
    return (
      <AuthCard title="Reset your password" description="Enter your account email">
        {request.tag === "sent" ? (
          <div className="flex flex-col gap-4">
            <p className="text-sm text-muted-foreground" data-testid="reset-sent">
              If an account exists for that email, a password reset link has
              been sent. Please check your inbox.
            </p>
            <p className="text-center text-xs text-muted-foreground">
              Remembered it?{" "}
              <Link to="/login" className="text-primary underline-offset-4 hover:underline">
                Sign in
              </Link>
            </p>
          </div>
        ) : (
          <form onSubmit={submitRequest} className="flex flex-col gap-4">
            {requestError && <p className="text-sm text-destructive">{requestError}</p>}
            <p className="text-sm text-muted-foreground">
              We'll send a password reset link to your email address. Reset
              links are single-use and expire after 15 minutes.
            </p>
            <Input
              type="email"
              placeholder="Email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              required
              autoComplete="email"
            />
            <Button type="submit" disabled={request.tag === "sending"}>
              {request.tag === "sending" ? "Sending..." : "Send reset link"}
            </Button>
            <p className="text-center text-xs text-muted-foreground">
              Remembered it?{" "}
              <Link to="/login" className="text-primary underline-offset-4 hover:underline">
                Sign in
              </Link>
            </p>
          </form>
        )}
      </AuthCard>
    );
  }

  // === Confirm mode ===
  if (confirm.tag === "done") {
    return (
      <AuthCard title="Password reset" description="Your password has been updated">
        <p className="mb-4 text-sm text-muted-foreground">
          Your password has been reset. All previous sessions were signed out —
          sign in with your new password.
        </p>
        <Link
          to="/login?reset=success"
          className="block w-full text-center text-primary underline-offset-4 hover:underline"
        >
          Sign in with your new password
        </Link>
      </AuthCard>
    );
  }

  if (
    confirm.tag === "expired" ||
    confirm.tag === "not-found" ||
    confirm.tag === "gone"
  ) {
    return (
      <AuthCard title="Reset your password" description="Reset link no longer valid">
        <p className="mb-4 text-sm text-muted-foreground">
          {confirm.tag === "expired"
            ? "This reset link has expired. Reset links are single-use and expire after 15 minutes."
            : "This reset link is invalid or incomplete."}
        </p>
        <Button
          className="w-full"
          onClick={() => {
            setMode("request");
            setConfirm({ tag: "form" });
          }}
        >
          Request a new link
        </Button>
      </AuthCard>
    );
  }

  if (confirm.tag === "used") {
    return (
      <AuthCard title="Reset your password" description="Reset link already used">
        <p className="mb-4 text-sm text-muted-foreground">
          This reset link has already been used. If you still need to reset
          your password, request a new link.
        </p>
        <div className="flex flex-col gap-3">
          <Button
            className="w-full"
            onClick={() => {
              setMode("request");
              setConfirm({ tag: "form" });
            }}
          >
            Request a new link
          </Button>
          <p className="text-center text-xs text-muted-foreground">
            Already reset it?{" "}
            <Link to="/login" className="text-primary underline-offset-4 hover:underline">
              Sign in
            </Link>
          </p>
        </div>
      </AuthCard>
    );
  }

  if (confirm.tag === "error") {
    return (
      <AuthCard title="Reset your password" description="Password reset">
        <p className="mb-4 text-sm text-muted-foreground">
          Something went wrong while resetting your password. Please try again.
        </p>
        <Button className="w-full" onClick={doConfirm}>
          Try again
        </Button>
      </AuthCard>
    );
  }

  // form | submitting
  return (
    <AuthCard title="Reset your password" description="Choose a new password">
      <form onSubmit={submitConfirm} className="flex flex-col gap-4">
        {confirmError && <p className="text-sm text-destructive">{confirmError}</p>}
        <Input
          type="password"
          placeholder="New password"
          value={newPassword}
          onChange={(e) => setNewPassword(e.target.value)}
          required
          minLength={8}
          autoComplete="new-password"
        />
        <Input
          type="password"
          placeholder="Confirm new password"
          value={confirmPassword}
          onChange={(e) => setConfirmPassword(e.target.value)}
          required
          minLength={8}
          autoComplete="new-password"
        />
        <Button type="submit" disabled={confirm.tag === "submitting"}>
          {confirm.tag === "submitting" ? "Resetting..." : "Reset password"}
        </Button>
      </form>
    </AuthCard>
  );
}
