import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useAuth } from "../providers/AuthProvider";
import { authApi } from "../api/auth";
import { ssoApi, ssoRedirectURL, type SSODomain } from "../api/sso";
import { sanitiseReturnTo } from "../lib/returnTo";
import { AuthCard } from "../components/auth/AuthCard";
import { LoginForm } from "../components/auth/LoginForm";
import { Button } from "../components/ui/Button";
import { ApiClientError } from "../api/client";

export function LoginPage() {
  const { login } = useAuth();
  const navigate = useNavigate();
  const [registrationEnabled, setRegistrationEnabled] = useState(false);
  const [instanceName, setInstanceName] = useState("Safe Space");
  const [motd, setMotd] = useState("");
  const [email, setEmail] = useState("");
  const [domains, setDomains] = useState<SSODomain[]>([]);
  const [returnTo, setReturnTo] = useState("");
  const [ssoStatus, setSsoStatus] = useState<string | null>(null);
  const [lookupStatus, setLookupStatus] = useState<string | null>(null);
  const [lookingUp, setLookingUp] = useState(false);

  useEffect(() => {
    authApi.getConfig().then((c) => {
      setRegistrationEnabled(c.registrationEnabled);
      if (c.instanceName) setInstanceName(c.instanceName);
      if (c.motd) setMotd(c.motd);
      if (c.oidcEnabled) {
        ssoApi.domains().then((r) => setDomains(r.domains)).catch(() => {});
      }
    }).catch(() => {});
  }, []);

  // Surface the SSO outcome from the callback redirect (?sso=...) so the user
  // sees a clear error if the IdP flow failed.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const sso = params.get("sso");
    if (sso) {
      setSsoStatus(sso);
      params.delete("sso");
    }
    // Epic 54, US-54.2: surface the lookup not-found outcome (?lookup=...).
    const lookup = params.get("lookup");
    if (lookup) {
      setLookupStatus(lookup);
      params.delete("lookup");
    }
    // Post-login redirect target (preserved from 401 handler or invite link).
    const rt = params.get("return_to");
    if (rt) {
      setReturnTo(sanitiseReturnTo(rt));
      params.delete("return_to");
    }
    const clean = params.toString();
    window.history.replaceState({}, "", clean ? `?${clean}` : window.location.pathname);
  }, []);

  const matchedDomain = domains.find((d) => email.toLowerCase().endsWith(d.domain.toLowerCase()));

  // Epic 54, US-54.2: when SSO is enabled and the typed email doesn't match a
  // claimed domain, offer a "Continue" button that calls the lookup endpoint.
  // The endpoint resolves email → org → redirectUrl (subdomain or direct SSO
  // start URL). This covers BYO-email users whose domain isn't claimed.
  const emailLooksValid = email.includes("@") && email.split("@")[1]?.includes(".");
  const showDiscoveryButton = domains.length > 0 && !matchedDomain && emailLooksValid;

  const handleDiscovery = async () => {
    setLookingUp(true);
    setLookupStatus(null);
    try {
      const { redirectUrl } = await authApi.lookup(email);
      // The not-found redirect URL is "/?lookup=not_found". In this SPA,
      // navigating there triggers a full page load → router redirect to /login
      // → query param lost. Handle it in-memory instead so the user sees the
      // error message without a broken redirect.
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

  return (
    <AuthCard
      title={`Welcome to ${instanceName}`}
      description={motd || "Sign in to your account"}
      footer={
        registrationEnabled ? (
          <Link to="/register" className="text-primary underline-offset-4 hover:underline">
            Create an account
          </Link>
        ) : undefined
      }
    >
      {ssoStatus && ssoStatus !== "success" && (
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
      )}
      {lookupStatus === "not_found" && (
        <p className="mb-3 text-sm text-red-500">
          We couldn't find an account for that email. Try a different email, or{" "}
          {registrationEnabled ? (
            <Link to="/register" className="underline underline-offset-4">
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
      <LoginForm
        onSubmit={async (u, p, r) => {
          await login(u, p, r);
          if (returnTo) navigate(returnTo);
        }}
        onEmailChange={setEmail}
      />
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
