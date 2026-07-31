import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../providers/AuthProvider";
import { authApi } from "../api/auth";
import { ssoApi, ssoRedirectURL, type SSODomain } from "../api/sso";
import { sanitiseReturnTo } from "../lib/returnTo";
import { AuthCard } from "../components/auth/AuthCard";
import { LoginForm } from "../components/auth/LoginForm";
import { PasskeyLoginForm } from "../components/auth/PasskeyLoginForm";
import { RecoveryCodeForm } from "../components/auth/RecoveryCodeForm";
import { Button } from "../components/ui/Button";
import { ApiClientError } from "../api/client";

type LoginMode = "passkey" | "password" | "recovery";

export function LoginPage() {
  const { login, loginWithToken } = useAuth();
  const [registrationEnabled, setRegistrationEnabled] = useState(false);
  const [instanceName, setInstanceName] = useState("Safe Space");
  const [motd, setMotd] = useState("");
  const [email, setEmail] = useState("");
  const [domains, setDomains] = useState<SSODomain[]>([]);
  const [returnTo, setReturnTo] = useState("");
  const [ssoStatus, setSsoStatus] = useState<string | null>(null);
  const [lookupStatus, setLookupStatus] = useState<string | null>(null);
  const [lookingUp, setLookingUp] = useState(false);
  const [passkeyEnabled, setPasskeyEnabled] = useState(false);
  const [mode, setMode] = useState<LoginMode>("password");

  useEffect(() => {
    authApi.getConfig().then((c) => {
      setRegistrationEnabled(c.registrationEnabled);
      if (c.instanceName) setInstanceName(c.instanceName);
      if (c.motd) setMotd(c.motd);
      setPasskeyEnabled(c.passkeyEnabled ?? false);
      if (c.passkeyEnabled) {
        setMode("passkey");
      }
      if (c.oidcEnabled) {
        ssoApi.domains().then((r) => setDomains(r.domains)).catch(() => {});
      }
    }).catch(() => {});
  }, []);

  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const sso = params.get("sso");
    if (sso) {
      setSsoStatus(sso);
      params.delete("sso");
    }
    const lookup = params.get("lookup");
    if (lookup) {
      setLookupStatus(lookup);
      params.delete("lookup");
    }
    const rt = params.get("return_to");
    if (rt) {
      setReturnTo(sanitiseReturnTo(rt));
      params.delete("return_to");
    }
    const clean = params.toString();
    window.history.replaceState({}, "", clean ? `?${clean}` : window.location.pathname);
  }, []);

  const redirectAfterAuth = () => {
    if (returnTo) {
      window.location.href = returnTo;
    }
  };

  const matchedDomain = domains.find((d) => email.toLowerCase().endsWith(d.domain.toLowerCase()));
  const emailLooksValid = email.includes("@") && email.split("@")[1]?.includes(".");
  const showDiscoveryButton = domains.length > 0 && !matchedDomain && emailLooksValid;

  const handleDiscovery = async () => {
    setLookingUp(true);
    setLookupStatus(null);
    try {
      const { redirectUrl } = await authApi.lookup(email);
      if (redirectUrl.includes("lookup=not_found")) {
        setLookupStatus("not_found");
        setLookingUp(false);
        return;
      }
      window.location.href = redirectUrl;
    } catch (err) {
      if (err instanceof ApiClientError && err.status === 429) {
        setLookupStatus("rate_limited");
      } else {
        setLookupStatus("error");
      }
      setLookingUp(false);
    }
  };

  const ssoErrorBanner = ssoStatus && ssoStatus !== "success" ? (
    <p className="mb-3 text-sm text-red-500">
      {ssoStatus === "provisioning_disabled"
        ? "Your account is not provisioned. Contact your administrator."
        : ssoStatus === "suspended"
          ? "Your account is suspended."
          : ssoStatus === "state_invalid"
            ? "Single sign-on session expired or was invalid. Please try again."
            : ssoStatus === "config_error"
              ? "Single sign-on is not configured on this instance. Please contact your administrator."
              : "Single sign-in failed. Please try again."}
    </p>
  ) : null;

  const footer = registrationEnabled ? (
    <Link to={returnTo ? `/register?return_to=${encodeURIComponent(returnTo)}` : "/register"} className="text-primary underline-offset-4 hover:underline">
      Create an account
    </Link>
  ) : undefined;

  // === Recovery mode ===
  if (mode === "recovery") {
    return (
      <AuthCard title={`Welcome to ${instanceName}`} description="Recover your account">
        <RecoveryCodeForm
          onSuccess={async (token) => {
            await loginWithToken(token);
            redirectAfterAuth();
          }}
          onCancel={() => setMode(passkeyEnabled ? "passkey" : "password")}
        />
      </AuthCard>
    );
  }

  return (
    <AuthCard
      title={`Welcome to ${instanceName}`}
      description={motd || "Sign in to your account"}
      footer={footer}
    >
      {ssoErrorBanner}
      {lookupStatus === "not_found" && (
        <p className="mb-3 text-sm text-red-500">
          We couldn't find an account for that email. Try a different email, or{" "}
          {registrationEnabled ? (
            <Link to={returnTo ? `/register?return_to=${encodeURIComponent(returnTo)}` : "/register"} className="underline underline-offset-4">
              create an account
            </Link>
          ) : (
            "contact your administrator"
          )}
          .
        </p>
      )}
      {lookupStatus === "rate_limited" && (
        <p className="mb-3 text-sm text-red-500">
          Too many attempts. Please try again in a minute.
        </p>
      )}
      {lookupStatus === "error" && (
        <p className="mb-3 text-sm text-red-500">
          Something went wrong. Please try again.
        </p>
      )}

      {mode === "passkey" ? (
        <div className="flex flex-col gap-4">
          <PasskeyLoginForm
            onSuccess={async (token) => {
              await loginWithToken(token);
              redirectAfterAuth();
            }}
            onUsePassword={() => setMode("password")}
            onRecover={() => setMode("recovery")}
          />
        </div>
      ) : (
        <div className="flex flex-col gap-4">
          <LoginForm
            onSubmit={async (u, p, r) => {
              await login(u, p, r);
              redirectAfterAuth();
            }}
            onEmailChange={setEmail}
          />
          {passkeyEnabled && (
            <div className="border-t border-border pt-4">
              <p className="mb-2 text-center text-xs text-muted-foreground">or</p>
              <Button
                variant="outline"
                className="w-full"
                onClick={() => setMode("passkey")}
              >
                Sign in with passkey
              </Button>
            </div>
          )}
          <button
            type="button"
            onClick={() => setMode("recovery")}
            className="text-center text-xs text-muted-foreground underline-offset-4 hover:underline"
          >
            Lost your passkey? Use a recovery code
          </button>
        </div>
      )}

      {matchedDomain && (
        <div className="mt-4 border-t border-border pt-4">
          <p className="mb-2 text-center text-xs text-muted-foreground">or</p>
          <Button
            variant="outline"
            className="w-full"
            onClick={() => {
              window.location.href = ssoRedirectURL(matchedDomain.orgSlug);
            }}
          >
            Sign in with {matchedDomain.orgName}
          </Button>
        </div>
      )}
      {showDiscoveryButton && (
        <div className="mt-4 border-t border-border pt-4">
          <p className="mb-2 text-center text-xs text-muted-foreground">or</p>
          <Button
            variant="outline"
            className="w-full"
            disabled={lookingUp}
            onClick={handleDiscovery}
          >
            {lookingUp ? "Looking up..." : "Continue with email"}
          </Button>
        </div>
      )}
    </AuthCard>
  );
}
