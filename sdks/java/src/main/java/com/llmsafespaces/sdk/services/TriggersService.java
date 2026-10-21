package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.List;
import java.util.Map;

/**
 * Trigger CRUD (/me/triggers).
 *
 * <p>The #1812 inline gate script: a trigger may carry {@code script}
 * — an INLINE script object {@code {"language": "python"|"node",
 * "handler": <source>}} (never a path; the path form is the separate,
 * legacy scriptPath). The handler defines
 * {@code handler(input) -> {"proceed": <bool>, "vars": {<str>: <str>}}}
 * and runs in the routine's target workspace before every fire's LLM
 * call, with the fire envelope as input:</p>
 *
 * <ul>
 *   <li>{@code proceed=false} exits the fire with NO LLM call (cheap
 *       deterministic gating — poll checks, thresholds, state diffs);</li>
 *   <li>{@code proceed=true} fires the prompt with vars substituted
 *       jinja-style ({@code {{key}}}; unmatched tokens stay literal) and
 *       ALL vars appended as trailing {@code [trigger script vars]}
 *       metadata;</li>
 *   <li>any script failure — runtime error, unparseable output, or a
 *       return violating the schema — fails CLOSED: failed fire, the
 *       named error in the fire audit, no LLM call;</li>
 *   <li>a script trigger with NO prompt never calls the LLM at all
 *       (gate + side effects only).</li>
 * </ul>
 *
 * <p>Routine targets only — rejected (400) alongside workflowId (a
 * workflow's DAG carries its own script/condition nodes).</p>
 *
 * <p>Worked example — nightly digest, LLM called only when work
 * accumulated:</p>
 * <pre>{@code
 * triggers.create(
 *     "nightly-digest", "cron",
 *     Map.of("expr", "0 2 * * *", "tz", "UTC"),
 *     wsId, null,                       // workspace, no workflow
 *     "Summarize the {{count}} new items.", null,
 *     null, null, null, null, null, null, null,   // legacy fields
 *     Map.of("language", "python",
 *            "handler", "def handler(input):\n"
 *                     + "    n = count_open_items()\n"
 *                     + "    return {\"proceed\": n > 0, \"vars\": {\"count\": str(n)}}\n"));
 * // proceed=false -> fire skipped, no model call
 * // proceed=true  -> prompt fires with {{count}} substituted
 * //                + all vars as trailing metadata
 * }</pre>
 */
public class TriggersService {
    private final LLMSafeSpacesClient c;

    public TriggersService(LLMSafeSpacesClient c) { this.c = c; }

    public List<Map<String, Object>> list() {
        Map<String, Object> resp = c.request("GET", "/me/triggers", null,
                new TypeToken<Map<String, Object>>(){}.getType());
        Object triggers = resp.get("triggers");
        if (triggers instanceof List) {
            @SuppressWarnings("unchecked")
            List<Map<String, Object>> result = (List<Map<String, Object>>) triggers;
            return result;
        }
        return List.of();
    }

    /**
     * Create a trigger.
     *
     * @param script the #1812 inline gate script as
     *               {@code Map.of("language", "python", "handler", src)}
     *               (see the class javadoc for the {proceed, vars} gate
     *               semantics), or {@code null} for a prompt-only trigger.
     *               Routine targets only — rejected alongside workflowId.
     *               Appended AFTER preserveSession so existing positional
     *               callers keep compiling.
     */
    public Map<String, Object> create(String name, String sourceType, Object sourceConfig,
                                       String workspaceId, String workflowId, String prompt,
                                       String agent, String scriptPath, List<String> scriptArgs,
                                       Object scriptEnv, String memoryMode, String captureMode,
                                       String preserveSession, Object script) {
        Map<String, Object> body = new java.util.LinkedHashMap<>();
        body.put("name", name);
        body.put("sourceType", sourceType);
        body.put("sourceConfig", sourceConfig);
        if (workspaceId != null && !workspaceId.isEmpty()) body.put("workspaceId", workspaceId);
        if (workflowId != null && !workflowId.isEmpty()) body.put("workflowId", workflowId);
        if (prompt != null && !prompt.isEmpty()) body.put("prompt", prompt);
        if (agent != null && !agent.isEmpty()) body.put("agent", agent);
        if (scriptPath != null && !scriptPath.isEmpty()) body.put("scriptPath", scriptPath);
        if (scriptArgs != null) body.put("scriptArgs", scriptArgs);
        if (scriptEnv != null) body.put("scriptEnv", scriptEnv);
        if (memoryMode != null && !memoryMode.isEmpty()) body.put("memoryMode", memoryMode);
        if (captureMode != null && !captureMode.isEmpty()) body.put("captureMode", captureMode);
        if (preserveSession != null && !preserveSession.isEmpty()) body.put("preserveSession", preserveSession);
        if (script != null) body.put("script", script);
        return c.request("POST", "/me/triggers", body,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    /**
     * Partially update a trigger (omitted keys keep their values).
     *
     * <p>The #1812 gate script patches with key-presence semantics: an
     * absent {@code script} key keeps the stored script, the JSON
     * literal {@code null} clears it back to no-script, and an object
     * {@code {"language": ..., "handler": ...}} replaces it. Put exactly
     * one of those three spellings in the map — never a Java {@code null}
     * value for "keep" (Gson drops null map values, which is the keep
     * case anyway; use the string {@code "null"} via a raw JSON body for
     * the clear case).</p>
     */
    public Map<String, Object> update(String id, Map<String, Object> updates) {
        return c.request("PUT", "/me/triggers/" + id, updates,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/me/triggers/" + id, null);
    }
}
