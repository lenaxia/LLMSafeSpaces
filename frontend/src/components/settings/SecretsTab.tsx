import { useState, useEffect } from "react";
import { useToast } from "../../providers/ToastProvider";
import { secretsApi, type SecretResponse } from "../../api/secrets";
import { Button } from "../ui/Button";
import { Input } from "../ui/Input";
import { Tooltip } from "../ui/Tooltip";
import { safeConfirm } from "../../lib/safeConfirm";

const SECRET_TYPES = [
  { value: "llm-provider", label: "LLM Providers", icon: "🤖", metaFields: ["provider", "apiKey"] },
  { value: "ssh-key", label: "SSH Keys", icon: "🔑", metaFields: ["key_type", "host"] },
  { value: "git-credential", label: "Git Credentials", icon: "📦", metaFields: ["host"] },
  { value: "secret-file", label: "Secret Files", icon: "📄", metaFields: ["mount_path"] },
  { value: "env-secret", label: "Environment Variables", icon: "⚙️", metaFields: ["var_name"] },
  // api-key is a legacy type superseded by llm-provider. New secrets of this
  // type cannot be created from the UI; this entry exists solely to render
  // existing api-key secrets returned by GET /secrets (which would otherwise
  // be silently filtered out by the grouped display logic).
  { value: "api-key", label: "API Keys (legacy)", icon: "🗝️", metaFields: [] as string[], legacyOnly: true },
] as const;

// API_KEY_SUNSET_DATE is the fixed date after which new api-key secrets are
// rejected by the API (US-44.9). Kept as an ISO string to mirror the backend
// constant pkg/secrets.APIKeySunsetDate; rendered human-readable via
// formatSunsetDate so the banner reads naturally from a single source of truth.
const API_KEY_SUNSET_DATE = "2026-12-19";

const SUNSET_MONTHS = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"];

function formatSunsetDate(iso: string): string {
  const parts = iso.split("-");
  if (parts.length !== 3) return iso;
  return `${SUNSET_MONTHS[Number(parts[1]) - 1]} ${Number(parts[2])}, ${parts[0]}`;
}

const FIELD_INFO: Record<string, string> = {
  provider: "LLM provider name (e.g. anthropic, openai, deepseek). Used to configure the agent's API endpoint.",
  apiKey: "API key for the LLM provider (e.g. sk-ant-... for Anthropic).",
  baseURL: "Optional: override the provider's default API endpoint (e.g. for Ollama or a proxy).",
  key_type: "SSH key algorithm. Determines the filename (~/.ssh/id_{type}) and ssh-agent configuration.",
  host: "Remote hostname this credential is for (e.g. github.com). Used to configure SSH config entries or git credential helpers.",
  mount_path: "File path inside the workspace where this secret will be written (with 0600 permissions).",
  var_name: "Environment variable name (e.g. DATABASE_URL). Will be available to the agent process at runtime.",
  notes: "Optional notes for your reference. Not injected into the workspace.",
};

const KEY_TYPE_OPTIONS = ["ed25519", "rsa", "ecdsa"] as const;
const PROVIDER_OPTIONS = ["anthropic", "openai", "deepseek", "google", "ollama", "other"] as const;

type SecretType = (typeof SECRET_TYPES)[number]["value"];

