# Conformance-testing "how we call opencode"

Where opencode processes a model's response and streams it back to the CLI
driver, the result sometimes arrives **garbled or broken** — a silent exit 0, a
final step reading `reason: "unknown"` with all-zero usage, a transport drop.
Most of the harness suite parses *saved* opencode output; it can never catch
the binary doing something unexpected. These tests do: they **exec the real
`opencode` on PATH through the real adapter**, pointed at a hermetic fake
provider, and assert the CLI classifies each shape correctly.

They live in `internal/harness/opencode_conformance_test.go`, and they are
deliberately **opt-in** (they exec a real binary and, in live mode, spend real
money):

```sh
# Hermetic: fake provider, zero spend when the fake resolves, deterministic.
# Runs against whatever `opencode` resolves to on PATH (the fleet's stable
# 1.18.x line). Caveat, learned the hard way: if the configured model id ever
# stops resolving to the fake, opencode silently runs its real default provider
# instead and that run spends real money before the "fake was hit" guard fails.
CLANKERBAR_OPCODE_CONFORMANCE=1 go test ./internal/harness -run OpencodeConformance -v

# Live: ONE genuine paid turn on the provider configured on this machine.
CLANKERBAR_OPCODE_CONFORMANCE_LIVE=1 \
OPENCODE_LIVE_MODEL=opencode-go/deepseek-v4-flash \
go test ./internal/harness -run OpencodeConformance_live -v
```

Run the hermetic pair whenever you change the opencode adapter, upgrade
opencode, or before flipping the fleet back to opencode. Run the live one as
the "is opencode adoption-ready" gate — it is the same skeleton as the hermetic
test, just pointed at a real provider. Neither runs without its env var, so
`go test -race ./...` in CI stays fast, free, and green.

## What it pins

- **Control** (`stop` script): a provider that streams a finished answer
  (`finish_reason: "stop"` plus a usage block) makes the adapter report a
  clean, spend-accounted, NOT-silent session — exit 0, `finish_reason: "stop"`,
  `UsageReported` true, `ZeroUsageUnknown` false.
