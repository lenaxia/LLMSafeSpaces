import { useEffect, useRef } from "react";
import { useAuth } from "../providers/AuthProvider";
import { settingsApi } from "../api/settings";

/**
 * Reports the browser's IANA timezone to the platform once per login
 * session (and on zone change). The API stores it as the user's
 * `timezone` setting and pushes it live to the user's workspace pods,
 * where the agent's get_datetime tool reports user-local time.
 *
 * Best-effort by design: failures are swallowed (the stored setting is
 * re-delivered on the next app load), and the effect never blocks the
 * UI — no state, no loading, no error surface.
 */
export function useTimezoneReporter() {
  const { user } = useAuth();
  const reported = useRef<string | null>(null);

  useEffect(() => {
    if (!user) return;

    const report = () => {
      let tz: string;
      try {
        tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
      } catch {
        return; // environment without Intl timezone support
      }
      if (!tz || tz === reported.current) return;
      reported.current = tz;
      settingsApi.setUserSetting("timezone", tz).catch(() => {
        // Un-deliver — retry on the next report (app reload / zone change).
        reported.current = null;
      });
    };

    report();
    // Browsers do not fire an event when the OS zone changes; poll at a
    // human cadence — cheap (an Intl format + string compare).
    const timer = window.setInterval(report, 60_000);
    return () => window.clearInterval(timer);
  }, [user]);
}
