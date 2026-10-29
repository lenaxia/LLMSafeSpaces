# Gate-script patterns — the worked forms

`trigger_create`'s `script` field runs an inline gate before every fire's LLM
call. The contract (return `{"proceed": <bool>, "vars": {<str>: <str>}}`;
`proceed=false` exits with no LLM call; unhandled failures fail closed and
count toward auto-disable) is stated in the tool description. This doc carries
the WORKED patterns — the shapes the first real live-fire (a PR-review
watcher, 2026-10-29) validated, including every external-contract trap it hit.

Audience: you, the agent authoring a gated trigger in this pod. Each pattern
is copy-pasteable Python (the idioms are language-neutral; node handlers
follow the same shapes). The full worked example at the bottom composes them.

## Pattern 1 — External HTTP polling: fetch → NORMALIZE → evaluate

The gate's whole job is usually "ask an external system, decide on the
answer". Three rules, all learned the hard way:

1. **Normalize every value you compare.** External APIs return what they
   return — lowercase states, nulls, numbers-as-strings. `str(x).lower()`
   before comparing, and never compare against a constant you invented
   (e.g. expecting `"APPROVED"` when the API returns `"approved"` — the
   comparison is silently always-false and the gate never opens).
2. **Read the API's actual unauthenticated contract, not its documented
   authenticated one.** The canonical case: GitHub's anon REST API returns
   `"review_decision": null` on `GET /pulls/{n}` — the field needs auth.
   The unauthenticated truth for PR review state is
   `GET /pulls/{n}/reviews`: take the LAST element (chronological), read
   its `state` (`"approved"` / `"changes_requested"` / `"commented"` —
   lowercase) and `submitted_at`.
3. **Budget the cadence against the shared anon rate limit.** The 60/hour
   unauthenticated budget is per-IP and SHARED by every anonymous caller
   riding the same egress IP — your trigger is one tenant of it. Budget
   `calls-per-fire × fires-per-hour ≤ ~40` (leave a third of the budget
   as headroom). A 15-minute cadence with 2 calls per fire is 8/h — safe.
   A rate-limit 403 is a transient fetch failure: take your failure
   posture (pattern 4), do not count it as a verdict.

Always set a timeout on the fetch — a hung socket burns the fire for
nothing (`urlopen(url, timeout=10)`).

## Pattern 2 — Conditional requests: 304 RAISES; ETags can pin stale bodies

If you opt into `If-None-Match`/ETag conditional requests:

- **`urlopen` THROWS on 304.** Python turns "not modified" into
  `urllib.error.HTTPError` with `e.code == 304`. That exception IS the
  success/no-change path — catch it explicitly:

  ```python
  try:
      with urllib.request.urlopen(url, timeout=10) as r:
          body = json.load(r)
  except urllib.error.HTTPError as e:
      if e.code == 304:
          return {"proceed": False, "vars": {}}   # not modified — nothing to do
      raise
  ```

- **A saved ETag can pin stale CDN content permanently.** CDN-fronted anon
  APIs may serve a stale body WITH its ETag; you then re-submit that ETag
  forever, keep getting 304s, and a real change stays invisible — the
  verdict arrives but your gate never sees it. For CDN-fronted anonymous
  endpoints, prefer PLAIN fetches + marker dedup (pattern 3) over ETag
  diffing: one cheap request per fire, no cache-layer coupling, the
  observation is always fresh.

## Pattern 3 — State dedup across fires: the marker-file signature

Fires have NO memory — the handler starts from scratch every interval.
Without dedup, a condition-true gate re-fires the LLM leg every interval
forever. The marker-file signature idiom:

1. Build a **signature** of the observed state — the minimal string that
   changes when the thing you watch changes (e.g.
   `"<last review state>@<submitted_at>"`).
2. Write it to a marker file on the PVC (`/workspace/...` — survives pod
   restarts; `/sandbox-runtime/...` is tmpfs and dies with the pod).
3. Skip (quiet `proceed=false`) when the file already holds the same
   signature.

```python
def observed_previously(marker: str, sig: str) -> bool:
    try:
        with open(marker) as f:
            return f.read() == sig
    except OSError:
        return False   # absent/unreadable marker = never observed

def mark_observed(marker: str, sig: str) -> None:
    try:
        os.makedirs(os.path.dirname(marker), exist_ok=True)
        with open(marker, "w") as f:
            f.write(sig)
    except OSError:
        pass   # unwritable marker: worst case is one extra fire — acceptable
```