- **Quiet death** (`quiet` script), opencode up to and including **v1.18.19**: the
  [#43622](https://github.com/anomalyco/opencode/issues/43622) shape, where no
  chunk ever carries a non-null `finish_reason`. opencode hands the CLI
  `reason: "unknown"`, all-zero tokens/cost, **exit 0 with no error** — which
  reads as a clean completion unless the adapter names it. The test is
  **world-aware**: on that build it asserts the adapter names the death
  (`ZeroUsageUnknown` true).
- **The retry world, opencode >= v1.18.20**: the silent exit is fixed upstream
  by RETRYING the empty stream, which against a persistently-empty stream
  never terminates on its own (the hazard #43622 warned about; #43881 remains
  open). The test bounds the provocation with a **15-second wall-clock cap** and
  asserts the CLI's operational contract for the new world: the cap ends the
  session, and the raw death signature (reason `"unknown"`, all-zero usage) is
  preserved on the Result. Note for maintainers: on a capped end the composite
  `ZeroUsageUnknown` marker cedes to `wall_clock_capped` (a single
  `terminal_reason` key), so a capped spin is not counted in the dead-phase
  tally today — whether it should be is an open decision for the 1.18.20+ world.
- **A proper-fix world**: if upstream ever lands an error-surfacing fix, the
  session ends on its own with neither signature and the test turns green with
  a loud "re-pin" log — never a false red.
- **That the fake was actually hit.** Each run asserts `/v1/chat/completions`
  reached the fake provider. Open code silently runs its own default provider
  (`opencode-go`) when it cannot resolve the configured model, so without this
  guard a "quiet death" run that actually hit a real backend would pass for the
  wrong reason. The guard fails loudly, but the run that missed the fake has
  already happened — if the model id regressed, that is one real paid turn spent
  on a failing test. Nothing here makes that turn free; it makes it fail instead
  of pass.

- **No orphaned servers.** `opencode run` is client/server; the test isolates
  each run with fresh XDG dirs so a warm server can never serve it. Verified on
  1.18.18 that the per-run server tears itself down when the run ends.

Upstream's fix landed as v1.18.20 (retry the empty stream instead of exiting
silently; the anomalyco issue's PRs #41466/#43881 describe the same family).
Two things changed with it: the silent exit-0 is
gone (a transiently-empty stream recovers instead of dying), and a
PERSISTENTLY-empty stream now spins in an unbounded retry instead — which is
why the wall-clock cap (`max_session_wall_clock` / per-phase `max_wall_clock`)
is no longer optional for opencode runs: it is the only backstop that can end
such a session, and the loop's salvage preserves everything it wrote when the
cap fires. The harness-conformance quiet-death test documents the exact
observable contract per build.

## How the hermetic harness works (verified against opencode 1.18.18)

Three facts, each learned the hard way, each load-bearing in the test:

1. **The model id must be provider-qualified.** A custom provider is configured
   with `npm: "@ai-sdk/openai-compatible"`, `options.baseURL`, and a `models`
   map, and the config's `model` must read **`fake/fake-model`** — never the
   bare `fake-model`. A bare id does not resolve, and opencode then runs its
   built-in default (in the fleet's case the paid `opencode-go`) instead of
   erroring. That is how the first draft of this test "passed" against a real
   backend at real cost. The `@ai-sdk/openai-compatible` package ships in
   opencode's `node_modules`, so no install step is needed.

2. **opencode run is client/server, and the server holds the config.** A
   server already running (even one spawned by an earlier `opencode run`) keeps
   its config, so `OPENCODE_CONFIG` alone cannot re-point a warm server — the
   earliest manual probes were silently served by a process that had read the
   fleet's config, and the fake was never reached. The test therefore isolates
   opencode's whole state with fresh
   `XDG_CONFIG_HOME` / `XDG_DATA_HOME` / `XDG_CACHE_HOME` per run, forcing a
   fresh server that reads the test's config. Production does not set these:
   the fleet's `config_dir` points at the real config directory, so the server
   it spawns reads the real config.

3. **`OPENCODE_CONFIG_DIR` appears inert on 1.18.18.** The adapter's
   `ConfigDir → OPENCODE_CONFIG_DIR` parity (modeled on Claude's
   `CLAUDE_CONFIG_DIR`) is not honored by opencode in the paths exercised here —
   the config that matters is the one in opencode's own config dir. It is
   harmless to keep setting it (the fleet does), but nothing should *depend* on
   it; the two things that actually steer a session are `OPENCODE_CONFIG` and
   the server's config dir. Worth a maintainer looking at whether the
   `config_dir` doctor check for opencode measures anything.

## A finding that changed the story: CLA-263's refusal is gone

The adapter and README say opencode **refuses to start** on a Claude-shaped
`.mcp.json` — "Unrecognized key: mcpServers" — verified at 1.18.2 (CLA-263). On
1.18.18 that refusal **no longer happens**: an `OPENCODE_CONFIG` carrying
`mcpServers` is accepted without complaint, and the session runs — with **no
clankerbar tools**. The unknown key is silently skipped. That is precisely the
outcome the refusal existed to prevent (a session that looks in the log like a
model that declined the work) and it now happens silently instead of loudly.
`doctor` still FAILs `opencode` pointed at a Claude-shaped file with the stale
"refuses to start" remedy; that message and the check's premise should be
re-verified against the shipped build.

This conformance test does not assert either behavior on purpose: pinning
"refuses" would fail on this build, and pinning "silently ignored" would bless
the wrong behavior. The drift is recorded here instead, and the CLA-263
remedy wording is what to look at next.

## Related

- [`docs/opencode-build.md`](./opencode-build.md) — which build the fleet runs
  (Homebrew stable 1.18.x), and the opencode2 nightly trap.
- [`docs/opencode-tool-schema-limits.md`](./opencode-tool-schema-limits.md) —
  the separate gateway tool-schema rejection family.
- CLAs: **CLA-381** (transient classifier / opencode2 transport drop),
  **CLA-398/401** (the quiet-death signature and its disconfirmation of the
  gateway), **CLA-406** (treating the dead phase as a discarded turn),
  **CLA-263** (the now-stale mcpServers refusal).
