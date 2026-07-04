import { useState, useCallback } from "react";
import type { FormEvent } from "react";
import { Button, Input } from "../ui";
import { ApiClientError } from "../../api/client";
import { getEnv } from "../../env";
import { TurnstileWidget } from "./TurnstileWidget";

interface Props {
  // onSubmit accepts turnstileToken as a 4th positional arg. When
  // Turnstile is disabled (siteKey empty), the token is "" and the
  // API middleware ignores it. When enabled, the widget provides a
  // real token before the submit button unlocks.
  onSubmit: (
    username: string,
    email: string,
    password: string,
    turnstileToken?: string,
  ) => Promise<void>;
}

export function RegisterForm({ onSubmit }: Props) {
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [turnstileToken, setTurnstileToken] = useState("");

  const { turnstileSiteKey } = getEnv();
  const turnstileEnabled = turnstileSiteKey.length > 0;
  // Submit is blocked when Turnstile is enabled but hasn't produced a
  // token yet. The widget's managed mode resolves invisibly for most
  // users; only bot-suspected sessions see an interactive challenge.
  const submitBlocked = turnstileEnabled && !turnstileToken;

  const handleTurnstileToken = useCallback((token: string) => {
    setTurnstileToken(token);
  }, []);

  const handleTurnstileExpire = useCallback(() => {
    // Tokens live ~5min. On expiry, clear so the user re-challenges
    // before another submit attempt. The widget auto-refreshes in
    // managed mode; this state clear makes the UI reflect that.
    setTurnstileToken("");
  }, []);

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setLoading(true);
    try {
      await onSubmit(username, email, password, turnstileToken);
    } catch (err) {
      setError(err instanceof ApiClientError ? err.message : "Something went wrong");
      // On backend-side turnstile_failed, the current token is invalid
      // — clear it so the user re-challenges.
      if (err instanceof ApiClientError && err.body?.error === "turnstile_failed") {
        setTurnstileToken("");
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <form onSubmit={handleSubmit} className="flex flex-col gap-4">
      {error && <p className="text-sm text-destructive">{error}</p>}
      <Input
        type="text"
        placeholder="Username"
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        required
        autoComplete="username"
      />
      <Input
        type="email"
        placeholder="Email"
        value={email}
        onChange={(e) => setEmail(e.target.value)}
        required
        autoComplete="email"
      />
      <Input
        type="password"
        placeholder="Password"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        required
        minLength={8}
        autoComplete="new-password"
      />
      {turnstileEnabled && (
        <TurnstileWidget
          siteKey={turnstileSiteKey}
          onToken={handleTurnstileToken}
          onExpire={handleTurnstileExpire}
          onError={() => setTurnstileToken("")}
        />
      )}
      <Button type="submit" disabled={loading || submitBlocked}>
        {loading ? "Creating account..." : "Create account"}
      </Button>
    </form>
  );
}
