# US-63.1 Spike Report — V2 Session API Verification

**Date:** 2026-08-09
**Workspace:** a2703d3d-27c4-4980-86b1-42f99daad330 (ns llmsafespaces)
**Pod IP:** 127.0.0.1
**opencode port:** 18096
**Auth:** Basic `opencode:<password>` (Secret `workspace-pw-a2703d3d-27c4-4980-86b1-42f99daad330/password`)
**Raw request/response log:** appended to this file (search "Raw HTTP capture").

> This report is the empirical record for Epic 63. Every claim below was
> observed against a live pod; nothing is inferred from code reading.

- **Session ID:** `ses_018b529c6ffe36DzIffAUL4YOp`
### V2 prompt body shape (contract finding — load-bearing for US-63.2)

PASS on shape B `{prompt:{text:"..."}}`, FAIL on shape A `{prompt:{parts:[...]}}`.

- Parts-based (harness + US-63.1 spec assumption, Epic 65/design 0049): HTTP 400 — **REJECTED** by opencode 1.18.10 with `InvalidRequestError: Missing key at ["prompt"]["text"]`.
- Text-string `{prompt:{text:"..."}}`: HTTP 200 — **ACCEPTED**, returns `{data:{admittedSeq, id:"msg_...", sessionID, prompt:{text}, delivery, timeCreated}}`.

**Implication for US-63.2:** the proxy V2 client must POST `{"prompt":{"text":...}}`, NOT the parts-based shape. All subsequent phases of this spike use pb() which emits the working text shape. See raw HTTP capture for the exact 400/200 bodies.

### Idle-path V2 prompt (delivery:queue)

HTTP 200. If 2xx: queue-admit works on an idle session (US-63.3 happy path confirmed). See raw log for body.

### Busy-path V2 prompt (delivery:queue)

HTTP 200. PASS=2xx (admits without 409, US-63.3 confirmed). FAIL=409 (V2 not honoring queue admission; epic BLOCKED).

### Interrupt in-flight turn

HTTP 204. Per F8 the queued message B must STILL run after interrupt (non-destructive). Verify in Phase 6 events.

### Interrupt idle session

HTTP 204. Proxy should treat both 2xx and 4xx as success (US-63.4); record which we got.

### SSE event capture

Observed 10 event(s) on /event. Event type strings (unique):
  - server.connected
  - server.heartbeat
  - session.next.prompt.admitted
  - session.next.prompted
  - session.next.step.ended
  - session.next.step.started
  - session.next.text.delta
  - session.next.text.ended
  - session.next.text.started

PASS — the V2 admission/promotion events predicted by F13/F14 ARE present on the proxy's existing /event stream. US-63.5 can bridge these directly.

Per F14 the load-bearing strings are `session.next.prompt.admitted` (admitted, promoted_seq null) and `session.next.prompted` (promoted/run). A full turn lifecycle is also visible: `session.next.step.started` → `session.next.text.started` → `session.next.text.delta` → `session.next.text.ended` → `session.next.step.ended`. See raw SSE capture below for sample payloads.


## Raw SSE capture (Phase 5)

