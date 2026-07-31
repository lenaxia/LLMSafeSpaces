import { useState } from "react";
import type { FormEvent } from "react";
import { Button, Input } from "../ui";
import { ApiClientError } from "../../api/client";
import { passkeyApi } from "../../api/passkey";

interface Props {
  onSuccess: (token: string) => Promise<void>;
  onCancel?: () => void;
}

export function RecoveryCodeForm({ onSuccess, onCancel }: Props) {
  const [email, setEmail] = useState("");
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setLoading(true);
    try {
      const resp = await passkeyApi.recover(email, code);
      await onSuccess(resp.token);
    } catch (err) {
      setError(
        err instanceof ApiClientError
          ? "Invalid recovery code. Please check your saved codes and try again."
          : "Recovery failed. Please try again.",
      );
    } finally {
      setLoading(false);
    }
  };

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
        placeholder="Recovery code"
        value={code}
        onChange={(e) => setCode(e.target.value)}
        required
        autoComplete="off"
        className="font-mono"
      />
      <Button type="submit" disabled={loading}>
        {loading ? "Recovering..." : "Recover account"}
      </Button>
      {onCancel && (
        <button
          type="button"
          onClick={onCancel}
          className="text-sm text-muted-foreground underline-offset-4 hover:underline"
        >
          Back to sign in
        </button>
      )}
    </form>
  );
}
