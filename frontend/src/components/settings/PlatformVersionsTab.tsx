import { useState, useEffect, useCallback } from "react";
import { platformInfoApi, type PlatformInfo } from "../../api/platformInfo";
import { Spinner } from "../ui/Spinner";

const COMPONENTS: { key: keyof PlatformInfo; label: string }[] = [
  { key: "api", label: "API" },
  { key: "controller", label: "Controller" },
  { key: "frontend", label: "Frontend" },
  { key: "relayRouter", label: "Relay Router" },
  { key: "baseRuntime", label: "Base Runtime" },
];

export function PlatformVersionsTab() {
  const [info, setInfo] = useState<PlatformInfo | null>(null);
  const [error, setError] = useState(false);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    try {
      const data = await platformInfoApi.get();
      setInfo(data);
      setError(false);
    } catch {
      setError(true);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  if (loading) return <Spinner />;

  if (error) {
    return (
      <div className="space-y-2">
        <p className="text-sm text-destructive">Failed to load platform versions.</p>
        <button
          onClick={load}
          className="text-sm text-accent underline hover:text-foreground"
        >
          Retry
        </button>
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold">Platform Versions</h2>
        <p className="text-sm text-muted-foreground">
          Running component versions, read from the deployed image tags.
        </p>
      </div>
      <div className="overflow-hidden rounded-md border border-border">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-border bg-muted/50">
              <th className="px-4 py-2 text-left font-medium">Component</th>
              <th className="px-4 py-2 text-left font-medium">Version</th>
            </tr>
          </thead>
          <tbody>
            {COMPONENTS.map(({ key, label }) => (
              <tr key={key} className="border-b border-border last:border-0">
                <td className="px-4 py-2">{label}</td>
                <td className="px-4 py-2 font-mono">
                  {info?.[key] || <span className="text-muted-foreground">unknown</span>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