Mark-on-observation means **at-most-once relay per state change**: if the
LLM leg itself fails after the gate opens, the fire shows `failed` in
`trigger_fires` (diagnosable, not silently retried). That is the right
trade for a relay; if you need retry-until-delivered instead, mark only
on non-relay observations and accept a re-fire per interval while the
condition stays true.

## Pattern 4 — Failure posture: choose it, catch it, state it

Unhandled exceptions fail CLOSED — the fire is `failed`, the error lands
in `trigger_fires`, and it counts toward `autoDisableAfter`. That is the
platform invariant. Your POLICY on expected failures is a choice; make it
by catching and returning a verdict yourself:

- **Quiet (`proceed=false`)** — the common poller case: a transient fetch
  failure, a timeout, a rate-limit 403. The next interval retries
  naturally; no LLM call, no noise, no failed-fire accounting.
- **Fail-open (`proceed=true`)** — "if I can't verify, wake someone": for
  gates whose whole value is catching a problem, silence is the failure
  mode. Route the wake-up through `vars` (pattern 5) so the LLM leg says
  WHAT could not be verified.

Wrap the handler body in one catch-all; a comment states the choice:

```python
def handler(input):
    try:
        return observe_and_decide(input)
    except Exception as e:
        # quiet poller: transient errors skip this fire; the schedule is the retry
        return {"proceed": False, "vars": {"skipped_error": str(e)}}
```

## Pattern 5 — Self-contained action prompts

The LLM leg is a FRESH context — it sees the prompt (with `{{key}}` vars
substituted) and nothing else. Exactly like a `create_session` prompt, it
must carry everything needed to act: the exact relay text, the exact
destination, the exact URLs/SHAs. Build the whole message in the handler
where you still have the data; never ask the LLM leg to re-derive or
"look up" what the gate already observed.

## The canonical worked example — a PR-review watcher

Composes all five patterns: polls a PR's review state anonymously,
dedups on the observed signature, relays once when the verdict lands,
quiet on everything transient.

```python
import json, os, urllib.request

REVIEWS = "https://api.github.com/repos/OWNER/REPO/pulls/1985/reviews"
PR_URL  = "https://github.com/OWNER/REPO/pull/1985"
MARKER  = "/workspace/gate-markers/pr-1985-last-review.txt"
RELAY_TO = "crimson-canyon-relays"   # exact destination session phrase

def handler(input):
    try:
        # pattern 2: PLAIN fetch — no ETag opt-in, no 304 exception path,
        # no CDN-stale pinning; one call per fire.
        with urllib.request.urlopen(REVIEWS, timeout=10) as r:
            reviews = json.load(r)

        if not reviews:
            return {"proceed": False, "vars": {}}
        last = reviews[-1]                                   # chronological truth
        state = str(last.get("state", "")).lower()           # pattern 1: NORMALIZE
        submitted = str(last.get("submitted_at", ""))

        sig = state + "@" + submitted                        # pattern 3: signature
        if observed_previously(MARKER, sig):
            return {"proceed": False, "vars": {}}            # unchanged — quiet
        mark_observed(MARKER, sig)                           # at-most-once per change

        if state != "approved":                              # a change, not THE change
            return {"proceed": False, "vars": {}}

        # pattern 5: the LLM leg is a fresh context — carry EVERYTHING.
        message = (
            "The PR-review watcher fired: " + PR_URL +
            " got an APPROVED review at " + submitted + ". " +
            "Send a message to session " + RELAY_TO + " with exactly this text: " +
            "'PR " + PR_URL + " is APPROVED — report APPROVED, the head SHA, " +
            "and the PR number to the orchestrator.'"
        )
        return {"proceed": True, "vars": {
            "message": message, "relay_to": RELAY_TO, "pr": PR_URL,
        }}
    except Exception as e:
        # pattern 4: quiet poller — transient fetch errors skip this fire;
        # the schedule is the retry. (Choose fail-open here instead if a
        # SILENT watcher is the worse failure mode for your use case.)
        return {"proceed": False, "vars": {"skipped_error": str(e)}}
```

Cadence: one request per fire; `*/15` cron = 4 calls/h — comfortably
inside the ~40/h budget rule (pattern 1, rule 3).
