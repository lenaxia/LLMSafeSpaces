import { useEffect, useState, useCallback } from "react";
import { useSearchParams } from "react-router-dom";
import { Button } from "../ui/Button";
import { passkeyApi } from "../../api/passkey";
import type { PasskeyListItem } from "../../api/passkey";
import { RecoveryCodesDisplay } from "../auth/RecoveryCodesDisplay";
import { ApiClientError } from "../../api/client";

export function PasskeySettings() {
  const [searchParams] = useSearchParams();
  const [passkeys, setPasskeys] = useState<PasskeyListItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [newRecoveryCodes, setNewRecoveryCodes] = useState<string[] | null>(null);
  const mustEnroll = searchParams.get("must_enroll_passkey") === "1";

  const fetchPasskeys = useCallback(async () => {
    setLoading(true);
    try {
      const resp = await passkeyApi.listPasskeys();
      setPasskeys(resp.passkeys ?? []);
    } catch {
      setError("Failed to load passkeys");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetchPasskeys(); }, [fetchPasskeys]);

  const handleDelete = async (id: string) => {
    setError("");
    try {
      await passkeyApi.deletePasskey(id);
      await fetchPasskeys();
    } catch (err) {
      if (err instanceof ApiClientError && err.status === 409) {
        setError("Cannot delete your last passkey. Add another one first.");
      } else {
        setError("Failed to delete passkey");
      }
    }
  };

  const handleRegenerate = async () => {
    setError("");
    try {
      const resp = await passkeyApi.regenerateRecoveryCodes();
      setNewRecoveryCodes(resp.recoveryCodes);
    } catch {
      setError("Failed to regenerate recovery codes");
    }
  };

  if (newRecoveryCodes) {
    return (
      <RecoveryCodesDisplay
        codes={newRecoveryCodes}
        onContinue={() => setNewRecoveryCodes(null)}
      />
    );
  }

  return (
    <div className="space-y-4">
      {mustEnroll && passkeys.length === 0 && (
        <div className="rounded-lg border border-yellow-500/50 bg-yellow-500/10 p-4">
          <p className="text-sm font-medium text-yellow-600 dark:text-yellow-400">
            You used a recovery code to sign in. Please enroll a new passkey to regain passwordless access.
          </p>
        </div>
      )}
      {error && <p className="text-sm text-destructive">{error}</p>}
      <div>
        <h3 className="mb-2 text-lg font-semibold">Passkeys</h3>
        {loading ? (
          <p className="text-sm text-muted-foreground">Loading...</p>
        ) : passkeys.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No passkeys registered. {mustEnroll ? "Enroll one to continue." : ""}
          </p>
        ) : (
          <ul className="space-y-2">
            {passkeys.map((pk) => (
              <li key={pk.id} className="flex items-center justify-between rounded-md border border-border p-3">
                <div>
                  <p className="text-sm font-medium">{pk.name || "Unnamed passkey"}</p>
                  <p className="text-xs text-muted-foreground">
                    Added {new Date(pk.createdAt).toLocaleDateString()}
                    {pk.lastUsedAt && ` · Last used ${new Date(pk.lastUsedAt).toLocaleDateString()}`}
                  </p>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => handleDelete(pk.id)}
                >
                  Remove
                </Button>
              </li>
            ))}
          </ul>
        )}
      </div>
      <div className="border-t border-border pt-4">
        <Button variant="outline" onClick={handleRegenerate}>
          Regenerate recovery codes
        </Button>
      </div>
    </div>
  );
}
