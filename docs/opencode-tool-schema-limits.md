# opencode-go request-time tool-schema rejections

The Console Go gateway (`opencode.ai/zen/go/v1/chat/completions`) can refuse a
request at **request time** because of the *tool schema* the client sent — before
any tokens are spent and before the session produces anything. This page is the
incident record and the CLI's handling of it. It is the counterpart of
[`docs/large-tool-results.md`](./large-tool-results.md), which documents a
different invisible failure: a tool result too large to inline.

## The signature

The rejection arrives as a typed error event on the opencode session's stream:

```json
{"type":"error","error":{"name":"APIError","data":{
  "message":"Error from provider (Console Go): Upstream request failed: [unsupported_tool_schema] The tool schema is not supported (tool_count_limit).",
  "statusCode":400,"isRetryable":false,
  "responseHeaders":{"cf-ray":"a2d73ba4cb2da60a-PDX","date":"Wed, 19 Aug 2026 06:52:00 GMT", ...},
  "responseBody":"{\"error\":{\"param\":\"tools\",\"type\":\"invalid_request_error\",\"message\":\"...\"}}",
  "metadata":{"url":"https://opencode.ai/zen/go/v1/chat/completions"}}}}
```

Two error codes are involved, and the naming is misleading:

- `unsupported_tool_schema` — what the gateway actually reports when it refuses.
- `tool_count_limit` — the code carried alongside it, which reads like a *count*
  cap. It is NOT one; see the root cause below.

## The 2026-08-19 incident (real log evidence)

Three iteration logs from 2026-08-19 carry the rejection as a genuine
`{"type":"error"}` event — this is the source of
[anomalyco/opencode#43374](https://github.com/anomalyco/opencode/issues/43374):

| iteration log | event time (UTC) | cf-ray |
|---|---|---|
| `iteration-20260819-164201-…26be477b.log` | 06:52:00 | `a2d73ba4cb2da60a-PDX` |
| `iteration-20260819-165257-…a700980d.log` | 06:53:06 | `a2d73d434861764b-SEA` |
| `iteration-20260819-165412-…cb99c11b.log` | 07:04:00 | `a2d74d3769c5ba4b-SEA` |

All three are byte-for-byte the shape above: `statusCode: 400`, `isRetryable:
false`, a cf-ray header, and the same message. They are captured as test fixtures
at `internal/harness/testdata/opencode-tool-schema-rejection.jsonl` and drive the
classifier's table test.

## Root cause: numeric bounds in the schema, not the tool count

The `tool_count_limit` code is a **misleading error code**. The comment thread on
[anomalyco/opencode#43378](https://github.com/anomalyco/opencode/issues/43378)
root-causes it: what Console Go actually rejects is the JSON Schema keywords
`minimum` / `maximum` / `exclusiveMinimum` / `exclusiveMaximum` on tool input
properties. A per-tool bisect isolated three failing tools — the only ones
carrying numeric bounds; stripping the four keywords from all tools returned 200,
re-adding them returned 400, and the identical payload to `api.deepseek.com`
succeeded with the bounds intact. The reported "16 tools OK, 17 tools rejected"
boundary was an artefact of which tool happened to be 17th, not a count cap.

A day later (2026-08-20) that finding could NOT be reproduced: 14 direct
requests to the gateway (1 to 64 tools, each with and without
`minimum`/`maximum`, on `deepseek-v4-flash`) all returned 200. So enforcement is
**inconsistent** — #43374 itself records the same catalog accepted and refused
seconds apart. "Not enforced today" is not "cannot return".

## Gateway, not client

The rejection is a property of the **gateway**, not of the opencode client:
[#43378](https://github.com/anomalyco/opencode/issues/43378) shows a completely
different client (nanobot-ai, Python/httpx) hitting the same gateway rejection, so
no client change lifts it. Switching opencode builds does not help either:
`opencode2` exists on the machine but the binary name is hardcoded in the
adapter, and the issue is upstream of any client.

## What the CLI does about a recurrence

Three things, deliberately:

1. **Classifies the family as transient** (`opencodeTransientRe` in
   `internal/harness/opencode.go`). This is a deliberate choice, not a default:
   "transient" is arguably a lie for a rejection that will refuse the identical
   payload again — but the gateway's enforcement is inconsistent (above), so the
   identical retry often succeeds, which is exactly what the transient path does.
   A persistent run of rejections is bounded separately: each rejected session
   reports no usage (the 400 fires at request time), so the driver's zero-spend
   bound stops the run after a few attempts rather than retrying forever.
2. **Marks the quiet death.** A session whose final `step_finish` carried
   reason `unknown` with all-zero usage used to read as an ordinary end — the
   driver logged the session's own totals and moved on, with no way to tell the
   death from a completion. The adapter now writes its own `terminal_reason`
   (`zero_usage_unknown`) when the FINAL step's own usage is all-zero (whatever
   the earlier steps cost — the real dead sessions had done paid work before
   the silent final step), and the driver's log lines name it
   (`ZeroUsageReason` / `Adapter.ZeroUsageUnknown`).
3. **Barely touches the client side.** The schema strip of our own MCP tools'
   numeric bounds is the OTHER repo's hardening (clankerbar CLA-399), and it is
   defence against a recurrence, not a fix for anything observed here.

## Confirmed vs unconfirmed: the link to our own dead phases

Six of sixteen opencode implement sessions on 2026-08-20 ended with a final
`step_finish` carrying `reason: "unknown"` and all-zero usage — the quiet-death
signature above — across five different tasks. What is **confirmed**: those
sessions died producing nothing, and the marker above is what makes any such
death self-describing. What is **unconfirmed**: that the tool-schema rejection
caused them. On 2026-08-20 the rejection strings appeared in the iteration logs
only as task-body text an agent read, never as an error event, and the direct
probes all returned 200. The live hypothesis for the dead phases is a different
gateway failure — streams ending with no terminal `finish_reason`
([#40420](https://github.com/anomalyco/opencode/issues/40420),
[#43379](https://github.com/anomalyco/opencode/issues/43379)) — tracked as
CLA-401. The tool-schema work here stands as hardening against a recurrence the
2026-08-19 logs prove is real.