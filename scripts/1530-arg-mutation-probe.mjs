// 1530-arg-mutation-probe.mjs — issue #1530 investigation harness.
//
// Question (the orchestrator's hypothesis): does the origin plugin's
// in-place arg mutation (llmsafespaces-origin.js line 49,
// `output.args.lsp_injected_session = input.sessionID`) race the
// MCP serializer and duplicate/corrupt the id fields on the wire?
//
// Method: replicate the PINNED opencode 1.18.15 dispatch exactly —
//   plugin/index.ts trigger(): `for (const hook of s.hooks)
//   await fn(input, output)` — sequential, awaited, ONE shared
//   `output` object per call site.
//   session/tools.ts:106-111: trigger("tool.execute.before",
//   {tool, sessionID, callID}, {args}); then item.execute(args) —
//   the SAME args object reference flows hook → execute → (wire) →
//   after-hook/persistence.
// The wire leg is JSON.stringify(args) inside the MCP client's HTTP
// body — exactly what the SDK does before fetch.
//
// Legs:
//   A1 sequential:   N dual-param calls (model passed BOTH session_id
//                    and a stale lsp_injected_session — the observed
//                    misfire precondition). Assert: wire clean,
//                    injection OVERWRITES the stale value.
//   A2 concurrent:   M calls in flight (separate args objects, as the
//                    harness allocates per tool call). Assert: clean.
//   A3 aliased:      the adversarial worst case the aliasing permits —
//                    TWO dispatches sharing ONE args object,
//                    interleaved at await points. Stringify may see
//                    EITHER origin (snapshot ambiguity) but can NEVER
//                    see torn or duplicated JSON. Assert: every body
//                    parses; keys exactly {session_id,
//                    lsp_injected_session}; each value in the known
//                    set; session_id NEVER contains a JSON fragment.
//   A4 mutating-get: the literal "mutation racing the serializer" —
//                    a Proxy whose reads mutate the object mid-
//                    stringify. V8 stringify is synchronous: still
//                    well-formed JSON, just possibly inconsistent
//                    across snapshots. Assert: parses, no torn values.
//
// The observed corruption (instances 1-13): session_id arriving as
//   ses_TARGET","lsp_injected_session":"ses_ORIGIN"}
// — a value containing ESCAPED-quoted JSON tail. Backslash escapes
// exist only in JSON SOURCE TEXT. If no leg here can produce them,
// the corruption predates serialization: it was in the JSON the
// MODEL emitted (duplicating the tail of a similar call visible in
// context), and the plugin/serializer race is falsified.
//
// Usage: node scripts/1530-arg-mutation-probe.mjs [N]
// Exit 0 = all legs clean (race NOT reproduced). Exit 1 = a leg
// produced corrupt wire JSON — the race IS real; keep the failing
// leg as the red regression.

import { pathToFileURL } from "node:url"
import { argv, exit } from "node:process"

const REPO = new URL("..", import.meta.url).pathname
const plugin = (await import(pathToFileURL(REPO + "runtimes/opencode/plugins/llmsafespaces-origin.js"))).default

const N = Number(argv[2] ?? 10000)
const TARGET = "ses_target_0000"
const ORIGIN_A = "ses_origin_AAAA"
const ORIGIN_B = "ses_origin_BBBB"
const FRAGMENT_MARK = '","lsp_injected_session":"'

let failures = 0
const fail = (leg, detail) => {
  failures++
  console.error(`CORRUPT WIRE [${leg}]: ${detail}`)
}

// The harness-exact dispatch: sequential awaited hooks over one
// shared output object (plugin/index.ts:289-293).
async function dispatch(input, output) {
  const hooks = plugin()
  for (const hook of Object.values(hooks)) {
    await hook(input, output) // eslint-disable-line no-await-in-loop
  }
}

// The harness-exact trigger site for send_message
// (session/tools.ts:104-111): input carries tool/sessionID/callID,
// output wraps the args object the tool will receive.
function triggerArgs(args, sessionID) {
  return dispatch(
    { tool: "llmsafespaces_send_message", sessionID, callID: "call_probe" },
    { args },
  )
}

// The wire: what the MCP client serializes into the HTTP body.
const wire = (args) => JSON.stringify({ arguments: args })

