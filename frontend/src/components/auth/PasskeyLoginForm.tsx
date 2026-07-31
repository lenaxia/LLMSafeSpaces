import { useState } from "react";
import type { FormEvent } from "react";
import {
  startAuthentication,
  browserSupportsWebAuthn,
} from "@simplewebauthn/browser";
import { Button, Input } from "../ui";
import { ApiClientError } from "../../api/client";
import { passkeyApi } from "../../api/passkey";

interface Props {
  onSuccess: (token: string) => Promise<void>;
  onUsePassword?: () => void;
  onRecover?: () => void;
}

export function PasskeyLoginForm({ onSuccess, onUsePassword, onRecover }: Props) {
  const [email, setEmail] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const supported = typeof window !== "undefined" && browserSupportsWebAuthn();

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setLoading(true);
    try {
      const beginResp = await passkeyApi.loginBegin(email);
      const assertResp = await startAuthentication({ optionsJSON: beginResp.options });
      const finishResp = await passkeyApi.loginFinish(beginResp.sessionToken, email, assertResp);
      await onSuccess(finishResp.token);
    } catch (err) {
      if (err instanceof ApiClientError) {
        setError(err.body?.error === "no passkey registered for this account"
          ? "No passkey is registered for this account. Use your password instead."
          : err.message);
      } else if (err instanceof Error) {
        if (err.name === "NotAllowedError") {
          setError("Authentication was cancelled or timed out. Please try again.");
        } else {
          setError(err.message);
        }
      } else {
        setError("Passkey login failed. Please try again.");
      }
    } finally {
      setLoading(false);
    }
  };

  if (!supported) {
    return (
      <div className="flex flex-col gap-4">
        <p className="text-center text-sm text-muted-foreground">
          Your browser does not support passkeys.
        </p>
        {onUsePassword && (
          <Button variant="outline" onClick={onUsePassword}>
            Use password instead
          </Button>
        )}
      </div>
    );
  }

  return (
    <form onSubmit={handleSubmit} className="flex flex-col gap-4">
      {error && <p className="text-sm text-destructive">{error}</p>}
      <Input
        type="email"
        placeholder="Email"
        value={email}
        onChange={(e) => setEmail(e.target.value)}
        required
        autoComplete="email"
      />
      <Button type="submit" disabled={loading}>
        {loading ? "Authenticating..." : "Sign in with passkey"}
      </Button>
      {onUsePassword && (
        <button
          type="button"
          onClick={onUsePassword}
          className="text-sm text-muted-foreground underline-offset-4 hover:underline"
        >
          Use password instead
        </button>
      )}
      {onRecover && (
        <button
          type="button"
          onClick={onRecover}
          className="text-xs text-muted-foreground underline-offset-4 hover:underline"
        >
          Lost your passkey? Use a recovery code
        </button>
      )}
    </form>
  );
}
