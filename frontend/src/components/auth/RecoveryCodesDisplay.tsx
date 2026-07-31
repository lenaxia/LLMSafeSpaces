import { useState } from "react";
import { Button } from "../ui";

interface Props {
  codes: string[];
  onContinue: () => void;
}

export function RecoveryCodesDisplay({ codes, onContinue }: Props) {
  const [copied, setCopied] = useState(false);
  const [acknowledged, setAcknowledged] = useState(false);

  const handleCopy = () => {
    navigator.clipboard.writeText(codes.join("\n"));
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div className="flex flex-col gap-4">
      <div className="rounded-lg border border-yellow-500/50 bg-yellow-500/10 p-4">
        <p className="text-sm font-medium text-yellow-600 dark:text-yellow-400">
          Save your recovery codes
        </p>
        <p className="mt-1 text-xs text-muted-foreground">
          These codes are your only way back into your account if you lose your
          passkey. Store them somewhere safe — they will never be shown again.
        </p>
      </div>
      <div className="rounded-lg border border-border bg-muted/50 p-4">
        <div className="grid grid-cols-2 gap-2 font-mono text-sm">
          {codes.map((code, i) => (
            <span key={i} className="select-all">{code}</span>
          ))}
        </div>
      </div>
      <div className="flex gap-2">
        <Button variant="outline" onClick={handleCopy} className="flex-1">
          {copied ? "Copied!" : "Copy codes"}
        </Button>
      </div>
      <label className="flex items-center gap-2 text-sm select-none cursor-pointer">
        <input
          type="checkbox"
          checked={acknowledged}
          onChange={(e) => setAcknowledged(e.target.checked)}
          className="h-4 w-4 rounded border-border accent-primary cursor-pointer"
        />
        I've saved these codes somewhere safe
      </label>
      <Button onClick={onContinue} disabled={!acknowledged} className="w-full">
        Continue
      </Button>
    </div>
  );
}