export function SecretsTab() {
  const [secrets, setSecrets] = useState<SecretResponse[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [showCreate, setShowCreate] = useState(false);
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  const [revealingId, setRevealingId] = useState<string | null>(null);
  const [revealedValue, setRevealedValue] = useState<string | null>(null);
  const [revealPassword, setRevealPassword] = useState("");
  const [updatingId, setUpdatingId] = useState<string | null>(null);
  const [secretBindings, setSecretBindings] = useState<Record<string, string[]>>({});
  const { toast } = useToast();

  const fetchSecrets = async () => {
    try {
      const res = await secretsApi.list();
      const secs = res.secrets || [];
      setSecrets(secs);
      // Fetch bindings for each secret
      const bindings: Record<string, string[]> = {};
      await Promise.all(secs.map(async (s) => {
        try {
          const b = await secretsApi.getSecretBindings(s.id);
          bindings[s.id] = b.workspaces;
        } catch { bindings[s.id] = []; }
      }));
      setSecretBindings(bindings);
    } catch (e: unknown) {
      setError((e instanceof Error ? e.message : String(e)));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { fetchSecrets(); }, []);

  // Auto-hide revealed value after 30 seconds
  useEffect(() => {
    if (revealedValue) {
      const timer = setTimeout(() => { setRevealedValue(null); setRevealingId(null); }, 30000);
      return () => clearTimeout(timer);
    }
  }, [revealedValue]);

  const handleDelete = async (id: string, name: string) => {
    if (!safeConfirm(`Delete secret "${name}"? This cannot be undone.`)) return;
    try {
      await secretsApi.delete(id);
      setSecrets((s) => s.filter((x) => x.id !== id));
    } catch (e: unknown) {
      setError((e instanceof Error ? e.message : String(e)));
    }
  };

  const handleReveal = async (id: string) => {
    if (!revealPassword) return;
    try {
      const res = await secretsApi.reveal(id, revealPassword);
      setRevealedValue(res.value);
      setRevealPassword("");
    } catch (e: unknown) {
      setError((e instanceof Error ? e.message : String(e)) === "encryption key not available; re-authenticate" ? "Session expired. Please log in again." : (e instanceof Error ? e.message : String(e)));
    }
  };

  const copyToClipboard = (text: string) => {
    navigator.clipboard.writeText(text);
    toast("Copied to clipboard");
  };

  const toggleGroup = (type: string) => {
    setCollapsed((c) => ({ ...c, [type]: !c[type] }));
  };

  const grouped = SECRET_TYPES.map((t) => ({
    ...t,
    secrets: secrets.filter((s) => s.type === t.value),
  })).filter((g) => g.secrets.length > 0);

  if (loading) return <p className="text-sm text-muted-foreground">Loading secrets...</p>;

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-lg font-medium">Secrets</h3>
          <p className="text-sm text-muted-foreground">
            Encrypted secrets injected into your workspaces. Values are never stored in plaintext.
          </p>
        </div>
        <Button onClick={() => setShowCreate(!showCreate)}>
          {showCreate ? "Cancel" : "+ New Secret"}
        </Button>
      </div>

      {error && (
        <div className="flex items-center justify-between rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">
          <span>{error}</span>
          <button onClick={() => setError("")} className="text-destructive">✕</button>
        </div>
      )}

      {showCreate && (
        <CreateSecretForm
          onCreated={() => { setShowCreate(false); fetchSecrets(); }}
          onError={setError}
        />
      )}

      {grouped.length === 0 && !showCreate ? (
        <p className="text-sm text-muted-foreground">No secrets yet. Create one to get started.</p>
      ) : (
        <div className="space-y-3">
          {grouped.map((group) => (
            <div key={group.value} className="rounded-md border border-border">
              <button
                onClick={() => toggleGroup(group.value)}
                className="flex w-full items-center justify-between px-4 py-2.5 text-left hover:bg-accent/30 transition-colors"
              >
                <span className="flex items-center gap-2 text-sm font-medium">
                  <span>{group.icon}</span>
                  <span>{group.label}</span>
                  <span className="rounded-full bg-accent px-2 py-0.5 text-xs">{group.secrets.length}</span>
                </span>
                <span className="text-xs text-muted-foreground">{collapsed[group.value] ? "▶" : "▼"}</span>
              </button>
              {"legacyOnly" in group && group.legacyOnly && (
                <div className="border-t border-border px-4 py-2 bg-amber-50 dark:bg-amber-950/20">
                  <p className="text-xs text-amber-700 dark:text-amber-400">
                    ⚠️ <code>api-key</code> secrets are deprecated. Migrate to <code>llm-provider</code> (for LLM APIs) or <code>env-secret</code> (for other APIs). <code>api-key</code> will be removed on {formatSunsetDate(API_KEY_SUNSET_DATE)}.
                  </p>
                </div>
              )}
              {!collapsed[group.value] && (
                <div className="border-t border-border divide-y divide-border">
                  {group.secrets.map((s) => (
                    <div key={s.id} className="px-4 py-2.5">
                      <div className="flex flex-wrap items-center justify-between gap-2">
                        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 min-w-0">
                          <span className="text-sm font-medium">{s.name}</span>
                          {s.metadata && Object.entries(s.metadata)
                            .filter(([k]) => k !== "public_key" && k !== "notes")
                            .map(([k, v]) => (
                              <span key={k} className="text-xs text-muted-foreground">{k}: {v}</span>
                            ))}
                          <span className="text-xs text-muted-foreground">
                            {new Date(s.createdAt).toLocaleDateString()}
                          </span>
                          {s.metadata?.notes && (
                            <Tooltip content={s.metadata.notes} side="top" align="start" maxWidth="md">
                              <span className="text-xs italic text-muted-foreground truncate max-w-[12rem] inline-block align-bottom cursor-default">
                                — {s.metadata.notes}
                              </span>
                            </Tooltip>
                          )}
                          {(secretBindings[s.id] ?? []).length > 0 && (
                            <span className="text-xs text-muted-foreground">
                              · {(secretBindings[s.id] ?? []).length} workspace{(secretBindings[s.id] ?? []).length > 1 ? "s" : ""}
                            </span>
                          )}
                          {s.globalDefault && (
                            <Tooltip content="Automatically bound to all newly created workspaces." side="top" align="start">
                              <span className="rounded-full bg-primary/10 px-2 py-0.5 text-xs text-primary cursor-default">
                                all workspaces
                              </span>
                            </Tooltip>
                          )}
                        </div>
                        <div className="flex items-center gap-2 shrink-0">
                          <button
                            onClick={() => { setRevealingId(revealingId === s.id ? null : s.id); setRevealedValue(null); setUpdatingId(null); }}
                            className="rounded px-2 py-1 text-xs text-primary hover:bg-primary/10 transition-colors"
                          >
                            {revealingId === s.id ? "Hide" : "Reveal"}
                          </button>
                          <button
                            onClick={() => { setUpdatingId(updatingId === s.id ? null : s.id); setRevealingId(null); setRevealedValue(null); }}
                            className="rounded px-2 py-1 text-xs text-primary hover:bg-primary/10 transition-colors"
                          >
                            {updatingId === s.id ? "Cancel" : "Update"}
                          </button>
                          <button
                            onClick={() => handleDelete(s.id, s.name)}
                            className="rounded px-2 py-1 text-xs text-destructive hover:bg-destructive/10 transition-colors"
                          >
                            Delete
                          </button>
                        </div>
                      </div>

                      {/* Public key display for SSH keys */}
                      {s.metadata?.public_key && (
                        <div className="mt-2">
                          <span className="text-xs text-muted-foreground font-medium">Public key (safe to share):</span>
                          <div className="mt-1 flex items-center gap-2 min-w-0">
                            <code className="flex-1 min-w-0 rounded bg-accent/50 px-2 py-1 text-xs font-mono truncate">
                              {s.metadata.public_key}
                            </code>
                            <button
                              onClick={() => copyToClipboard(s.metadata.public_key!)}
                              className="text-xs text-primary hover:text-primary/80 whitespace-nowrap shrink-0"
                            >
                              Copy public key
                          </button>
                          </div>
                        </div>
                      )}

                      {/* Reveal panel */}
                      {revealingId === s.id && !revealedValue && (
                        <div className="mt-2 flex items-center gap-2">
                          <Input
                            type="password"
                            value={revealPassword}
                            onChange={(e) => setRevealPassword(e.target.value)}
                            placeholder="Enter password to reveal"
                            className="text-sm flex-1"
                            onKeyDown={(e) => e.key === "Enter" && handleReveal(s.id)}
                          />
                          <Button onClick={() => handleReveal(s.id)} disabled={!revealPassword}>
                            Decrypt
                          </Button>
                        </div>
                      )}

                      {/* Revealed value */}
                      {revealingId === s.id && revealedValue && (
                        <div className="mt-2 space-y-1">
                          <div className="flex items-center gap-2">
                            <code className="flex-1 rounded bg-background border border-border px-2 py-1 text-xs font-mono text-foreground break-all">
                              {revealedValue}
                            </code>
                            <button
                              onClick={() => copyToClipboard(revealedValue)}
                              className="text-xs text-primary hover:text-primary/80 whitespace-nowrap"
                            >
                              Copy
                            </button>
                          </div>
                          <p className="text-xs text-muted-foreground">Auto-hides in 30 seconds</p>
                        </div>
                      )}

                      {/* Update panel */}
                      {updatingId === s.id && (
                        <UpdateSecretForm
                          secret={s}
                          onUpdated={() => { setUpdatingId(null); fetchSecrets(); }}
                          onCancel={() => setUpdatingId(null)}
                          onError={setError}
                        />
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// --- Update Form ---

function UpdateSecretForm({
  secret,
  onUpdated,
  onCancel,
  onError,
}: {
  secret: SecretResponse;
  onUpdated: () => void;
  onCancel: () => void;
  onError: (e: string) => void;
}) {
  const { toast } = useToast();
  const [value, setValue] = useState("");
  const [globalDefault, setGlobalDefault] = useState(secret.globalDefault);
  const [submitting, setSubmitting] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    try {
      await secretsApi.update(secret.id, { value, globalDefault });
      toast("Secret updated");
      onUpdated();
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      onError(msg === "encryption key not available; re-authenticate"
        ? "Session expired. Please log out and log back in to manage secrets."
        : msg);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <form onSubmit={handleSubmit} className="mt-2 space-y-3 rounded-md border border-border bg-accent/5 p-3">
      <div>
        <label className="text-xs font-medium text-muted-foreground">New value</label>
        <textarea
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder="Enter new secret value"
          className="mt-1 w-full rounded-md border border-border bg-background px-3 py-2 text-sm font-mono min-h-[60px] resize-y"
          required
        />
      </div>
      <div className="flex items-center gap-2">
        <input
          id={`update-global-default-${secret.id}`}
          type="checkbox"
          checked={globalDefault}
          onChange={(e) => setGlobalDefault(e.target.checked)}
          className="h-4 w-4 rounded border-border"
        />
        <label htmlFor={`update-global-default-${secret.id}`} className="text-xs text-foreground cursor-pointer">
          Include in all workspaces
        </label>
        <Tooltip content="When enabled, this secret is automatically bound to all newly created workspaces. Does not apply retroactively to existing workspaces." side="top" align="start">
          <button type="button" className="cursor-help focus:outline-none" aria-label="More info">
            <span className="text-muted-foreground text-xs">ⓘ</span>
          </button>
        </Tooltip>
      </div>
      <div className="flex items-center gap-2">
        <Button type="submit" disabled={submitting || !value}>
          {submitting ? "Saving..." : "Save"}
        </Button>
        <button type="button" onClick={onCancel} className="text-xs text-muted-foreground hover:text-foreground">
          Cancel
        </button>
      </div>
    </form>
  );
}

// --- Create Form ---

function generateRandomSecret(length = 32): string {
  const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  const array = new Uint8Array(length);
  crypto.getRandomValues(array);
  return Array.from(array, (b) => chars[b % chars.length]).join("");
}

async function generateSSHKeypair(): Promise<{ privateKey: string; publicKey: string }> {
  const keyPair = await crypto.subtle.generateKey(
    { name: "Ed25519" } as EcKeyGenParams,
    true,
    ["sign", "verify"],
  ).catch(() => null);

  if (keyPair) {
    const privPkcs8 = await crypto.subtle.exportKey("pkcs8", (keyPair as CryptoKeyPair).privateKey);
    const pubRaw = new Uint8Array(await crypto.subtle.exportKey("raw", (keyPair as CryptoKeyPair).publicKey));

    // Build OpenSSH public key wire format: [len][algorithm][len][key]
    const algo = new TextEncoder().encode("ssh-ed25519");
    const pubWire = new Uint8Array(4 + algo.length + 4 + pubRaw.length);
    new DataView(pubWire.buffer).setUint32(0, algo.length);
    pubWire.set(algo, 4);
    new DataView(pubWire.buffer).setUint32(4 + algo.length, pubRaw.length);
    pubWire.set(pubRaw, 4 + algo.length + 4);
    const pubB64 = btoa(String.fromCharCode(...pubWire));

    // Build OpenSSH private key format
    const checkInt = crypto.getRandomValues(new Uint32Array(1))[0]!;
    const comment = new TextEncoder().encode("generated@llmsafespaces");
    // Extract raw 32-byte private key from PKCS#8 (last 32 bytes of the DER)
    const privBytes = new Uint8Array(privPkcs8).slice(-32);
    // OpenSSH ed25519 private key "keypair" is privkey(32) || pubkey(32)
    const privKeyPair = new Uint8Array(64);
    privKeyPair.set(privBytes, 0);
    privKeyPair.set(pubRaw, 32);

    // Assemble private section (unencrypted)
    const privSection = encodeOpenSSHPrivateSection(checkInt, pubRaw, privKeyPair, comment);
    // Pad to block size (8 for none cipher)
    const padded = padToBlockSize(privSection, 8);

    // Assemble full OpenSSH private key
    const cipherName = new TextEncoder().encode("none");
    const kdfName = new TextEncoder().encode("none");
    const authMagic = new TextEncoder().encode("openssh-key-v1\0");

    const parts: Uint8Array[] = [
      authMagic,
      uint32BE(cipherName.length), cipherName,
      uint32BE(kdfName.length), kdfName,
      uint32BE(0), // kdf options (empty string)
      uint32BE(1), // number of keys
      uint32BE(pubWire.length), pubWire,
      uint32BE(padded.length), padded,
    ];
    const blob = concatBytes(parts);
    const privB64 = btoa(String.fromCharCode(...blob));
    const privLines = privB64.match(/.{1,70}/g)!.join("\n");

    return {
      privateKey: `-----BEGIN OPENSSH PRIVATE KEY-----\n${privLines}\n-----END OPENSSH PRIVATE KEY-----\n`,
      publicKey: `ssh-ed25519 ${pubB64} generated@llmsafespaces`,
    };
  }

  // Fallback: generate random bytes as placeholder
  const priv = generateRandomSecret(64);
  const pub = `ssh-ed25519 ${btoa(generateRandomSecret(32))} generated@llmsafespaces`;
  return { privateKey: priv, publicKey: pub };
}

function uint32BE(n: number): Uint8Array {
  const buf = new Uint8Array(4);
  new DataView(buf.buffer).setUint32(0, n);
  return buf;
}

function concatBytes(arrays: Uint8Array[]): Uint8Array {
  const total = arrays.reduce((s, a) => s + a.length, 0);
  const result = new Uint8Array(total);
  let offset = 0;
  for (const a of arrays) { result.set(a, offset); offset += a.length; }
  return result;
}

function encodeOpenSSHPrivateSection(checkInt: number, pubRaw: Uint8Array, privKeyPair: Uint8Array, comment: Uint8Array): Uint8Array {
  const algo = new TextEncoder().encode("ssh-ed25519");
  const parts: Uint8Array[] = [
    uint32BE(checkInt), uint32BE(checkInt), // two identical check ints
    uint32BE(algo.length), algo,
    uint32BE(pubRaw.length), pubRaw,
    uint32BE(privKeyPair.length), privKeyPair,
    uint32BE(comment.length), comment,
  ];
  return concatBytes(parts);
}

function padToBlockSize(data: Uint8Array, blockSize: number): Uint8Array {
  const padLen = blockSize - (data.length % blockSize);
  if (padLen === blockSize) return data;
  const padded = new Uint8Array(data.length + padLen);
  padded.set(data);
  for (let i = 0; i < padLen; i++) padded[data.length + i] = i + 1;
  return padded;
}

function CreateSecretForm({ onCreated, onError }: { onCreated: () => void; onError: (e: string) => void }) {
  const { toast } = useToast();
  const [name, setName] = useState("");
  const [type, setType] = useState<SecretType>("llm-provider");
  const [value, setValue] = useState("");
  const [metadata, setMetadata] = useState<Record<string, string>>({});
  const [globalDefault, setGlobalDefault] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [createdValue, setCreatedValue] = useState<string | null>(null);
  const [generating, setGenerating] = useState(false);

  const selectedType = SECRET_TYPES.find((t) => t.value === type)!;

  const handleGenerate = async () => {
    setGenerating(true);
    if (type === "ssh-key") {
      const kp = await generateSSHKeypair();
      setValue(kp.privateKey);
      setMetadata((m) => ({ ...m, public_key: kp.publicKey, key_type: m.key_type || "ed25519" }));
    } else {
      setValue(generateRandomSecret(48));
    }
    setGenerating(false);
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    try {
      const submitMetadata = { ...metadata };
      await secretsApi.create({
        name, type, value, globalDefault,
        metadata: Object.keys(submitMetadata).length > 0 ? submitMetadata : undefined,
      });
      setCreatedValue(value);
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      onError(msg === "encryption key not available; re-authenticate"
        ? "Session expired. Please log out and log back in to manage secrets."
        : msg);
    } finally {
      setSubmitting(false);
    }
  };

  const handleDone = () => {
    setCreatedValue(null);
    onCreated();
  };

  // Show "secret created" confirmation with copy option
  if (createdValue) {
    return (
      <div className="rounded-md border border-border bg-accent/20 p-4 space-y-3">
        <p className="text-sm font-medium text-foreground">✓ Secret "{name}" created and encrypted</p>
        <div className="flex items-center gap-2">
          <code className="flex-1 rounded bg-background border border-border px-2 py-1 text-xs font-mono text-foreground break-all max-h-24 overflow-auto whitespace-pre-wrap">
            {createdValue}
          </code>
          <button
            onClick={() => { navigator.clipboard.writeText(createdValue); toast("Copied to clipboard"); }}
            className="rounded bg-primary px-3 py-1 text-xs text-primary-foreground hover:bg-primary/90"
          >
            Copy
          </button>
        </div>
        {metadata.public_key && (
          <div className="flex items-center gap-2 min-w-0">
            <span className="text-xs text-muted-foreground shrink-0">Public key:</span>
            <code className="flex-1 min-w-0 rounded bg-background border border-border px-2 py-1 text-xs font-mono text-foreground truncate">
              {metadata.public_key}
            </code>
            <button
              onClick={() => { navigator.clipboard.writeText(metadata.public_key ?? ""); toast("Public key copied"); }}
              className="rounded bg-primary px-3 py-1 text-xs text-primary-foreground hover:bg-primary/90 shrink-0"
            >
              Copy
            </button>
          </div>
        )}
        <p className="text-xs text-muted-foreground">You can reveal this value later using your password.</p>
        <Button onClick={handleDone}>Done</Button>
      </div>
    );
  }

  return (
    <form onSubmit={handleSubmit} className="space-y-4 rounded-md border border-border p-4 bg-accent/5">
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
        <div>
          <label className="text-sm font-medium">Name</label>
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="my-api-key" required />
        </div>
        <div>
          <label className="text-sm font-medium">Type</label>
          <select
            value={type}
            onChange={(e) => { setType(e.target.value as SecretType); setMetadata({}); setValue(""); }}
            className="w-full rounded-md border border-border bg-background px-3 py-2 text-sm"
          >
            {SECRET_TYPES.filter((t) => !("legacyOnly" in t && t.legacyOnly)).map((t) => (
              <option key={t.value} value={t.value}>{t.icon} {t.label}</option>
            ))}
          </select>
        </div>
      </div>

      <div>
        <div className="flex items-center justify-between mb-1">
          <label className="text-sm font-medium">Value</label>
          <button
            type="button"
            onClick={handleGenerate}
            disabled={generating}
            className="text-xs text-primary hover:text-primary/80"
          >
            {type === "ssh-key" ? "🔑 Generate keypair" : "🎲 Generate random"}
          </button>
        </div>
        <textarea
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder={type === "ssh-key" ? "-----BEGIN OPENSSH PRIVATE KEY-----" : type === "env-secret" ? "postgres://user:pass@host/db" : "sk-..."}
          className="w-full rounded-md border border-border bg-background px-3 py-2 text-sm font-mono min-h-[80px] resize-y"
          required
        />
        <p className="mt-1 text-xs text-muted-foreground">This value will be encrypted. You can reveal it later with your password.</p>
      </div>

      {selectedType.metaFields.length > 0 && (
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          {selectedType.metaFields.map((field) => (
            <div key={field}>
              <label className="text-sm font-medium inline-flex items-center gap-1">
                {field === "mount_path" ? "File path" : field.replace(/_/g, " ")}
                {FIELD_INFO[field] && (
                  <Tooltip content={FIELD_INFO[field]} side="top" align="start">
                    <button type="button" className="cursor-help focus:outline-none" aria-label="More info">
                      <span className="text-muted-foreground text-xs">ⓘ</span>
                    </button>
                  </Tooltip>
                )}
              </label>
              {field === "mount_path" ? (
                <div className="flex items-center gap-0">
                  <span className="rounded-l-md border border-r-0 border-border bg-accent px-2 py-2 text-xs text-muted-foreground whitespace-nowrap">/home/sandbox/.secrets/</span>
                  <Input
                    value={metadata[field] || ""}
                    onChange={(e) => setMetadata({ ...metadata, [field]: e.target.value.replace(/^\/+/, "").replace(/\.\.\//g, "") })}
                    placeholder="cert.pem"
                    required
                    className="rounded-l-none"
                  />
                </div>
              ) : field === "key_type" ? (
                <select
                  value={metadata[field] || "ed25519"}
                  onChange={(e) => setMetadata({ ...metadata, [field]: e.target.value })}
                  className="w-full rounded-md border border-border bg-background px-3 py-2 text-sm"
                >
                  {KEY_TYPE_OPTIONS.map((opt) => (
                    <option key={opt} value={opt}>{opt}</option>
                  ))}
                </select>
              ) : field === "provider" ? (
                <select
                  value={metadata[field] || ""}
                  onChange={(e) => setMetadata({ ...metadata, [field]: e.target.value })}
                  className="w-full rounded-md border border-border bg-background px-3 py-2 text-sm"
                  required
                >
                  <option value="" disabled>Select provider...</option>
                  {PROVIDER_OPTIONS.map((opt) => (
                    <option key={opt} value={opt}>{opt}</option>
                  ))}
                </select>
              ) : (
                <Input
                  value={metadata[field] || ""}
                  onChange={(e) => setMetadata({ ...metadata, [field]: e.target.value })}
                  placeholder={field === "var_name" ? "DATABASE_URL" : "github.com"}
                  required={field === "var_name"}
                />
              )}
            </div>
          ))}
        </div>
      )}

      {/* Notes field — always available */}
      <div>
        <label className="text-sm font-medium inline-flex items-center gap-1">
          Notes
          <Tooltip content={FIELD_INFO.notes} side="top" align="start">
            <button type="button" className="cursor-help focus:outline-none" aria-label="More info">
              <span className="text-muted-foreground text-xs">ⓘ</span>
            </button>
          </Tooltip>
        </label>
        <Input
          value={metadata.notes || ""}
          onChange={(e) => setMetadata({ ...metadata, notes: e.target.value })}
          placeholder="Optional — e.g. 'production key, expires 2026-12'"
        />
      </div>

      {/* Show public key if generated */}
      {metadata.public_key && (
        <div>
          <label className="text-sm font-medium">Public Key (will be stored unencrypted for display)</label>
          <div className="flex items-center gap-2 mt-1 min-w-0">
            <code className="flex-1 min-w-0 rounded bg-accent/50 px-2 py-1 text-xs font-mono truncate">
              {metadata.public_key}
            </code>
            <button type="button" onClick={() => { navigator.clipboard.writeText(metadata.public_key ?? ""); toast("Public key copied"); }} className="text-xs text-primary shrink-0">
              Copy
            </button>
          </div>
        </div>
      )}

      {/* Include in all workspaces */}
      <div className="flex items-center gap-2">
        <input
          id="create-global-default"
          type="checkbox"
          checked={globalDefault}
          onChange={(e) => setGlobalDefault(e.target.checked)}
          className="h-4 w-4 rounded border-border"
        />
        <label htmlFor="create-global-default" className="text-sm text-foreground cursor-pointer">
          Include in all workspaces
        </label>
        <Tooltip
          content="When enabled, this secret is automatically bound to all newly created workspaces. Does not apply retroactively to existing workspaces."
          side="top"
          align="start"
        >
          <button type="button" className="cursor-help focus:outline-none" aria-label="More info">
            <span className="text-muted-foreground text-xs">ⓘ</span>
          </button>
        </Tooltip>
      </div>

      <Button type="submit" disabled={submitting || !name || !value}>
        {submitting ? "Encrypting..." : "Create Secret"}
      </Button>
    </form>
  );
}
