# Binary zod schema extract — opencode 1.18.15 ACP layer

**Provenance:** extracted from the pinned binary `/opencode/usr/local/bin/opencode` (sha of extraction command below) via
`strings -n 12 /opencode/usr/local/bin/opencode | grep -n "request_permission"` — hit #96548. The binary bundles its ACP
layer as minified JS with inline zod schemas; the excerpt below is the verbatim region covering the method tables, the
`session/update` union, the permission result schema, and the request/notification dispatch tables. Minified variable
names are the binary's own (`f` = zod). This file makes the doc's `[zod]` citations auditable without re-running strings.

---

## Method name tables (verbatim)

Client→agent (requests):
```
G={authenticate:"authenticate",document_did_change:"document/didChange",document_did_close:"document/didClose",
document_did_focus:"document/didFocus",document_did_open:"document/didOpen",document_did_save:"document/didSave",
initialize:"initialize",logout:"logout",nes_accept:"nes/accept",nes_close:"nes/close",nes_reject:"nes/reject",
nes_start:"nes/start",nes_suggest:"nes/suggest",providers_disable:"providers/disable",providers_list:"providers/list",
providers_set:"providers/set",session_cancel:"session/cancel",session_close:"session/close",session_fork:"session/fork",
session_list:"session/list",session_load:"session/load",session_new:"session/new",session_prompt:"session/prompt",
session_resume:"session/resume",session_set_config_option:"session/set_config_option",session_set_mode:"session/set_mode",
session_set_model:"session/set_model"}
```

Agent→client:
```
Q={elicitation_complete:"elicitation/complete",elicitation_create:"elicitation/create",
fs_read_text_file:"fs/read_text_file",fs_write_text_file:"fs/write_text_file",
session_request_permission:"session/request_permission",session_update:"session/update",
terminal_create:"terminal/create",terminal_kill:"terminal/kill",terminal_output:"terminal/output",
terminal_release:"terminal/release",terminal_wait_for_exit:"terminal/wait_for_exit"}
```

## The sessionUpdate union (verbatim)

```
Rk=f.union([
 j.and(f.object({sessionUpdate:f.literal("user_message_chunk")})),
 j.and(f.object({sessionUpdate:f.literal("agent_message_chunk")})),
 j.and(f.object({sessionUpdate:f.literal("agent_thought_chunk")})),
 Sk.and(f.object({sessionUpdate:f.literal("tool_call")})),
 qf.and(f.object({sessionUpdate:f.literal("tool_call_update")})),
 O$.and(f.object({sessionUpdate:f.literal("plan")})),
 Lk.and(f.object({sessionUpdate:f.literal("available_commands_update")})),
 i$.and(f.object({sessionUpdate:f.literal("current_mode_update")})),
 E$.and(f.object({sessionUpdate:f.literal("config_option_update")})),
 l$.and(f.object({sessionUpdate:f.literal("session_info_update")})),
 Ek.and(f.object({sessionUpdate:f.literal("usage_update")}))])
```

## Permission round-trip schemas (verbatim — the doc's §5.1 basis)

Request options and result:
```
f$=f.union([f.literal("allow_once"),f.literal("allow_always"),f.literal("reject_once"),f.literal("reject_always")]),
b$=f.object({_meta:…,kind:f$,name:f.string(),optionId:If}),            // If = f.string()
j$=f.object({_meta:…,optionId:If}),
C$=f.union([f.object({outcome:f.literal("cancelled")}),j$.and(f.object({outcome:f.literal("selected")}))]),
L$=f.object({_meta:…,outcome:C$})
```
i.e. result = `{outcome:{outcome:"selected",optionId:<string>}}` — **singular `optionId`**, `"cancelled"` double-l.

Request params (`hf`): `{_meta?, options: b$[], sessionId, toolCall: qf}` where `qf` = `{_meta?, content?(content|diff|terminal)[], kind?, locations?[{path,line?}], rawInput?, rawOutput?, status?, title?, toolCallId}`.

## Dispatch tables — which methods accept requests vs notifications only (verbatim structure)

Request handler (`B`): initialize, session_new, session_load, session_list, session_fork, session_resume, session_close,
session_set_mode, authenticate, providers_list/set/disable, logout, session_prompt, session_set_model,
session_set_config_option, nes_start/suggest/close.

Notification handler (`J`): **session_cancel**, document_did_open/did_change/did_close/did_save/did_focus, nes_accept/reject.

Guards worth noting (verbatim pattern): `case G.session_fork:{if(!b.unstable_forkSession)throw _.methodNotFound(F);…}` — the
TS-namespace `unstable_` naming maps to wire methods session/fork, session/set_model, providers/*, logout, nes/*,
document/* (the families the doc's §8 register lists as unstable_*).

## Fork params schema (the doc's §5.2 basis — handler stricter than library)

```
d=f.object({_meta:…,additionalDirectories:f.array(f.string()).optional(),cwd:f.string().nullish(),
mcpServers:f.array(T).optional(),sessionId:w,title:f.string().nullish(),updatedAt:f.string().nullish()})
```
`cwd` is `nullish` (optional) in the library schema, yet the live server rejects fork without it (-32602) — see
transcript-lifecycle.ndjson (23.69) vs transcript-fork.ndjson (13.86).

## session/list params (the doc's §5.3 basis)

```
z=f.object({_meta:…,additionalDirectories:f.array(f.string()).optional(),cursor:f.string().nullish(),cwd:f.string().nullish(),limit:…uint32…nullish()})
```
`limit` exists in the schema; the live server ignores it (24 sessions returned for `{limit:10}` — transcript-lifecycle.ndjson 23.68).

## ContentBlock / toolCall kind / stopReason unions (verbatim)

```
$f=union(text | image{data,mimeType} | audio{data,mimeType} | resource_link{name,size?,title?,uri,description?,mimeType?} | resource{resource{text|mimeType,uri} | {blob,mimeType,uri}})
cf=union("read"|"edit"|"delete"|"move"|"search"|"execute"|"think"|"fetch"|"switch_mode"|"other")
kk=union("end_turn"|"max_tokens"|"max_turn_requests"|"refusal"|"cancelled")
vf=union("pending"|"in_progress"|"completed"|"failed")   // toolCall status
```