```
data: {"id":"evt_fe74af1ae00171S2wl5dpf4Nmc","type":"server.connected","properties":{}}

data: {"id":"evt_fe74af5a9001Te5wMrTgCYh5eI","type":"session.next.prompt.admitted","properties":{"messageID":"msg_fe74af5a80018za4MzMQnRz60Y","sessionID":"ses_018b529c6ffe36DzIffAUL4YOp","timestamp":"2026-08-09T16:11:17.289Z","prompt":{"text":"reply with: ack3"},"delivery":"queue"}}

data: {"id":"evt_fe74af5b7001faWq0yioM8Mt3e","type":"session.next.prompted","properties":{"sessionID":"ses_018b529c6ffe36DzIffAUL4YOp","timestamp":"2026-08-09T16:11:09.995Z","messageID":"msg_fe74ad92a00174clFQTJdSeBmD","prompt":{"text":"reply with the single word: ack"},"delivery":"queue"}}

data: {"id":"evt_fe74b15450027AJA4D4C4DBddx","type":"session.next.step.started","properties":{"sessionID":"ses_018b529c6ffe36DzIffAUL4YOp","agent":"build","model":{"id":"ling-3.0-tiny-free","providerID":"opencode"},"assistantMessageID":"msg_fe74b1545001XZ621V39WNOel0","timestamp":"2026-08-09T16:11:25.381Z"}}

data: {"id":"evt_fe74b154d001EzzQsvdnrkb0ZH","type":"session.next.text.started","properties":{"sessionID":"ses_018b529c6ffe36DzIffAUL4YOp","assistantMessageID":"msg_fe74b1545001XZ621V39WNOel0","timestamp":"2026-08-09T16:11:25.389Z","textID":"text-0"}}

data: {"id":"evt_fe74b1559001jQkSIQpGOx1JwR","type":"session.next.text.delta","properties":{"sessionID":"ses_018b529c6ffe36DzIffAUL4YOp","assistantMessageID":"msg_fe74b1545001XZ621V39WNOel0","timestamp":"2026-08-09T16:11:25.401Z","textID":"text-0","delta":"ack"}}

data: {"id":"evt_fe74b155e001Crf6949JLW6LTl","type":"session.next.text.ended","properties":{"sessionID":"ses_018b529c6ffe36DzIffAUL4YOp","assistantMessageID":"msg_fe74b1545001XZ621V39WNOel0","timestamp":"2026-08-09T16:11:25.406Z","textID":"text-0","text":"ack"}}

data: {"id":"evt_fe74b1565001NdvYmGB3vhfZL2","type":"session.next.step.ended","properties":{"sessionID":"ses_018b529c6ffe36DzIffAUL4YOp","timestamp":"2026-08-09T16:11:25.413Z","assistantMessageID":"msg_fe74b1545001XZ621V39WNOel0","finish":"stop","cost":0,"tokens":{"input":3328,"output":1,"reasoning":22,"cache":{"read":0,"write":0}}}}

data: {"id":"evt_fe74b1572001QK09m61dGzGpmy","type":"session.next.prompted","properties":{"sessionID":"ses_018b529c6ffe36DzIffAUL4YOp","timestamp":"2026-08-09T16:11:12.053Z","messageID":"msg_fe74ae1340014TeVzsg46cX0gH","prompt":{"text":"write a 200-word essay about the ocean"},"delivery":"queue"}}

data: {"id":"evt_fe74b18cb0015dEAIdElQNHeWj","type":"server.heartbeat","properties":{}}

curl: (28) Operation timed out after 12002 milliseconds with 2486 bytes received

```
### OOM-restart drain behavior (US-63.9 input)

FAIL (expected outcome per F16) — queued input E did NOT drain autonomously: no 'ack-oom-restart' in session history and no V2 admission/promotion activity on the post-restart SSE stream. US-63.9 MUST ship a drain trigger (upstream resume endpoint OR proxy-side wake) before US-63.7 deletes the stranded-queue sweep.


## Raw SSE capture (Phase 6 — post-restart drain watch, 60s)

```
data: {"id":"evt_fe74b41b8001GNiSJfSa0xXfr0","type":"server.connected","properties":{}}

data: {"id":"evt_fe74b68ec001mZQuoAmUXmyge4","type":"server.heartbeat","properties":{}}

data: {"id":"evt_fe74b8ffd001jrbNEEYNU8XnBN","type":"server.heartbeat","properties":{}}

data: {"id":"evt_fe74bb70e0015AK36zy90FX7tY","type":"server.heartbeat","properties":{}}

data: {"id":"evt_fe74bde1f001sgYJ2g52GmfGdA","type":"server.heartbeat","properties":{}}

data: {"id":"evt_fe74c0531001CXYo8nDbMRVOj4","type":"server.heartbeat","properties":{}}

data: {"id":"evt_fe74c2c42001V7p20PGnMBjY9z","type":"server.heartbeat","properties":{}}

curl: (28) Operation timed out after 62000 milliseconds with 623 bytes received

```

## Raw HTTP capture

