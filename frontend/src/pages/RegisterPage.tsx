import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../providers/AuthProvider";
import { authApi } from "../api/auth";
import { sanitiseReturnTo } from "../lib/returnTo";
import { AuthCard } from "../components/auth/AuthCard";
import { RegisterForm } from "../components/auth/RegisterForm";
import { PasskeyRegisterForm } from "../components/auth/PasskeyRegisterForm";
import { RecoveryCodesDisplay } from "../components/auth/RecoveryCodesDisplay";
import { Button } from "../components/ui/Button";

type Mode = "passkey" | "password" | "recovery-codes";

export function RegisterPage() {
  const { register, loginWithToken } = useAuth();
  const [returnTo, setReturnTo] = useState("");
  const [passkeyEnabled, setPasskeyEnabled] = useState(false);
  const [mode, setMode] = useState<Mode>("password");
  const [recoveryCodes, setRecoveryCodes] = useState<string[]>([]);

  useEffect(() => {
    const raw = new URLSearchParams(window.location.search).get("return_to");
    if (raw) setReturnTo(sanitiseReturnTo(raw));
    authApi.getConfig().then((c) => {
      setPasskeyEnabled(c.passkeyEnabled ?? false);
      if (c.passkeyDefaultSignup && c.passkeyEnabled) {
        setMode("passkey");
      }
    }).catch(() => {});
  }, []);

  const redirectAfterAuth = () => {
    if (returnTo) {
      window.location.href = returnTo;
    }
  };

  // Passkey registration success → show recovery codes → login → redirect
  const handlePasskeySuccess = async (token: string, codes: string[]) => {
    // Store the token immediately so the session is established even if
    // the recovery-code display page is navigated away from. But show the
    // recovery codes BEFORE loginWithToken — if me() fails (transient
    // network error), the user still sees their one-time codes. The token
    // is already in localStorage; loginWithToken on Continue is best-effort.
    localStorage.setItem("lsp_token", token);
    setRecoveryCodes(codes);
    setMode("recovery-codes");
  };

  const handleRecoveryCodesContinue = async () => {
    // Best-effort: populate the user state. If this fails (transient
    // error), the token is already in localStorage — the next page load
    // will be authenticated.
    try {
      await loginWithToken(localStorage.getItem("lsp_token") ?? "");
    } catch {
      // Non-fatal — token is stored, session will work on reload.
    }
    redirectAfterAuth();
  };

  if (mode === "recovery-codes" && recoveryCodes.length > 0) {
    return (
      <AuthCard
        title="Recovery codes"
        description="Save these before continuing"
      >
        <RecoveryCodesDisplay
          codes={recoveryCodes}
          onContinue={handleRecoveryCodesContinue}
        />
      </AuthCard>
    );
  }

  return (
    <AuthCard
      title="Create account"
      description="Get started with Safe Space"
      footer={
        <Link to={returnTo ? `/login?return_to=${encodeURIComponent(returnTo)}` : "/login"} className="text-primary underline-offset-4 hover:underline">
          Already have an account? Sign in
        </Link>
      }
    >
      {mode === "passkey" ? (
        <div className="flex flex-col gap-4">
          <PasskeyRegisterForm onSuccess={async (_token, codes) => {
            await handlePasskeySuccess(_token, codes);
          }} />
          <button
            type="button"
            onClick={() => setMode("password")}
            className="text-sm text-muted-foreground underline-offset-4 hover:underline"
          >
            Use password instead
          </button>
        </div>
      ) : (
        <div className="flex flex-col gap-4">
          <RegisterForm
            onSubmit={async (u, e, p, t) => {
              await register(u, e, p, t);
              redirectAfterAuth();
            }}
          />
          {passkeyEnabled && (
            <div className="border-t border-border pt-4">
              <p className="mb-2 text-center text-xs text-muted-foreground">or</p>
              <Button
                variant="outline"
                className="w-full"
                onClick={() => setMode("passkey")}
              >
                Sign up with passkey
              </Button>
            </div>
          )}
        </div>
      )}
    </AuthCard>
  );
}
