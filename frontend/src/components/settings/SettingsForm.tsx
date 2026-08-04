import { useState, useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import type { SettingDef } from "../../api/settings";
import { Toggle } from "../ui/Toggle";
import { NumberInput } from "../ui/NumberInput";
import { Select } from "../ui/Select";
import { TagInput } from "../ui/TagInput";
import { normalizeSettingValue } from "../../lib/settingsNormalize";
import { imageFactoryApi } from "../../api/imageFactory";

interface SettingsFormProps {
  schema: SettingDef[];
  values: Record<string, unknown>;
  onSave: (key: string, value: unknown) => Promise<void>;
  disabled?: boolean;
}

export function SettingsForm({ schema, values, onSave, disabled }: SettingsFormProps) {
  const categories = [...new Set(schema.map((s) => s.category))];

  return (
    <div className="space-y-8">
      {categories.map((category) => (
        <section key={category}>
          <h3 className="mb-3 text-sm font-semibold text-muted-foreground uppercase tracking-wide">
            {category}
          </h3>
          <div className="divide-y divide-border rounded-md border border-border">
            {schema
              .filter((s) => s.category === category)
              .map((def) => (
                <SettingRow
                  key={def.key}
                  def={def}
                  value={values[def.key] ?? def.default}
                  onSave={onSave}
                  disabled={disabled}
                />
              ))}
          </div>
        </section>
      ))}
    </div>
  );
}

interface SettingRowProps {
  def: SettingDef;
  value: unknown;
  onSave: (key: string, value: unknown) => Promise<void>;
  disabled?: boolean;
}

function SettingRow({ def, value, onSave, disabled }: SettingRowProps) {
  const [saving, setSaving] = useState(false);
  const isReadOnly = def.readOnly === true;

  const handleChange = async (newValue: unknown) => {
    if (isReadOnly) return;
    setSaving(true);
    try {
      await onSave(def.key, newValue);
    } catch {
      // Error handled by parent (toast)
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 sm:gap-4 px-4 py-3">
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2">
          <label className="text-sm font-medium" htmlFor={def.key}>
            {def.label}
          </label>
          {isReadOnly && (
            <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-xs font-medium text-muted-foreground" title="This setting is managed by Helm. Edit the Helm values and upgrade to change it.">
              Managed by Helm
            </span>
          )}
        </div>
        <p className="text-xs text-muted-foreground mt-0.5">{def.description}</p>
      </div>
      <div className="shrink-0">
        <SettingControl def={def} value={value} onChange={handleChange} disabled={disabled || saving || isReadOnly} />
      </div>
    </div>
  );
}

interface SettingControlProps {
  def: SettingDef;
  value: unknown;
  onChange: (value: unknown) => void;
  disabled?: boolean;
}

function SettingControl({ def, value, onChange, disabled }: SettingControlProps) {
  switch (def.type) {
    case "bool":
      return <Toggle checked={value as boolean} onCheckedChange={onChange} disabled={disabled} id={def.key} />;
    case "int":
      return (
        <NumberInput
          value={value as number}
          onChange={onChange}
          min={def.min}
          max={def.max}
          disabled={disabled}
          id={def.key}
        />
      );
    case "enum":
      return (
        <Select
          value={value as string}
          onValueChange={onChange}
          options={def.enum ?? []}
          disabled={disabled}
          id={def.key}
        />
      );
    case "string":
      if (def.key === "preferredRuntime") {
        return (
          <RuntimeSelect
            id={def.key}
            value={value as string}
            onCommit={onChange}
            disabled={disabled}
          />
        );
      }
      return (
        <StringInput
          id={def.key}
          settingKey={def.key}
          value={value as string}
          onCommit={onChange}
          disabled={disabled}
          pattern={def.pattern}
          patternHint={def.default ? String(def.default) : undefined}
        />
      );
    case "strings":
      return (
        <TagInput
          id={def.key}
          value={Array.isArray(value) ? (value as string[]) : []}
          onChange={onChange}
          disabled={disabled}
        />
      );
    default:
      return <span className="text-xs text-muted-foreground">unsupported</span>;
  }
}

/** Text input that only commits on blur or Enter — avoids saving on every keystroke.
 *
 * On commit, the typed value is run through normalizeSettingValue() to
 * canonicalize unambiguous near-misses (e.g. "8gi" → "8Gi"). The
 * normalized form is then checked against `pattern`; if it matches,
 * onCommit fires with the normalized value AND the visible input is
 * updated so the user sees the auto-correction. If it doesn't match,
 * the value is left as-typed and an aria-invalid + helpful error is
 * shown.
 *
 * Without a `pattern`, behavior is unchanged (free-form input).
 *
 * The two-stage policy mirrors pkg/settings/Normalize() + Validate()
 * on the backend so a curl client and a UI client both produce the
 * same canonical wire payload. */
function StringInput({ id, settingKey, value, onCommit, disabled, placeholder, pattern, patternHint }: {
  id: string;
  settingKey?: string;
  value: string;
  onCommit: (value: unknown) => void;
  disabled?: boolean;
  placeholder?: string;
  pattern?: string;
  patternHint?: string;
}) {
  const [local, setLocal] = useState(value);
  const [error, setError] = useState<string | null>(null);
  const errorId = `${id}-error`;

  // Sync when value prop changes externally (e.g. API response)
  useEffect(() => {
    setLocal(value);
    setError(null);
  }, [value]);

  // Compile the pattern once per change — RegExp construction is cheap
  // and we want fresh state when the schema swaps the pattern.
  const re = pattern ? safeRegExp(pattern) : null;

  const commit = () => {
    if (local === value) {
      // No change — don't show an error for an untouched field.
      setError(null);
      return;
    }
    // Normalize first, then pattern-check. Lets unambiguous typos
    // ("8gi") get auto-corrected; leaves ambiguous/garbage to the
    // pattern rejection.
    const normalized = settingKey ? normalizeSettingValue(settingKey, local) : local;
    if (re && !re.test(normalized)) {
      setError(
        patternHint
          ? `Value does not match required format. Example: ${patternHint}`
          : `Value does not match required pattern: ${pattern}`,
      );
      return;
    }
    setError(null);
    if (normalized !== local) {
      // Show the user the canonicalized form they're committing.
      setLocal(normalized);
    }
    onCommit(normalized);
  };

  // Hint priority: explicit placeholder > pattern example > pattern itself.
  // The hint is what the user sees before typing, distinct from the
  // error which appears after a bad commit.
  const visibleHint = placeholder || patternHint || pattern || undefined;

  return (
    <div className="flex flex-col gap-1">
      <input
        id={id}
        type="text"
        value={local}
        onChange={(e) => {
          setLocal(e.target.value);
          // Clear stale error as soon as the user starts editing again.
          if (error) setError(null);
        }}
        onBlur={commit}
        onKeyDown={(e) => { if (e.key === "Enter") { e.currentTarget.blur(); } }}
        disabled={disabled}
        placeholder={visibleHint}
        title={pattern ? `Pattern: ${pattern}` : undefined}
        aria-invalid={error ? "true" : undefined}
        aria-describedby={error ? errorId : undefined}
        className={
          "h-8 w-full sm:w-48 rounded-md border bg-background px-2 text-sm focus:outline-none focus:ring-2 disabled:opacity-50 disabled:cursor-not-allowed " +
          (error
            ? "border-destructive focus:ring-destructive"
            : "border-border focus:ring-ring")
        }
      />
      {error && (
        <span id={errorId} role="alert" className="text-xs text-destructive">
          {error}
        </span>
      )}
    </div>
  );
}

/** Compile a regex from a (possibly user-supplied) pattern string.
 * Returns null if the pattern is invalid — in that case we degrade to
 * accepting anything rather than crashing the form. The Go-side
 * settings schema is the source of truth; an invalid pattern there is
 * a separate bug, caught by TestInstanceSettings_DefaultsPassValidation. */
function safeRegExp(pattern: string): RegExp | null {
  try {
    return new RegExp(pattern);
  } catch {
    return null;
  }
}

/** RuntimeSelect renders a dropdown of Ready image-factory configs for the
 * preferredRuntime user setting. Falls back to a plain text input if the
 * API is unavailable or returns no configs. */
function RuntimeSelect({ id, value, onCommit, disabled }: {
  id: string;
  value: string;
  onCommit: (v: unknown) => void;
  disabled?: boolean;
}) {
  const { data } = useQuery({
    queryKey: ["image-factory-configs"],
    queryFn: () => imageFactoryApi.listConfigs(),
    staleTime: 30_000,
  });
  const readyConfigs = (data?.configs ?? []).filter((c) => c.status === "ready");

  if (readyConfigs.length === 0) {
    return (
      <input
        id={id}
        type="text"
        value={value}
        onChange={(e) => onCommit(e.target.value)}
        disabled={disabled}
        placeholder="No custom images available — using default"
        className="h-8 w-full sm:w-48 rounded-md border border-border bg-background px-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring disabled:opacity-50"
      />
    );
  }

  return (
    <select
      id={id}
      value={value}
      onChange={(e) => onCommit(e.target.value)}
      disabled={disabled}
      className="h-8 w-full sm:w-48 rounded-md border border-border bg-background px-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring disabled:opacity-50"
    >
      <option value="">Use default</option>
      {readyConfigs.map((cfg) => (
        <option key={cfg.id} value={cfg.hash}>
          {cfg.name}
        </option>
      ))}
    </select>
  );
}
