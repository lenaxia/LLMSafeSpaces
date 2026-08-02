import { useEffect } from "react";
import { Navigate, useParams } from "react-router-dom";
import { ssoRedirectURL } from "../api/sso";
import { Spinner } from "../components/ui/Spinner";

export function SSOStartPage() {
  const { orgSlug } = useParams<{ orgSlug: string }>();

  useEffect(() => {
    if (!orgSlug) return;
    window.location.href = ssoRedirectURL(orgSlug);
  }, [orgSlug]);

  if (!orgSlug) {
    return <Navigate to="/login" replace />;
  }

  return (
    <div className="flex h-screen items-center justify-center">
      <Spinner size="lg" />
    </div>
  );
}
