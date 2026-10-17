# Which opencode build we run

Short answer: **opencode 2.x**, installed by the official installer at
`~/.opencode/bin/opencode` (2.0.32 at the time of writing). The 1.x line is no longer fixed
upstream, and the CLI no longer speaks its dialect (CLA-675, operator decision 2026-10-18).
There is one opencode harness, named `opencode`; the `opencode2` harness name that drove the
2.0 preview is retired, and a config still naming it fails validation with the rename in the
message.

## The adapter runs a bare name, so PATH decides

`internal/harness/opencode.go` execs `"opencode"` with no path. There is no config dial for
the binary, so **whatever `opencode` resolves to on the daemon's PATH is what runs**. Put
`~/.opencode/bin` first on the daemon's PATH. An `opencode2` elsewhere on PATH (an old pnpm
beta) is never executed by the CLI; a Homebrew 1.x `opencode` that wins the PATH race is,
and `clankerbar doctor` FAILs it (`harness: ... opencode 1.x is no longer supported`).

**Never answer "which build ran?" from PATH.** Read it off the session record: 2.x writes
the creating build into `session_v2.version` in `<data dir>/opencode/opencode.db`, and
`<data dir>/opencode/log/opencode.log` carries `version=` on every `cli starting` line. The
conformance suite identifies the build that way.

## How the adapter drives 2.x

Verified against 2.0.32 with a hermetic fake provider and fake MCP server (zero spend; the
suite in `internal/harness/opencode_conformance_test.go` re-checks every claim below).

| Concern | 2.x behaviour | What the adapter does |
|---|---|---|
| Entry point | `run` attaches to the managed background service unless `--standalone`; the service's config then wins. A non-TTY stdin left open hangs `run`. | `opencode run --standalone --format json --agent build [--model m] -- <prompt>`, stdin nil. |
| Permissions | `OPENCODE_PERMISSION` is **ignored** (the string is not in the binary). With no policy, 2.x is **fail-open**: write, shell and webfetch all run, inside the workdir or not. A config `permission` block is honoured; tool-level deny removes the tool; patterns are last-match-wins. | Writes the fail-closed policy into the `permission` key of `OPENCODE_CONFIG_CONTENT`, set on **every** spawn (with or without an mcp config). Never passes `--auto`. |
| Ask shape | read/edit asks are relative to the **instance directory** (`$PWD`), in or out of a git checkout. The driver spawns each session in the **task's repo checkout** (`sessionDir` / `config.ResolveCheckout`), not the project `workdir`, and pins `$PWD` to it. | `"**"` allow, `"../**"` deny, `"../<extra>/**"` allow per other declared repo; absolute `external_directory` allows. |
| Agents | An agent-level permission block in **any** layer (1.x `agent.<name>.permission`, 2.x `agents.<name>.permissions`) overrides the top-level policy for that agent, and an ambient `default_agent` picks the agent a session starts on. A top-level `permission` in a lower layer was observed to have no effect on the pinned policy. | Pins the same policy on `build`, `plan`, `general` and `explore`, drops the mcp file's own `agents`/`default_agent`, and passes `--agent build`. A project opencode.json in the checkout (which a session can write for the next spawn) carrying any of those shapes leaves out-of-tree access refused (conformance-verified). |
| MCP | Servers default to **Code Mode**: their tools are reachable only inside the `execute` tool, whose stream shows no per-call output, and code there reached the network with webfetch denied. `codemode: false` is honoured only in the 2.x dialect (`mcp.servers.<name>`). | Sets `codemode: false` on every server in the content layer, moving 1.x `mcp.<name>` entries under `mcp.servers` (1.x `enabled: false` becomes `disabled: true`). Denies `execute`. Tools then appear top-level as `<server>_<tool>`. |
| MCP start-up | Servers connect after the session's first LLM request; a first-turn `claim_task` fails with `No tool named "clankerbar_claim_task" is currently available`. | Prepends a one-line note to working prompts telling the model to call the tool again. Chosen over an adapter-side retry because the failed call is an ordinary turn the model already reacts to; a re-spawn would pay for the whole prompt again. |
| Events | `step_start`, `text`, `tool_use`, `step_finish`, `error`. Tool parts carry the call id as `id` (1.x `callID`) and the part id as `partID`; provider call ids repeat across steps. | Parses `id`; recognises replays by `partID`. |
| Large results | An MCP result above 51,200 bytes is truncated: `output` becomes `[showing 0 lines of 1; full output saved to <data>/opencode/tool-output/tool_...]` and `metadata.metadata` is `{"truncated":true,"outputPath":...}`. Plane payloads reach that size. | Reads the saved file back when opencode's metadata says it truncated: only from the session's own `<data home>/opencode/tool-output`, only a regular `tool_*` file, size-bounded. Any failure leaves the claim untracked with a diagnostic. Only `claim_task` results are parsed, so nothing else depends on it. |
| Usage | `step_finish` only for tool-call steps; the final answer and title generation emit none. `opencode session export --standalone <sid>` has the session totals and per-message `finish`/`cost`/`tokens`. Cost is non-zero only when the model's provider config has a `cost` block. | Reads the export after every process and charges each process its own messages (the creating one also gets title generation). The stream's sums are the fallback. The final-step markers (quiet death, output-cap stall) come from the export's last message. |
| Resume | `-s <id>` continues a session headlessly; an unknown id **silently creates** a new session. | Resumes only into a session id seen on the stream. |
| Errors | Typed: `provider.auth` (401, exit 1), `provider.rate-limit` (429, retried ~10x over ~82s, then exit 1), `provider.no-route` (unknown model, exit 1, no request). | `provider.auth` and `provider.no-route` stop; `provider.rate-limit` rides the backoff. The type is recorded on the Result. |
| Config layers | Global dir (`$XDG_CONFIG_HOME/opencode`, **replaced** by `OPENCODE_CONFIG_DIR`), then `OPENCODE_CONFIG`, then `<cwd>/opencode.json` and `<cwd>/.opencode/opencode.json`, then `OPENCODE_CONFIG_CONTENT` (last; its `permission` beat a project file). `~/.opencode`, `~/.claude`, `~/.agents` and `~/.config/opencode2` are **not** read. | `config_dir` -> `OPENCODE_CONFIG_DIR`, `mcp_config_path` -> `OPENCODE_CONFIG` plus the rewritten content layer. doctor's ambient audit scans the 2.x layers. |
| Auth | Credentials live in `<XDG_DATA_HOME or ~/.local/share>/opencode/opencode.db`, table `credential`; a migration imports a legacy `auth.json`. 1.18.27 refuses a database 2.x created. | doctor counts credential rows per integration (never reading a value). |

