import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useAuth } from "../providers/AuthProvider";
import { sanitiseReturnTo } from "../lib/returnTo";
import { AuthCard } from "../components/auth/AuthCard";
import { RegisterForm } from "../components/auth/RegisterForm";

export function RegisterPage() {
  const { register } = useAuth();
  const navigate = useNavigate();
  const [returnTo, setReturnTo] = useState("");

  useEffect(() => {
    const raw = new URLSearchParams(window.location.search).get("return_to");
    if (raw) setReturnTo(sanitiseReturnTo(raw));
  }, []);

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
      <RegisterForm
        onSubmit={async (u, e, p, t) => {
          await register(u, e, p, t);
          if (returnTo) navigate(returnTo);
        }}
      />
    </AuthCard>
  );
}
