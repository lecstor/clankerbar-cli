# Conformance-testing "how we call opencode"

Most of the harness suite parses *saved* opencode output; it can never catch the binary
doing something unexpected. The conformance suite does: it **execs the real opencode 2.x
binary through the real adapter** (`opencode{}.Invoke`), pointed at a hermetic fake
OpenAI-compatible provider and a fake stdio MCP server, and asserts the CLI reads each shape
correctly. Zero spend: nothing leaves the machine, and every test asserts the fake provider
was actually hit.

It lives in `internal/harness/opencode_conformance_test.go` and is **opt-in**:

```sh
# Hermetic. Execs ~/.opencode/bin/opencode, or $CLANKERBAR_OPCODE_BIN.
CLANKERBAR_OPCODE_CONFORMANCE=1 go test ./internal/harness -run OpencodeConformance -v

# Live: ONE genuine paid turn on the provider configured on this machine, with the
# operator's real config and auth.
CLANKERBAR_OPCODE_CONFORMANCE_LIVE=1 \
OPENCODE_LIVE_MODEL=opencode-go/deepseek-v4-flash \
go test ./internal/harness -run OpencodeConformance_live -v
```

Run the hermetic suite whenever you change the opencode adapter or upgrade opencode. Neither
runs without its env var, so `go test -race ./...` in CI stays fast, free and green. The
hermetic suite takes about 1.5 minutes, almost all of it the 429 case (2.x retries a rate
limit for ~82s before it exits).

## What it pins

| Test | Asserts |
|---|---|
| `_stop` | A plain answer exits 0 with the final message; usage comes from the session export (the stream carries none), title generation included; cost matches the model's `cost` block; `finish_reason` is `stop`. |
| `_permissions/drain` | Under the adapter's policy, `webfetch`, `websearch`, `execute`, `question`, `skill` and `subagent` are not offered; a write and a shell command inside the workdir land; a write outside it and a read of `/etc/hosts` are refused by `external_directory`. Logs whether a shell command can still write outside the workdir (the accepted gap, docs/opencode-build.md). |
| `_permissions/readonly` | A probe-shaped run is offered no `edit`, `write` or `shell`, and writes nothing. |
| `_permissions/ambient *` | A project opencode.json in the session's checkout carrying `agent.build.permission`, `agents.build.permissions`, `default_agent` or a top-level `permission` that allows everything leaves out-of-tree reads/writes refused, webfetch/subagent unoffered, and in-tree writes working. |
| `_permissions/pattern` | Last-match-wins: `shell: {"*":"allow","git push*":"deny"}` denies the push and runs the other command. The policy's ordering depends on this. |
| `_mcpClaim/v1`, `/v2` | With an MCP config in either dialect, `clankerbar_claim_task` runs top-level (never through `execute`), the claim is observed (task, run id), the fake MCP server saw the call, and the warm-up note reached the model. Logs when the first-turn race fired. |
| `_largeClaim` | A 120 KB claim_task result, which 2.x truncates to a pointer at a saved file, is still observed as a held claim. |
| `_mcpDisable` | A full MCP entry with the off switch stops a server the global config declares; a partial one (no `type`) is logged as leaving it running. |
| `_resume` | `-s <sid>` continues the same session headlessly, the resumed run is charged only its own requests, and the session holds both turns. |
| `_errors` | 401 -> `provider.auth`, not transient; unknown model -> `provider.no-route`, no request sent, not transient; 429 -> `provider.rate-limit`, transient. None reports usage. |
| `_contentPin` | `OPENCODE_CONFIG_CONTENT` wins over `OPENCODE_CONFIG`. The permission policy and the Code Mode rewrite live in that layer. |

## How the hermetic harness works

1. **The binary is named, the build is read off the session record.** `TestMain` puts the
   binary's directory first on PATH, so the adapter's bare `opencode` resolves to it, and
   each test fails if PATH resolves anything else. After a run the suite reads
   `session_v2.version` from the isolated `opencode.db` (sqlite3; falling back to the
   `version=` log line) and fails unless it is 2.x. It never trusts PATH or `--version`;
   an `opencode2` elsewhere on PATH may be an old pnpm beta.
2. **Isolation.** HOME, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME` and
   `XDG_STATE_HOME` point at fresh temp dirs and `config_dir` is set, so neither the
   operator's config nor their credential store is read, and `--standalone` (always passed)
   keeps the run off the managed background service.
3. **The model id must be provider-qualified.** The fake is a custom provider
   (`npm: "@ai-sdk/openai-compatible"`, `options.baseURL`, a `models` map), selected by the
   2.x model object `{"providerID":"fake","model":"fake-model"}`. An id that does not resolve
   makes opencode fall back to a real default provider, which is why every test asserts the
   fake was hit.
4. **The fake MCP server is the test binary.** An MCP entry's `command` is the test
   executable with `CLANKERBAR_FAKE_MCP_DIR` set; `TestMain` then serves newline-delimited
   JSON-RPC (`claim_task`, `get_backlog_summary`) instead of running tests, and logs each
   call. No python or node is needed.
5. **Raw execs set `PWD`.** opencode takes its instance directory from `$PWD`, not the real
   cwd, so the two tests that exec the binary directly set it (the adapter's pinPWD does the
   same for every session).

## The Claude-shaped `.mcp.json` is silently ignored

The adapter's MCPConfigUse note and doctor say opencode refuses to start on a Claude-shaped
`.mcp.json` ("Unrecognized key: mcpServers"), as 1.18.2 did (CLA-263). Neither 1.18.18 nor
2.0.32 refuses it: an `mcpServers` key is skipped and the session runs with no clankerbar
tools (re-checked on 2.0.32 on 2026-10-18). doctor still FAILs that combination, which is
the right verdict for a different reason than the one it gives.

## Related

- [`docs/opencode-build.md`](./opencode-build.md) - which build the fleet runs and how the
  adapter drives it.
- [`docs/opencode-tool-schema-limits.md`](./opencode-tool-schema-limits.md) - the gateway
  tool-schema rejection family.
- CLA-675 (the 2.x move and this suite), CLA-441 (the content-layer pin), CLA-406
  (resurrection), CLA-381 (transient classification), CLA-263 (the mcpServers refusal).
