// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Prompts settings tab (#1499): the user's saved-prompt library —
// list, create/edit, delete-with-confirm. The composer #-recall popup
// (#1496) reads the same data through its own adapter.

import { useEffect, useState } from "react";
import { userPromptsApi } from "../../api/userPrompts";
import type { UserPrompt } from "../../api/userPrompts";
import { useToast } from "../../providers/ToastProvider";
import { Spinner } from "../ui/Spinner";
import { FileText, Trash2, Plus, Pencil, X } from "lucide-react";

type EditingState =
  | { mode: "closed" }
  | { mode: "create" }
  | { mode: "edit"; prompt: UserPrompt };

export function PromptsTab() {
  const [prompts, setPrompts] = useState<UserPrompt[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<EditingState>({ mode: "closed" });
  const [confirmDelete, setConfirmDelete] = useState<UserPrompt | null>(null);
  const { toast } = useToast();

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await userPromptsApi.list();
      setPrompts(data.prompts);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to load prompts");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    load();
  }, []);

  const handleSaved = (saved: UserPrompt, wasCreate: boolean) => {
    setPrompts((prev) =>
      wasCreate ? [saved, ...prev] : prev.map((p) => (p.id === saved.id ? saved : p)),
    );
    setEditing({ mode: "closed" });
    toast(wasCreate ? `Created "${saved.name}"` : `Saved "${saved.name}"`);
  };

  const handleDelete = async (prompt: UserPrompt) => {
    try {
      await userPromptsApi.delete(prompt.id);
      setPrompts((prev) => prev.filter((p) => p.id !== prompt.id));
      setConfirmDelete(null);
      toast(`Deleted "${prompt.name}"`);
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Delete failed", "error");
    }
  };

  if (loading) return <div className="flex justify-center p-8"><Spinner /></div>;

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-lg font-medium">Prompts</h3>
          <p className="text-sm text-muted-foreground">
            Your saved prompts — recall them in the composer with #.
          </p>
        </div>
        <button
          onClick={() => setEditing({ mode: "create" })}
          disabled={editing.mode !== "closed"}
          className="flex items-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-xs hover:bg-accent disabled:opacity-50"
        >
          <Plus className="h-3 w-3" /> New prompt
        </button>
      </div>

      {error && (
        <div className="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
          {error}
        </div>
      )}

      {editing.mode !== "closed" && (
        <PromptEditor
          key={editing.mode === "edit" ? editing.prompt.id : "create"}
          initial={editing.mode === "edit" ? editing.prompt : null}
          onCancel={() => setEditing({ mode: "closed" })}
          onSaved={(saved, wasCreate) => handleSaved(saved, wasCreate)}
        />
      )}

      {prompts.length === 0 && editing.mode === "closed" && !error && (
        <p className="text-sm text-muted-foreground">
          No saved prompts yet — create one to recall it in the composer.
        </p>
      )}

      <ul className="space-y-2">
        {prompts.map((p) => (
          <li
            key={p.id}
            className="flex items-start justify-between gap-3 rounded-md border border-border p-3"
            data-testid={`prompt-row-${p.id}`}
          >
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <FileText className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                <span className="truncate text-sm font-medium">{p.name}</span>
              </div>
              <p className="mt-1 line-clamp-2 whitespace-pre-wrap text-xs text-muted-foreground">
                {p.content}
              </p>
            </div>
            <div className="flex shrink-0 gap-1">
              <button
                aria-label={`Edit ${p.name}`}
                onClick={() => setEditing({ mode: "edit", prompt: p })}
                className="rounded-md p-1.5 hover:bg-accent"
              >
                <Pencil className="h-3.5 w-3.5" />
              </button>
              <button
                aria-label={`Delete ${p.name}`}
                onClick={() => setConfirmDelete(p)}
                className="rounded-md p-1.5 text-destructive hover:bg-accent"
              >
                <Trash2 className="h-3.5 w-3.5" />
              </button>
            </div>
          </li>
        ))}
      </ul>

      {confirmDelete && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40" role="dialog">
          <div className="w-96 rounded-md border border-border bg-background p-4 shadow-lg">
            <h4 className="text-sm font-medium">Delete "{confirmDelete.name}"?</h4>
            <p className="mt-1 text-xs text-muted-foreground">This cannot be undone.</p>
            <div className="mt-4 flex justify-end gap-2">
              <button
                onClick={() => setConfirmDelete(null)}
                className="rounded-md border border-border px-3 py-1.5 text-xs hover:bg-accent"
              >
                Cancel
              </button>
              <button
                onClick={() => handleDelete(confirmDelete)}
                className="rounded-md bg-destructive px-3 py-1.5 text-xs text-destructive-foreground"
              >
                Delete
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function PromptEditor({
  initial,
  onCancel,
  onSaved,
}: {
  initial: UserPrompt | null;
  onCancel: () => void;
  onSaved: (saved: UserPrompt, wasCreate: boolean) => void;
}) {
  const [name, setName] = useState(initial?.name ?? "");
  const [content, setContent] = useState(initial?.content ?? "");
  const [saving, setSaving] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);

  const submit = async () => {
    const trimmed = name.trim();
    if (!trimmed || !content.trim()) {
      setFormError("Name and content are required.");
      return;
    }
    setSaving(true);
    setFormError(null);
    try {
      const res = initial
        ? await userPromptsApi.update(initial.id, { name: trimmed, content })
        : await userPromptsApi.create({ name: trimmed, content });
      onSaved(res.prompt, initial === null);
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : "Save failed";
      setFormError(msg.includes("prompt_name_taken") ? "A prompt with that name already exists." : msg);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="space-y-3 rounded-md border border-border p-4" data-testid="prompt-editor">
      <div className="flex items-center justify-between">
        <h4 className="text-sm font-medium">{initial ? "Edit prompt" : "New prompt"}</h4>
        <button onClick={onCancel} aria-label="Close editor" className="rounded-md p-1 hover:bg-accent">
          <X className="h-3.5 w-3.5" />
        </button>
      </div>
      <input
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder="Prompt name"
        maxLength={100}
        className="w-full rounded-md border border-border bg-background px-3 py-2 text-sm"
        aria-label="Prompt name"
      />
      <textarea
        value={content}
        onChange={(e) => setContent(e.target.value)}
        placeholder="Prompt content — recalled verbatim from the composer"
        rows={6}
        className="w-full rounded-md border border-border bg-background px-3 py-2 font-mono text-xs"
        aria-label="Prompt content"
      />
      {formError && <p className="text-xs text-destructive">{formError}</p>}
      <div className="flex justify-end gap-2">
        <button onClick={onCancel} className="rounded-md border border-border px-3 py-1.5 text-xs hover:bg-accent">
          Cancel
        </button>
        <button
          onClick={submit}
          disabled={saving}
          className="rounded-md bg-primary px-3 py-1.5 text-xs text-primary-foreground disabled:opacity-50"
        >
          {saving ? "Saving…" : initial ? "Save" : "Create"}
        </button>
      </div>
    </div>
  );
}
