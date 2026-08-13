import { useEffect, useState } from "react";
import { useOutletContext } from "react-router-dom";
import { type OrgResponse } from "../../api/orgs";
import { ssoApi, type OrgSSOConfig, type OrgRole } from "../../api/sso";
import { Button } from "../ui/Button";
import { Spinner } from "../ui/Spinner";
import { safeConfirm } from "../../lib/safeConfirm";

interface SSOContext {
  org: OrgResponse;
  isAdmin: boolean;
}

interface FormState {
  discoveryUrl: string;
  clientId: string;
  clientSecret: string;
  claimedDomains: string;
  autoProvision: boolean;
  /** Serialized as "group:role" lines for simple textarea editing. */
  mapping: string;
}

function toFormState(cfg: OrgSSOConfig | null): FormState {
  const mapping = cfg
    ? Object.entries(cfg.groupRoleMapping)
        .map(([g, r]) => `${g}:${r}`)
        .join("\n")
    : "";
  return {
    discoveryUrl: cfg?.discoveryUrl ?? "",
    clientId: cfg?.clientId ?? "",
    clientSecret: "",
    claimedDomains: (cfg?.claimedDomains ?? []).join(", "),
    autoProvision: cfg?.autoProvision ?? true,
    mapping,
  };
}

function parseMapping(text: string): Record<string, OrgRole> {
  const out: Record<string, OrgRole> = {};
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const idx = trimmed.lastIndexOf(":");
    if (idx <= 0) continue;
    const group = trimmed.slice(0, idx).trim();
    const role = trimmed.slice(idx + 1).trim();
    if (role !== "admin" && role !== "member") continue;
    out[group] = role;
  }
  return out;
}

