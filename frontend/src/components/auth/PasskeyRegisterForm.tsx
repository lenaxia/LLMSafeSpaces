import { useState } from "react";
import type { FormEvent } from "react";
import {
  startRegistration,
  browserSupportsWebAuthn,
} from "@simplewebauthn/browser";
import { Button, Input } from "../ui";
import { ApiClientError } from "../../api/client";
import { passkeyApi } from "../../api/passkey";

interface Props {
  onSuccess: (token: string, recoveryCodes: string[]) => Promise<void>;
}

export function PasskeyRegisterForm({ onSuccess }: Props) {
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const supported = typeof window !== "undefined" && browserSupportsWebAuthn();

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setLoading(true);
    try {
      const beginResp = await passkeyApi.registerBegin(email, name || undefined);
      const attResp = await startRegistration({ optionsJSON: beginResp.options });
      const finishResp = await passkeyApi.registerFinish(
        beginResp.sessionToken,
        email,
        attResp,
        name || undefined,
      );
      await onSuccess(finishResp.token, finishResp.recoveryCodes ?? []);
    } catch (err) {
      if (err instanceof ApiClientError) {
        setError(err.body?.error === "account already exists" ? "An account with this email already exists." : err.message);
      } else if (err instanceof Error) {
        if (err.name === "NotAllowedError") {
          setError("Passkey creation was cancelled or timed out. Please try again.");
        } else {
          setError(err.message);
        }
      } else {
        setError("Passkey registration failed. Please try again.");
      }
    } finally {
      setLoading(false);
    }
  };

  if (!supported) {
    return (
      <div className="text-center text-sm text-muted-foreground">
        <p>Your browser does not support passkeys.</p>
        <p className="mt-2">Please use a password instead.</p>
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
      <Input
        type="text"
        placeholder="Name (optional)"
        value={name}
        onChange={(e) => setName(e.target.value)}
        autoComplete="name"
      />
      <Button type="submit" disabled={loading}>
        {loading ? "Creating passkey..." : "Create account with passkey"}
      </Button>
    </form>
  );
}
