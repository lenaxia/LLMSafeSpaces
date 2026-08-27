import { useCallback, useEffect, useState, useSyncExternalStore } from "react";
import { settingsApi } from "../api/settings";

const STORAGE_KEY = "llmsafespaces_user_settings";

// --- Shared in-memory store (singleton) ---

type Listener = () => void;

let cache: Record<string, unknown> = loadFromStorage();
const listeners: Set<Listener> = new Set();

function loadFromStorage(): Record<string, unknown> {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return raw ? JSON.parse(raw) : {};
  } catch {
    return {};
  }
}

function getSnapshot(): Record<string, unknown> {
  return cache;
}

function subscribe(listener: Listener): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function notify() {
  for (const l of listeners) l();
}

function updateCache(next: Record<string, unknown>) {
  cache = next;
  localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
  notify();
}

/** Reset internal cache from localStorage. Exported for test isolation. */
export function _resetStoreFromStorage() {
  cache = loadFromStorage();
  notify();
}

/** Update the shared store directly. Used by ThemeProvider to sync API data. */
export function _updateSettingsCache(settings: Record<string, unknown>) {
  updateCache(settings);
}

// --- Hooks ---

/** Reads user settings with localStorage-first strategy (instant render, API sync on mount).
 * All consumers share the same reactive state — changes propagate immediately. */
export function useUserSettings() {
  const settings = useSyncExternalStore(subscribe, getSnapshot);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    settingsApi.getUserSettings()
      .then((res) => { updateCache(res.settings); })
      .catch(() => {}) // Use cached values on failure
      .finally(() => setLoading(false));
  }, []);

  const setSetting = useCallback(async (key: string, value: unknown) => {
    // Optimistic local update — all subscribers re-render immediately
    updateCache({ ...cache, [key]: value });
    // Persist to API
    await settingsApi.setUserSetting(key, value);
  }, []);

  return { settings, loading, setSetting };
}

/** Reactive single-setting hook. Re-renders when the setting changes anywhere in the app. */
export function useUserSetting<T>(key: string, defaultValue: T): T {
  const settings = useSyncExternalStore(subscribe, getSnapshot);
  return (settings[key] as T) ?? defaultValue;
}

/** Write a single user setting: optimistic local update + API persist.
 * Same contract as useUserSettings().setSetting without the mount fetch. */
export async function setUserSetting(key: string, value: unknown): Promise<void> {
  updateCache({ ...cache, [key]: value });
  await settingsApi.setUserSetting(key, value);
}
