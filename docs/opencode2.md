# The `opencode2` adapter

`opencode2` is OpenCode's **2.0 preview** line (`@opencode-ai/cli`, e.g.
`opencode2 v0.0.0-beta-18314`). The fleet runs the **stable** build
(`opencode` 1.18.x) for the reasons in
[`docs/opencode-build.md`](./opencode-build.md), and the stable adapter
hardcodes the binary name `opencode` so the two can never silently swap. This
adapter exists so the preview can be **driven and evaluated by name** —
`clankerbar run --harness=opencode2` or a `harnesses.opencode2` block — without
any risk to the stable line: this adapter hardcodes `opencode2` instead.

## What the installed build is verified to do (opencode2 v0.0.0-beta-18314)

The adapter is deliberately **thin**, because the preview's headless surface is
thin. Verified empirically 2026-08-28 against the installed
`opencode2 v0.0.0-beta-18314` (hermetic fake provider, zero spend, the same
method as `docs/harness-conformance.md`; the build is identified from the
SESSION RECORD's `version=` line, never from PATH). The four load-bearing
claims, each confirmed or corrected against what this build actually does:

- **Invocation — CONFIRMED.** `opencode2 run --standalone --format json
  [--model m] -- <prompt>`. The `--standalone` flag is LOAD-BEARING: `opencode2
  run` is client/server and otherwise attaches to the shared background server
  (`opencode2 serve --service`) — which may be an operator's live interactive
  TUI (as it is on this machine). A standalone run spawns a private server, so
  a loop's session can never couple itself to your live session; the session
  record shows the private `serve --stdio --port 0` child it spawns.
- **The `--format json` stream — CORRECTED from the vanished nightly's doc.**
  The beta emits `text`, `step_start`, `tool_use`, `step_finish` and typed
  `error` events. But a plain text answer (the common drain case) emits ONLY
  the `text` event: the provider's usage block is NOT surfaced (verified: the
  fake's 18511+2 usage never appeared on the stream), so `UsageReported` stays
  false and budget accounting is inert (`ReportsCost` false). `step_finish`
  events appear on tool-call turns, carrying reason/cost/tokens per step (no
  `total` sibling — the adapter sums input/output/reasoning); `tool_use` is
  observed but not consumed for claim tracking, so `TracksClaims` stays false.
  Typed `error` events (`provider.invalid-output`, ...) are collected for
  classification.
- **Config dialect — CONFIRMED.** `model` is an OBJECT
  (`{"providerID": "...", "model": "..."}`) and MCP servers live under
  `mcp.servers` (the operator's own v2 config and `opencode2 debug config`
  corroborate it). `OPENCODE_CONFIG` points at the config file, a custom
  `npm: "@ai-sdk/openai-compatible"` provider block is accepted, and the
  provider-qualified `--model` form resolves. The file's bytes are ALSO pinned
  as `OPENCODE_CONFIG_CONTENT` — see below.
- **`OPENCODE_CONFIG_DIR` — CORRECTED from the vanished nightly's doc.** It
  does NOT move config-file discovery: the beta reads `~/.claude`,
  `<cwd>/.claude`, `~/.agents`, `~/.config/opencode2` (its `opencode.json`)
  and `~/.opencode` HARDCODED — `XDG_CONFIG_HOME` does not move them
  (verified via `opencode2 debug config` with both pointed elsewhere). What
  the variable DOES steer is the PLUGIN dir (verified: a plugin under
  `$OPENCODE_CONFIG_DIR/plugins` loads) plus an additional config-dir merge
  layer. The adapter therefore never sets it: `config_dir` maps to nothing,
  and redirecting plugins to the fleet's config dir would silently detach the
  operator's plugin setup. The operator's own wrapper sets it for interactive
  sessions, and its effects are inherited by fleet runs that way.

**The config pin, new with CLA-541.** `OPENCODE_CONFIG_CONTENT` merges AFTER
every other layer on beta-18314 — verified 2026-08-28 with two configs naming
different providers: whichever provider the content layer named is the one the
session hit, in both directions. That is the same ordering the stable adapter
reads out of 1.18.19, so the adapter ships the same fail-closed pin (CLA-441):
the driver's config file is read and handed over as `OPENCODE_CONFIG_CONTENT`,
and an unreadable file refuses the spawn. Without it, the last word on a
spawned session would be some ambient layer's — the hardcoded
`~/.config/opencode2/opencode.json`, an inherited `OPENCODE_CONFIG_CONTENT`,
or an `OPENCODE_CONFIG_DIR` dir — exactly the class that redirected a v1
session to another project's backlog.

**Permissions.** The adapter exports the SAME fail-closed `OPENCODE_PERMISSION`
policy the stable adapter exports (one policy to maintain, one posture for
both lines). VERIFIED caveat: beta-18314 does NOT honor the env var — with
`--auto` + `{"*": "deny"}` the write tool still executed, and with no env var
at all the same write was declined; the config `permission` block is not
honored either. The fail-closed property of an unattended opencode2 run comes
from the HEADLESS DEFAULT: without `--auto`, every tool call is declined ("The
user declined this tool call"), and this adapter never passes `--auto`. The
exported policy is belt-and-braces for a future build that honors it; `doctor`
says exactly this.

## What that means for running it (the honest capabilities)

| Capability | Value | Consequence |
| --- | --- | --- |
| `TracksClaims` | **false** | `tool_use` events exist on tool-call turns but the adapter does not consume them for claim observation. `phases` is refused for an opencode2 sequence (like codex) — **single-phase runs only**. |
| `ReportsCost` | **false** | A plain text answer (the common case) emits NO `step_finish` and the provider's usage block is not surfaced, so `budget.max_cost_usd` is inert. |
| `HonoursSessionWallClock` | **true** | No turn flag and no usage, so the per-session wall-clock cap (the process-group kill in `Invoke`) is the only backstop. Set it deliberately. |
| `HonoursMaxTurns` / `HasSessionTokenCeiling` | **false** | Both inert. |
| `ZeroUsageUnknown` | **false** always | The quiet-death signature (silent exit-0, CLA-398) is not exhibited by this build: the #43622 shape (a stream with no finish_reason) makes the beta emit typed `provider.invalid-output` error events, RETRY internally, and exit 1 — a loud failure the error classification already reads. Inventing a marker for a shape this build does not produce would mislead the driver. |

Classification is the provider-ecosystem word-classes shared with the stable
adapter, scoped to stderr and typed error events (never assistant narration).
Because a plain text answer reports no usage, `IsUnclassifiedTransient` is
false on the common path: an unrecognised failure stops loudly rather than
re-spawning paid sessions forever (CLA-381's failure direction, deliberately).
The same no-usage fact has a second, sharper consequence an operator should
expect: nothing can ever reset the always-on zero-spend breaker, so a
*recognised* blip (a 429, a transport drop) is retried at most 3 times per
task (the default `max_zero_spend_attempts`) and then the run stops with the
zero-spend message. That message reads "the sessions died before reporting any
usage" — for opencode2 that wording is misleading; read it as "the blip
persisted past its retry budget", not as sessions crashing.

## Use it

```sh
# Single-phase only. The model lives in opencode2's own config (or via a
# harnesses.opencode2.models tier); the MCP entry is a 2.x-schema config
# handed as OPENCODE_CONFIG (+ the content pin).
clankerbar run --harness=opencode2 -c ./clankerbar.json
# doctor knows it too (auth markers; the opencode2 permission case reports
# the fail-closed posture truthfully):
clankerbar doctor --harness=opencode2
```

## Test it

Hermetic exec conformance (real `opencode2` binary, fake provider, zero spend —
requires `opencode2` on PATH):

```sh
CLANKERBAR_OPCODE2_CONFORMANCE=1 go test ./internal/harness -run Opencode2Conformance -v
```

The unit tests (`internal/harness/opencode2_test.go`) cover args, the text
parser, env (including the content pin and its fail-closed read), permissions,
classification and capabilities without spawning anything.

## Verify the build, don't trust the name

`opencode2` is a preview: the error surface changes without notice (CLA-381)
and the config schema already differs from 1.x. Before relying on it, confirm
what is actually running and what its `--format json` emits — the conformance
test above is exactly that check (it reads the version off the session record,
the same `version=` line `docs/opencode-build.md` reads for the v1 line), and
`docs/opencode-build.md`'s rule still applies: never answer "which build ran?"
from PATH.