## Shell is not path-gated

**opencode 2.x does not path-gate the shell tool.** Only the shell tool's own `workdir`
argument is checked against `external_directory`; the paths inside a command are not. Under
the adapter's policy, `touch /abs/outside` and `cat /etc/hosts` both ran. On 1.x, commands
whose file arguments left the project were gated by `external_directory`.

**The operator accepted this gap on 2026-10-18 (CLA-675)**, because an unattended session can
already run arbitrary commands inside the workdir. No OS sandbox was added, and no shell
deny patterns naming absolute paths (they would be easy to step around).

The same acceptance covers a narrower escape that would matter only if the shell were
gated: read/edit asks are judged on the path as written, so a symlink inside the checkout
pointing outside it (`ln -s / escape`, then a read of `escape/etc/hosts`) asks `escape/...`,
matches `**`, and reaches the target. A session that can run `ln` can already run `cat`. `clankerbar doctor`
states it beside the permission posture as a known, accepted gap, not a failure, and the
conformance suite logs whether a build still shows it.

## What the operator configures

- **PATH**: `~/.opencode/bin` first on the daemon's PATH.
- **`mcp_config_path`** (conventionally `~/.config/clankerbar/opencode-mcp.json`): either
  dialect works; the adapter rewrites 1.x entries. It must be strict JSON and should carry no
  `permission` key: the file is also loaded as `OPENCODE_CONFIG`, where its keys would merge
  ahead of the pinned policy's catch-all. doctor WARNs on one.
- **Credentials**: `opencode auth login` stores them in opencode.db, or give the provider its
  key in the opencode config as `{env:...}`. doctor counts active rows only; the store records
  no expiry, so a counted key may still be revoked at the provider.
- **Switching off a global MCP server**: a disable must repeat the server's **full** entry
  (its `type` and `command`/`url`) with `"enabled": false` (or, under `mcp.servers`,
  `"disabled": true`). A partial `{"enabled": false}` with no `type` leaves the server
  running on 2.x; doctor's `opencode_mcp_disables` check WARNs on one.
- **Global config** (`~/.config/opencode/opencode.json`, or the `config_dir` if set): 2.x
  reads it. MCP servers declared only there stay in Code Mode and so are unreachable with
  `execute` denied; declare the ones sessions need in `mcp_config_path`.
- **Cost**: `budget.max_cost_usd` is live, but the cost figure is only non-zero for a model
  whose provider config declares `cost`.
- **Do not set** `OPENCODE_PERMISSION` (ignored) or `OPENCODE_CONFIG_CONTENT` (replaces the
  pinned policy) in any `env` block; doctor WARNs on both. A `permission` block in the mcp
  config has no effect on 2.x; doctor notes it.

## Gaps against 1.x worth knowing

- MCP **resource** tools (`list_mcp_resources`, `read_mcp_resource`) are registered in Code
  Mode only, so with `execute` denied a session cannot read the served skills as MCP
  resources. The policy keeps its `mcp:*` read allow for a build that exposes them again.
- The quiet-death signature (a final step with reason `unknown` and zero usage) was a 1.x
  shape, anomalyco/opencode#43622; the preview line turned the same provider stream into typed
  `provider.invalid-output` errors and a loud exit 1. The CLA-406 resurrection path is kept
  and now reads its signal from the export.
- A 429 costs about 90 seconds before the adapter sees it, because 2.x retries it itself.

## Testing

```sh
CLANKERBAR_OPCODE_CONFORMANCE=1 go test ./internal/harness -run OpencodeConformance -v
```

Execs `~/.opencode/bin/opencode` (override with `CLANKERBAR_OPCODE_BIN`) through the real
adapter, with HOME and the XDG dirs pointed at temp dirs. See
[`docs/harness-conformance.md`](./harness-conformance.md).

## History

The fleet ran Homebrew 1.18.x until 2026-10, pinned to 1.18.19 on purpose: that build's
silent exit on an indeterminate finish reason (#43622) was caught by CLA-406, where 1.18.20+
retried the same stream without bound. The `opencode2` preview adapter (CLA-541, after
decision `f953195e`, 2026-08-28) drove `opencode2 v0.0.0-beta-18314` single-phase only: that
build did not surface usage on a plain answer and honoured no permission setting, relying on
its headless default declining every tool call. 2.0.32 changed both, which is why the stable
adapter was moved to 2.x rather than the preview adapter promoted. CLA-381 (the transport
drop that made opencode exit-1 classification retry transient failures) still applies.

## See also

- [`docs/opencode-tool-schema-limits.md`](./opencode-tool-schema-limits.md) - the Console Go
  tool-schema rejection family.
- [`docs/harness-conformance.md`](./harness-conformance.md) - the conformance suite.
- CLA-675 (this move), CLA-441 (the content-layer pin), CLA-406 (resurrection), CLA-381.
