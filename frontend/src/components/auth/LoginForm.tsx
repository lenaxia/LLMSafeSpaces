import { useState } from "react";
import type { FormEvent } from "react";
import { Button, Input } from "../ui";
import { ApiClientError } from "../../api/client";

interface Props {
  onSubmit: (username: string, password: string, rememberMe: boolean) => Promise<void>;
  /** Fired on every email change so parents (e.g. SSO domain detection) can react. */
  onEmailChange?: (email: string) => void;
  /**
   * #1651: builds the resend-verification link shown on the 403
   * email-not-verified branch. LoginPage overrides the default to thread
   * its sanitised return_to through the verify → sign-in chain.
   */
  verifyHrefFor?: (email: string) => string;
}

const defaultVerifyHrefFor = (email: string) =>
  `/verify-email?email=${encodeURIComponent(email)}`;

export function LoginForm({ onSubmit, onEmailChange, verifyHrefFor = defaultVerifyHrefFor }: Props) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [rememberMe, setRememberMe] = useState(false);
  const [error, setError] = useState("");
  // #1651: set when the backend 403s with emailVerified:false — the
  // credentials were correct but the account isn't verified yet. The backend
  // deliberately says why (reaching this branch proves password possession),
  // so the UI names the reason and offers a prefilled resend link.
  const [needsVerification, setNeedsVerification] = useState(false);
  const [loading, setLoading] = useState(false);

  const handleEmail = (value: string) => {
    setUsername(value);
    onEmailChange?.(value);
  };

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNeedsVerification(false);
    setLoading(true);
    try {
      await onSubmit(username, password, rememberMe);
    } catch (err) {
      if (
        err instanceof ApiClientError &&
        err.status === 403 &&
        err.body?.emailVerified === false
      ) {
        setNeedsVerification(true);
      } else {
        setError(err instanceof ApiClientError ? "Invalid email or password" : "Something went wrong");
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <form onSubmit={handleSubmit} className="flex flex-col gap-4">
      {error && <p className="text-sm text-destructive">{error}</p>}
      {needsVerification && (
        <div className="text-sm">
          <p className="text-destructive">
            Please verify your email address before logging in.
          </p>
          <p className="mt-1">
            <a
              href={verifyHrefFor(username)}
              className="text-primary underline underline-offset-4"
            >
              Resend verification email
            </a>
          </p>
        </div>
      )}
      <Input
        type="email"
        placeholder="Email"
        value={username}
        onChange={(e) => handleEmail(e.target.value)}
        required
        autoComplete="email"
      />
      <Input
        type="password"
        placeholder="Password"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        required
        autoComplete="current-password"
      />
      <label className="flex items-center gap-2 text-sm select-none cursor-pointer">
        <input
          type="checkbox"
          checked={rememberMe}
          onChange={(e) => setRememberMe(e.target.checked)}
          className="h-4 w-4 rounded border-border accent-primary cursor-pointer"
        />
        Remember me for 30 days
      </label>
      <Button type="submit" disabled={loading}>
        {loading ? "Signing in..." : "Sign in"}
      </Button>
    </form>
  );
}