// A1: sequential dual-param calls. Model supplied a stale
// lsp_injected_session (e.g. copied from a prior visible call);
// plugin must unconditionally overwrite it.
{
  let clean = 0
  for (let i = 0; i < N; i++) {
    const args = {
      session_id: TARGET,
      message: "probe",
      lsp_injected_session: "ses_MODEL_FORGED_stale",
    }
    await triggerArgs(args, ORIGIN_A)
    const body = wire(args)
    const parsed = JSON.parse(body)
    const keys = Object.keys(parsed.arguments)
    const a = parsed.arguments
    if (
      keys.length === 3 &&
      keys.filter((k) => k === "session_id").length === 1 &&
      keys.filter((k) => k === "lsp_injected_session").length === 1 &&
      a.session_id === TARGET &&
      a.lsp_injected_session === ORIGIN_A &&
      !a.session_id.includes(FRAGMENT_MARK)
    ) {
      clean++
    } else {
      fail("A1-sequential", body)
    }
  }
  console.log(`A1 sequential dual-param:      ${clean}/${N} clean, injection overwrites stale value`)
}

// A2: concurrent in-flight calls, per-call args objects.
{
  const M = Math.min(N, 500)
  let clean = 0
  const results = await Promise.all(
    Array.from({ length: M }, async (_, i) => {
      const args = { session_id: TARGET, message: `probe-${i}` }
      await triggerArgs(args, i % 2 ? ORIGIN_A : ORIGIN_B)
      return wire(args)
    }),
  )
  for (const body of results) {
    const a = JSON.parse(body).arguments
    if (
      a.session_id === TARGET &&
      (a.lsp_injected_session === ORIGIN_A || a.lsp_injected_session === ORIGIN_B) &&
      !a.session_id.includes(FRAGMENT_MARK)
    ) {
      clean++
    } else {
      fail("A2-concurrent", body)
    }
  }
  console.log(`A2 concurrent per-call args:   ${clean}/${M} clean`)
}

// A3: the aliasing worst case — ONE args object shared by TWO
// interleaved dispatches with different origins. The serializer may
// observe either origin (snapshot ambiguity is real) but never torn
// or duplicated JSON.
{
  const M = Math.min(N, 500)
  let clean = 0
  const shared = { session_id: TARGET, message: "aliased" }
  const dispatches = Promise.all([triggerArgs(shared, ORIGIN_A), triggerArgs(shared, ORIGIN_B)])
  // interleave serializers across the awaits of both dispatches
  const bodies = []
  for (let i = 0; i < M; i++) {
    bodies.push(wire(shared))
    await Promise.resolve() // yield to pending hook microtasks
  }
  await dispatches
  bodies.push(wire(shared))
  for (const body of bodies) {
    const a = JSON.parse(body).arguments
    const keys = Object.keys(a)
    if (
      keys.filter((k) => k === "session_id").length === 1 &&
      keys.filter((k) => k === "lsp_injected_session").length <= 1 &&
      a.session_id === TARGET &&
      (a.lsp_injected_session === undefined ||
        a.lsp_injected_session === ORIGIN_A ||
        a.lsp_injected_session === ORIGIN_B) &&
      !a.session_id.includes(FRAGMENT_MARK)
    ) {
      clean++
    } else {
      fail("A3-aliased", body)
    }
  }
  console.log(`A3 aliased shared args:        ${clean}/${bodies.length} clean (either-origin snapshots accepted)`)
}

// A4: mutation DURING stringify — the literal race, forced through a
// Proxy whose property reads mutate the target. V8's stringify runs
// synchronously on one thread: reads interleave with reads, never
// with the plugin's awaited writes. Output must still parse.
{
  let clean = 0
  const M = Math.min(N, 1000)
  for (let i = 0; i < M; i++) {
    const target = { session_id: TARGET, message: "proxy" }
    const adversarial = new Proxy(target, {
      get(t, prop, recv) {
        // mutate mid-flight: a hostile stand-in for the plugin write
        t.lsp_injected_session = i % 2 ? ORIGIN_A : ORIGIN_B
        return Reflect.get(t, prop, recv)
      },
    })
    const body = wire(adversarial)
    const a = JSON.parse(body).arguments
    if (a.session_id === TARGET && !a.session_id.includes(FRAGMENT_MARK)) {
      clean++
    } else {
      fail("A4-mutating-get", body)
    }
  }
  console.log(`A4 mutating-get mid-stringify: ${clean}/${M} clean`)
}

console.log(
  failures === 0
    ? "\nprobe verdict: RACE NOT REPRODUCED — no leg can emit duplicated keys or embedded escaped fragments; the wire cannot tear this way."
    : `\nprobe verdict: RACE REPRODUCED — ${failures} corrupt bodies; the failing leg is the red regression.`,
)
exit(failures === 0 ? 0 : 1)