```
==== prompt shape A: parts-based (harness/spec assumption) ====
URL: http://127.0.0.1:18096/api/session/ses_018b529c6ffe36DzIffAUL4YOp/prompt
ARGS: -X POST -H Content-Type: application/json -d {"prompt":{"parts":[{"type":"text","text":"shape-probe-parts"}]},"delivery":"queue"}
--- request body (if any) ---
--- response status: 400 ---
--- response headers ---
HTTP/1.1 400 Bad Request
Content-Type: application/json
Date: Sun, 09 Aug 2026 16:11:09 GMT
Content-Length: 100
Vary: Origin

--- response body ---
{
  "_tag": "InvalidRequestError",
  "message": "Missing key\n  at [\"prompt\"][\"text\"]",
  "kind": "Payload"
}

==== prompt shape B: {prompt:{text}} (1.18.10 contract) ====
URL: http://127.0.0.1:18096/api/session/ses_018b529c6ffe36DzIffAUL4YOp/prompt
ARGS: -X POST -H Content-Type: application/json -d {"prompt":{"text":"shape-probe-text"},"delivery":"queue"}
--- request body (if any) ---
--- response status: 200 ---
--- response headers ---
HTTP/1.1 200 OK
Content-Type: application/json
Date: Sun, 09 Aug 2026 16:11:09 GMT
Content-Length: 193
Vary: Origin

--- response body ---
{
  "data": {
    "admittedSeq": 1,
    "id": "msg_fe74ad6cf001wVsEGC894VY4eW",
    "sessionID": "ses_018b529c6ffe36DzIffAUL4YOp",
    "prompt": {
      "text": "shape-probe-text"
    },
    "delivery": "queue",
    "timeCreated": 1786291869393
  }
}

==== V2 prompt idle, delivery=queue ====
URL: http://127.0.0.1:18096/api/session/ses_018b529c6ffe36DzIffAUL4YOp/prompt
ARGS: -X POST -H Content-Type: application/json -d {"prompt":{"text":"reply with the single word: ack"},"delivery":"queue"}
--- request body (if any) ---
--- response status: 200 ---
--- response headers ---
HTTP/1.1 200 OK
Content-Type: application/json
Date: Sun, 09 Aug 2026 16:11:09 GMT
Content-Length: 208
Vary: Origin

--- response body ---
{
  "data": {
    "admittedSeq": 3,
    "id": "msg_fe74ad92a00174clFQTJdSeBmD",
    "sessionID": "ses_018b529c6ffe36DzIffAUL4YOp",
    "prompt": {
      "text": "reply with the single word: ack"
    },
    "delivery": "queue",
    "timeCreated": 1786291869995
  }
}

==== V2 prompt A (long, to occupy) ====
URL: http://127.0.0.1:18096/api/session/ses_018b529c6ffe36DzIffAUL4YOp/prompt
ARGS: -X POST -H Content-Type: application/json -d {"prompt":{"text":"write a 200-word essay about the ocean"},"delivery":"queue"}
--- request body (if any) ---
--- response status: 200 ---
--- response headers ---
HTTP/1.1 200 OK
Content-Type: application/json
Date: Sun, 09 Aug 2026 16:11:11 GMT
Content-Length: 215
Vary: Origin

--- response body ---
{
  "data": {
    "admittedSeq": 4,
    "id": "msg_fe74ae1340014TeVzsg46cX0gH",
    "sessionID": "ses_018b529c6ffe36DzIffAUL4YOp",
    "prompt": {
      "text": "write a 200-word essay about the ocean"
    },
    "delivery": "queue",
    "timeCreated": 1786291872053
  }
}

==== V2 prompt B (queued while A busy) ====
URL: http://127.0.0.1:18096/api/session/ses_018b529c6ffe36DzIffAUL4YOp/prompt
ARGS: -X POST -H Content-Type: application/json -d {"prompt":{"text":"reply with: ack2"},"delivery":"queue"}
--- request body (if any) ---
--- response status: 200 ---
--- response headers ---
HTTP/1.1 200 OK
Content-Type: application/json
Date: Sun, 09 Aug 2026 16:11:12 GMT
Content-Length: 193
Vary: Origin

--- response body ---
{
  "data": {
    "admittedSeq": 5,
    "id": "msg_fe74ae559001LzqfRrA0Vs4xs5",
    "sessionID": "ses_018b529c6ffe36DzIffAUL4YOp",
    "prompt": {
      "text": "reply with: ack2"
    },
    "delivery": "queue",
    "timeCreated": 1786291873114
  }
}

==== V2 interrupt (in-flight) ====
URL: http://127.0.0.1:18096/api/session/ses_018b529c6ffe36DzIffAUL4YOp/interrupt
ARGS: -X POST -H Content-Type: application/json -d {}
--- request body (if any) ---
--- response status: 204 ---
--- response headers ---
HTTP/1.1 204 No Content
Vary: Origin
Date: Sun, 09 Aug 2026 16:11:15 GMT
Content-Length: 0

--- response body ---


==== V2 interrupt (idle session) ====
URL: http://127.0.0.1:18096/api/session/ses_018b529c6ffe36DzIffAUL4YOp/interrupt
ARGS: -X POST -H Content-Type: application/json -d {}
--- request body (if any) ---
--- response status: 204 ---
--- response headers ---
HTTP/1.1 204 No Content
Vary: Origin
Date: Sun, 09 Aug 2026 16:11:16 GMT
Content-Length: 0

--- response body ---


==== V2 prompt C (to generate SSE events) ====
URL: http://127.0.0.1:18096/api/session/ses_018b529c6ffe36DzIffAUL4YOp/prompt
ARGS: -X POST -H Content-Type: application/json -d {"prompt":{"text":"reply with: ack3"},"delivery":"queue"}
--- request body (if any) ---
--- response status: 200 ---
--- response headers ---
HTTP/1.1 200 OK
Content-Type: application/json
Date: Sun, 09 Aug 2026 16:11:17 GMT
Content-Length: 193
Vary: Origin

--- response body ---
{
  "data": {
    "admittedSeq": 6,
    "id": "msg_fe74af5a80018za4MzMQnRz60Y",
    "sessionID": "ses_018b529c6ffe36DzIffAUL4YOp",
    "prompt": {
      "text": "reply with: ack3"
    },
    "delivery": "queue",
    "timeCreated": 1786291877289
  }
}

==== V2 prompt D (long, before OOM) ====
URL: http://127.0.0.1:18096/api/session/ses_018b529c6ffe36DzIffAUL4YOp/prompt
ARGS: -X POST -H Content-Type: application/json -d {"prompt":{"text":"write a 300-word essay about the moon"},"delivery":"queue"}
--- request body (if any) ---
--- response status: 200 ---
--- response headers ---
HTTP/1.1 200 OK
Content-Type: application/json
Date: Sun, 09 Aug 2026 16:11:28 GMT
Content-Length: 215
Vary: Origin

--- response body ---
{
  "data": {
    "admittedSeq": 13,
    "id": "msg_fe74b20c00019l5im4WBBSvHvF",
    "sessionID": "ses_018b529c6ffe36DzIffAUL4YOp",
    "prompt": {
      "text": "write a 300-word essay about the moon"
    },
    "delivery": "queue",
    "timeCreated": 1786291888323
  }
}

==== V2 prompt E (queued before OOM, should drain after restart) ====
URL: http://127.0.0.1:18096/api/session/ses_018b529c6ffe36DzIffAUL4YOp/prompt
ARGS: -X POST -H Content-Type: application/json -d {"prompt":{"text":"reply with: ack-oom-restart"},"delivery":"queue"}
--- request body (if any) ---
--- response status: 200 ---
--- response headers ---
HTTP/1.1 200 OK
Content-Type: application/json
Date: Sun, 09 Aug 2026 16:11:29 GMT
Content-Length: 205
Vary: Origin

--- response body ---
{
  "data": {
    "admittedSeq": 14,
    "id": "msg_fe74b24e4001fyE3t0EPZC0HMx",
    "sessionID": "ses_018b529c6ffe36DzIffAUL4YOp",
    "prompt": {
      "text": "reply with: ack-oom-restart"
    },
    "delivery": "queue",
    "timeCreated": 1786291889381
  }
}

```