export function OrgSSOTab() {
  const { org, isAdmin } = useOutletContext<SSOContext>();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [hasSecret, setHasSecret] = useState(false);
  const [verifiedDomains, setVerifiedDomains] = useState<string[]>([]);
  const [verificationToken, setVerificationToken] = useState("");
  const [form, setForm] = useState<FormState>(toFormState(null));

  useEffect(() => {
    if (!isAdmin) {
      setLoading(false);
      return;
    }
    setLoading(true);
    ssoApi
      .getConfig(org.id)
      .then((cfg) => {
        setForm(toFormState(cfg));
        setHasSecret(cfg.hasSecret);
        setVerifiedDomains(cfg.verifiedDomains ?? []);
        setVerificationToken(cfg.verificationToken ?? "");
      })
      .catch((e) => {
        // 404/empty config is the normal "not configured yet" state — render defaults.
        setForm(toFormState(null));
        setHasSecret(false);
        if (!(e?.status === 404)) {
          setError(e instanceof Error ? e.message : "Failed to load SSO config");
        }
      })
      .finally(() => setLoading(false));
  }, [org.id, isAdmin]);

  const handleSave = async () => {
    setSaving(true);
    setError("");
    setSaved(false);
    try {
      const cfg = await ssoApi.upsert(org.id, {
        discoveryUrl: form.discoveryUrl.trim(),
        clientId: form.clientId.trim(),
        clientSecret: form.clientSecret.trim() || undefined,
        claimedDomains: form.claimedDomains
          .split(",")
          .map((d) => d.trim())
          .filter(Boolean),
        autoProvision: form.autoProvision,
        groupRoleMapping: parseMapping(form.mapping),
      });
      setHasSecret(cfg.hasSecret);
      setForm((prev) => ({ ...prev, clientSecret: "" }));
      setSaved(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to save SSO config");
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async () => {
    if (!safeConfirm("Remove SSO configuration? Members will no longer be able to sign in via SSO.")) {
      return;
    }
    setSaving(true);
    setError("");
    try {
      await ssoApi.remove(org.id);
      setForm(toFormState(null));
      setHasSecret(false);
      setSaved(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to delete SSO config");
    } finally {
      setSaving(false);
    }
  };

  if (!isAdmin) {
    return <p className="text-sm text-muted-foreground">Admin access required.</p>;
  }

  if (loading) {
    return (
      <div className="flex items-center justify-center py-12">
        <Spinner size="md" />
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-xl font-semibold">Single Sign-On (OIDC)</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Configure an OIDC identity provider so members sign in with their work account.
          The client secret is encrypted at rest and never displayed after saving.
        </p>
      </div>

      {error && <p className="text-sm text-red-500">{error}</p>}
      {saved && <p className="text-sm text-green-600">SSO configuration saved.</p>}

      <div className="space-y-4 rounded border border-border p-4">
        <label className="block text-sm">
          <span className="mb-1 block font-medium">Discovery URL</span>
          <input
            type="url"
            className="w-full rounded border border-border bg-background px-3 py-2 text-sm"
            placeholder="https://idp.example.com/.well-known/openid-configuration"
            value={form.discoveryUrl}
            onChange={(e) => setForm({ ...form, discoveryUrl: e.target.value })}
          />
        </label>

        <label className="block text-sm">
          <span className="mb-1 block font-medium">Client ID</span>
          <input
            type="text"
            className="w-full rounded border border-border bg-background px-3 py-2 text-sm font-mono"
            value={form.clientId}
            onChange={(e) => setForm({ ...form, clientId: e.target.value })}
          />
        </label>

        <label className="block text-sm">
          <span className="mb-1 block font-medium">
            Client Secret {hasSecret && <span className="text-muted-foreground">(stored — leave blank to keep current)</span>}
          </span>
          <input
            type="password"
            className="w-full rounded border border-border bg-background px-3 py-2 text-sm font-mono"
            placeholder={hasSecret ? "••••••••" : "IdP client secret"}
            value={form.clientSecret}
            onChange={(e) => setForm({ ...form, clientSecret: e.target.value })}
            autoComplete="new-password"
          />
        </label>

        <label className="block text-sm">
          <span className="mb-1 block font-medium">Claimed Domains</span>
          <input
            type="text"
            className="w-full rounded border border-border bg-background px-3 py-2 text-sm"
            placeholder="@acme.com, @acme.io"
            value={form.claimedDomains}
            onChange={(e) => setForm({ ...form, claimedDomains: e.target.value })}
          />
          <span className="mt-1 block text-xs text-muted-foreground">
            Comma-separated. Users with these email domains get a “Sign in with {org.name}” button.
          </span>
        </label>

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={form.autoProvision}
            onChange={(e) => setForm({ ...form, autoProvision: e.target.checked })}
          />
          <span>Auto-provision new users on first SSO login</span>
        </label>

        <label className="block text-sm">
          <span className="mb-1 block font-medium">Group → Role Mapping</span>
          <textarea
            className="w-full rounded border border-border bg-background px-3 py-2 text-sm font-mono"
            rows={4}
            placeholder={"admins:admin\ndevelopers:member"}
            value={form.mapping}
            onChange={(e) => setForm({ ...form, mapping: e.target.value })}
          />
          <span className="mt-1 block text-xs text-muted-foreground">
            One mapping per line as <code>groupName:role</code> (role is admin or member).
            The highest-privilege matching group wins; unmapped users default to member.
          </span>
        </label>

        <div className="flex items-center gap-2">
          <Button size="sm" onClick={handleSave} disabled={saving}>
            {saving ? "Saving…" : "Save SSO Configuration"}
          </Button>
          {hasSecret && (
            <Button size="sm" variant="destructive" onClick={handleDelete} disabled={saving}>
              Remove
            </Button>
          )}
        </div>
      </div>

      {hasSecret && form.claimedDomains && (
        <DomainVerification
          orgId={org.id}
          orgName={org.name}
          claimedDomains={form.claimedDomains.split(",").map((d) => d.trim().replace(/^@/, "")).filter(Boolean)}
          verifiedDomains={verifiedDomains}
          verificationToken={verificationToken}
          onVerified={(domain) => setVerifiedDomains((prev) => [...new Set([...prev, domain])])}
          onTokenRotated={(token) => setVerificationToken(token)}
        />
      )}
    </div>
  );
}

interface DomainVerificationProps {
  orgId: string;
  orgName: string;
  claimedDomains: string[];
  verifiedDomains: string[];
  verificationToken: string;
  onVerified: (domain: string) => void;
  onTokenRotated: (token: string) => void;
}

function DomainVerification({
  orgId,
  orgName,
  claimedDomains,
  verifiedDomains,
  verificationToken,
  onVerified,
  onTokenRotated,
}: DomainVerificationProps) {
  const [verifying, setVerifying] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [success, setSuccess] = useState("");

  const handleVerify = async (domain: string) => {
    setVerifying(domain);
    setError("");
    setSuccess("");
    try {
      const result = await ssoApi.verifyDomain(orgId, domain);
      if (result.verified) {
        onVerified(domain);
        setSuccess(`${domain} verified — auto-routing enabled.`);
      } else {
        setError(`${domain} verification failed — check the TXT record.`);
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Verification failed";
      setError(`${domain}: ${msg}`);
    } finally {
      setVerifying(null);
    }
  };

  const handleRotate = async () => {
    if (!safeConfirm("Rotate the verification token? Existing DNS records must be updated to match.")) {
      return;
    }
    setVerifying("token");
    setError("");
    try {
      const result = await ssoApi.rotateToken(orgId);
      onTokenRotated(result.verificationToken);
      setSuccess("Token rotated. Update your DNS TXT records.");
    } catch (e) {
      setError(e instanceof Error ? e.message : "Token rotation failed");
    } finally {
      setVerifying(null);
    }
  };

  const hasToken = verificationToken.length > 0;

  return (
    <div className="space-y-4 rounded border border-border p-4">
      <div>
        <h3 className="text-lg font-semibold">Domain Verification</h3>
        <p className="mt-1 text-sm text-muted-foreground">
          Verified domains auto-route on the login page (users see "Sign in with {orgName}").
          Unverified claimed domains require the org slug URL — no auto-routing.
        </p>
      </div>

      {error && <p className="text-sm text-red-500">{error}</p>}
      {success && <p className="text-sm text-green-600">{success}</p>}

      {claimedDomains.length === 0 ? (
        <p className="text-sm text-muted-foreground">No claimed domains. Add domains above and save first.</p>
      ) : (
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-border text-left">
              <th className="py-2">Domain</th>
              <th className="py-2">Status</th>
              <th className="py-2">Action</th>
            </tr>
          </thead>
          <tbody>
            {claimedDomains.map((domain) => {
              const isVerified = verifiedDomains.includes(domain);
              return (
                <tr key={domain} className="border-b border-border">
                  <td className="py-2 font-mono">{domain}</td>
                  <td className="py-2">
                    {isVerified ? (
                      <span className="text-green-600">✓ Verified</span>
                    ) : (
                      <span className="text-muted-foreground">Unverified</span>
                    )}
                  </td>
                  <td className="py-2">
                    {!isVerified && (
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => handleVerify(domain)}
                        disabled={verifying !== null}
                      >
                        {verifying === domain ? "Checking…" : "Verify"}
                      </Button>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      <div className="space-y-2 border-t border-border pt-4">
        <div className="flex items-center justify-between">
          <span className="text-sm font-medium">DNS Verification Token</span>
          <Button size="sm" variant="outline" onClick={handleRotate} disabled={verifying !== null}>
            {verifying === "token" ? "Rotating…" : hasToken ? "Rotate Token" : "Generate Token"}
          </Button>
        </div>
        {hasToken ? (
          <>
            <p className="text-xs text-muted-foreground">
              Add a DNS TXT record:
            </p>
            <div className="rounded bg-muted p-2 font-mono text-xs">
              <div>
                <span className="text-muted-foreground">Name:</span>{" "}
                _llmsafespaces-verify.{claimedDomains[0] ?? "<domain>"}
              </div>
              <div>
                <span className="text-muted-foreground">Value:</span> {verificationToken}
              </div>
            </div>
            <p className="text-xs text-muted-foreground">
              One token works for all your domains. Add the TXT record to each domain's DNS,
              then click Verify above.
            </p>
          </>
        ) : (
          <p className="text-xs text-muted-foreground">
            No token generated yet. Click "Generate Token" to get a value for your DNS TXT record.
          </p>
        )}
      </div>
    </div>
  );
}
