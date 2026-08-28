# The `opencode2` adapter

`opencode2` is OpenCode's **2.0 preview / nightly** line
(`@opencode-ai/cli`, e.g. `opencode2 v0.0.0-dev-17653`). The fleet runs the
**stable** build (`opencode` 1.18.x) for the reasons in
[`docs/opencode-build.md`](./opencode-build.md), and the stable adapter
hardcodes the binary name `opencode` so the two can never silently swap. This
adapter exists so the nightly can be **driven and evaluated by name** —
`clankerbar run --harness=opencode2` or a `harnesses.opencode2` block — without
any risk to the stable line: this adapter hardcodes `opencode2` instead.

## What the CLI is verified to do (opencode2 v0.0.0-dev-17653)

The adapter is deliberately **thin**, because the nightly's headless surface is
thin. Verified empirically (isolated XDG state + a hermetic fake provider, the
same method as `docs/harness-conformance.md`):

- **Invocation** — `opencode2 run --standalone --format json [--model m] --
  <prompt>`. The `--standalone` flag is LOAD-BEARING: `opencode2 run` is
  client/server and otherwise attaches to the shared background server
  (`opencode2 serve --service`) — which may be an operator's live interactive
  TUI (as it is on this machine). A standalone run spawns a private server, so
  a loop's session can never couple itself to your live session.
- **What `--format json` emits is *not* the 1.x stream.** Only assistant
  `text` parts, one JSON per part. There is **no `step_finish`** (no reason /
  tokens / cost), **no `tool_use`** (no way to observe a claim), **no error
  events**. Everything that makes the stable adapter rich is absent here.
- **Config dialect differs from 1.x.** `model` is an object
  (`{"providerID": "…", "model": "…"}`) and MCP servers sit under `mcp.servers`
  (verified via `opencode2 debug config`). `OPENCODE_CONFIG` still points at the config
  file, a custom `npm: "@ai-sdk/openai-compatible"` provider block is accepted,
  and the provider-qualified `--model` form resolves (a bare id does not —
  same rule as 1.x). `OPENCODE_CONFIG_DIR` is **not** used: opencode2 reads
  hardcoded paths (`~/.claude`, `~/.agents`, `~/.config/opencode`). Setting
  `config_dir` is therefore accepted but **inert** — the doctor `config_dir`
  check is advisory only for opencode2 and its "set it" remedy does not apply.

## What that means for running it (the honest capabilities)

| Capability | Value | Consequence |
| --- | --- | --- |
| `TracksClaims` | **false** | The stream carries no tool events, so the driver cannot observe a claim. `phases` is refused for an opencode2 sequence (like codex) — **single-phase runs only**. |
| `ReportsCost` | **false** | `budget.max_cost_usd` is inert for opencode2. |
| `HonoursSessionWallClock` | **true** | No turn flag and no usage, so the per-session wall-clock cap (the process kill in `Invoke`) is the only backstop. Set it deliberately. |
| `HonoursMaxTurns` / `HasSessionTokenCeiling` | **false** | Both inert. |
| `ZeroUsageUnknown` | **false** always | The quiet-death marker is read off a `step_finish` event, and this surface has none. A session that exits 0 having produced no text is indistinguishable from one that said nothing — a real blind spot, stated rather than faked. |

Classification is the provider-ecosystem word-classes shared with the stable
adapter, scoped to stderr and typed error events (never assistant narration).
opencode2 reports no usage, so `IsUnclassifiedTransient` is false: an
unrecognised failure stops loudly rather than re-spawning paid sessions forever
(CLA-381's failure direction, deliberately). The same no-usage fact has a
second, sharper consequence an operator should expect: nothing can ever reset
the always-on zero-spend breaker, so a *recognised* blip (a 429, a transport
drop) is retried at most 3 times per task (the default `max_zero_spend_attempts`)
and then the run stops with the zero-spend message. That message reads "the
sessions died before reporting any usage" — for opencode2 that wording is
misleading; read it as "the blip persisted past its retry budget", not as
sessions crashing.

## Use it

```sh
# Single-phase only. The model lives in opencode2's own config (or via a
# harnesses.opencode2.models tier); the MCP entry is a 2.x-schema config
# handed as OPENCODE_CONFIG.
clankerbar run --harness=opencode2 -c ./clankerbar.json
# doctor knows it too (auth markers, "no permission-policy checks" until
# the V2 permission model is verified):
clankerbar doctor --harness=opencode2
```

## Test it

Hermetic exec conformance (real `opencode2` binary, fake provider, zero spend —
requires `opencode2` on PATH):

```sh
CLANKERBAR_OPCODE2_CONFORMANCE=1 go test ./internal/harness -run Opencode2Conformance -v
```

The unit tests (`internal/harness/opencode2_test.go`) cover args, the text
parser, env, classification and capabilities without spawning anything.

## Verify the build, don't trust the name

`opencode2` is a nightly: the error surface changes without notice (CLA-381)
and the config schema already differs from 1.x. Before relying on it, confirm
what is actually running and what its `--format json` emits — the conformance
test above is exactly that check, and `docs/opencode-build.md`'s rule still
applies: never answer "which build ran?" from PATH.
