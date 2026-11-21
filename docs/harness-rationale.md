# Harness Rationale

Harness adapters: the adapter contract, per-CLI invocation, streams, sessions and resumes.

This file holds the design and process reasoning retired from the code comments in
`internal/harness` when those comments were cut to the one-to-three-line rule (CLA-809). The
comments beside the code keep the invariant, trap or constraint an editor needs at
that site; the fuller rationale - why a default is the number it is, how a
failure was measured, which alternative was rejected - lives here. Task refs and
dates are provenance for the reasoning, not a changelog.

Each section is one source file. Within it, a heading names the declaration the
comments belonged to, or quotes the first line when they were a free-standing
section note.


## `internal/harness/claude.go`

### claude

> claude drives Claude Code (`claude -p`). Notable, per the capability spike:
>   - `.mcp.json` is NOT auto-discovered in -p mode; pass --mcp-config explicitly.
>   - --output-format stream-json (+ --verbose) streams events live, so we can
>     render progress to the console AND parse the final result/usage/limit.
>   - CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 keeps the session alive while
>     delegated subagents are still running (the drain dispatches to subagents).
>   - CLAUDE_CONFIG_DIR pins the config dir so a headless run loads the same
>     skills, plugins, and auth as the interactive one.

### newSessionResult

> newSessionResult is the Result a session starts from, before a byte of its
> stream is parsed.
>
> It exists so the SEED is testable. A resumed phase never calls claim_task — it
> was told to continue a run, not start one — so without this its claim would be
> the zero value, and Result.Claim.Held() is what gates the driver's handback,
> the CLA-314 salvage and the CLA-253 delivery check. Seeding leaves the stream
> to supply Settled/HasWIP on top: noteToolUse matches the progress and terminal
> tools against this TaskID, which is precisely the arm that would otherwise
> never fire for the phase that pushes the branch and opens the PR.
>
> Extracted rather than assigned inline because a test that builds its own
> Result does not exercise an assignment inside Invoke — which is exactly how the
> first cut of this shipped with the seed untested and a mutation of it surviving
> the whole suite.

### claudeArgs

> claudeArgs builds the session's argv. Extracted from Invoke so it can be
> asserted directly: a flag that silently stops being emitted is invisible to
> every other test, and one of these is a dependency on an UNDOCUMENTED flag
> (see claudeMaxTurnsReason), where a version bump is exactly how it would go.
>
> The prompt is NOT an argument (CLA-606). `--input-format stream-json` makes the CLI read
> user turns from stdin for the session's lifetime, which is what lets a console message
> become the session's next turn. The old `-p <prompt>` form could not accept one. The
> prompt is written as the first stream-json user message by Invoke; closing stdin tells the
> CLI to finish the current turn and exit, which is how the session ends.

### The project's other declared repos (CLA-437)

> The project's other declared repos (CLA-437): --add-dir is Claude's own
> mechanism for granting tool access beyond the working directory, so a
> two-repo project's session can read and edit its sibling whatever directory
> it starts in. One flag, then each dir as its OWN argv element: the CLI
> documents the flag as variadic (`--add-dir <directories...>`), and a
> variadic consumes following elements until the next flag - so a single
> space-joined element parsed as ONE directory whose path contains spaces,
> and two or more extras granted nothing (CLA-443). The operator's
> --settings policy still governs: this widens what the session MAY be
> granted, the settings file's deny rules still say what it IS. Probes never
> reach here.

### Kill the whole process group (descendants holding inherited pipes)

> Kill the whole process group (descendants holding inherited pipes)
> rather than only the direct child, on ANY cancellation of sctx - the
> token-ceiling kill from consume(), or the caller's own cancellation
> (a Ctrl-C that stopped reaching the session subtree through the tty
> the moment Setpgid moved it out of the foreground group). Through
> exec.Cmd.Cancel, not a monitor goroutine: os/exec calls Cancel only
> while it still considers the process live. The trailing Kill mirrors
> CommandContext's own default Cancel, so the direct child still dies
> even if it already left the group.
>
> Assigned BEFORE Start, deliberately: os/exec documents that the caller
> sets Cancel and the other cancellation fields before starting the
> command, and the watchdog goroutine Start launches reads the field at
> cancellation time without synchronising on it - a post-Start write is
> a data race (-race catches it) even where it would happen to take
> effect.
>
> A descendant that ESCAPED the group (daemonised with setsid) survives
> this kill — but it can no longer stall Wait past cmd.WaitDelay below,
> which force-closes whatever exec-owned pipes it holds. What remains of
> the gap is bounded and accounted: the escapee itself outlives the
> session unfed (CLA-423 closed the hang, not the orphan), and an escapee
> holding the exec-owned STDOUT end still blocks consume() before Wait —
> and so before WaitDelay's clock — ever starts (CLA-431).

### A cap that can hang is not a cap

> A cap that can hang is not a cap: the same grandchild-holds-the-pipe
> scenario the opencode adapter closes with WaitDelay (opencode.go:291-303).
> Armed UNCONDITIONALLY. An earlier revision set it only when the exec
> context carried a deadline, but no deadline ever reaches this path in
> production — the driver's run ctx is signal.NotifyContext (cancel-only),
> sctx below is WithCancel-only, and claude honours no wall-clock cap of
> its own — so that arm was dead code outside tests (phase-2 review of
> this branch). Arming always is safe for live sessions by WaitDelay's own
> semantics: its clock starts only once the exec context is done or Wait
> has observed child exit, so I/O of a still-running session is never cut
> short. It does NOT kill the escapee; it only lets the adapter return and
> account for what arrived before the forced close. That close triggers
> the same stream-drain path the scanner already handles: EOF, clean
> parse, and any bytes past the window counted in OutputDropped.

### noteToolResult

> noteToolResult applies one clankerbar call's RESULT to res: it resolves the
> delivery claim noteToolUse armed on the request, and then whatever else that
> request was waiting to learn.
>
> A refused call changed nothing on the plane, so it must change nothing here
> either — which is the whole reason these are judged on the result rather than
> on the request. An `in_review` rejected for a missing Tests header leaves the
> task held, and that session is exactly the one whose claim needs handing back.
>
> It lives beside noteToolUse and is called by BOTH adapters that observe a
> session's clankerbar traffic: claude off its `user`/tool_result events, and
> opencode off the single terminal `tool_use` event that carries a call's input
> and output together (CLA-365). The two stream shapes differ; the rule about
> what a result MEANS is the plane's, not either harness's, so it is stated once.

### noteToolUse

> noteToolUse records what a clankerbar call will mean once its result lands.
>
> A terminal tool is not the only way a session lets go. A BLOCKING question
> ends the run and sets the task `blocked` without any task write at all — it is
> the documented one-call handback, and the protocol steers clankers here
> instead of any bare status write.
> Missing it is not a missed handback but a wrong one: the driver would post
> `ready` over a task that is waiting on the operator, dropping it back into the
> claimable queue with the question unanswered.

### noteClaimed

> noteClaimed records the task/run a successful claim_task returned. A payload
> missing either id (a lost race, a refusal, a shape we do not recognise) leaves
> the tracked claim alone — the driver must never be handed a half-claim it would
> then try to release.
>
> Both ways of giving up SAY SO. They used to return in silence, and the silence
> cost a whole live phased run to localise (CLA-330): everything downstream reads
> `Claim.Held()`, which is false on a zero Claim, so a claim that was never
> recorded is indistinguishable from a session that never claimed — no handback,
> no checkpoint, no complaint. A diagnostic here is the only place the difference
> is still visible.

### persistedOutputMarker

> The envelope Claude Code substitutes for a tool_result too large to inline: a
> pointer to the file holding the real thing, plus a truncated preview of it. It
> carries no `is_error` and nothing upstream flags it — the result simply stops
> being the JSON a parser expects.
>
> This is what broke the phase seam (CLA-330), and it is neither of the two
> causes that bug was first attributed to. A `claim_task` result reached 66KB and
> was spilled; `noteClaimed` could not parse the pointer it got instead, so
> `res.Claim` stayed at its zero value. The driver then read `Claim.Held()` as
> false and ended the drain - "the implement phase ended without holding the
> task" - while the plane had that task claimed the whole time, and phase 2 never
> ran.
>
> Where the 66KB came from is worth stating exactly, because the obvious guess is
> wrong: the decision list is ALREADY capped at 20, of 98. Those 20 were 48KB
> between them, on top of a 13KB task detail. So the payload grows with the
> LENGTH of decisions and details, not with how many exist - a count cap is not a
> size cap, which is the same lesson clankerbar's own docs/token-budget.md draws
> from the other side. Any agent-facing payload bounded only by a count will
> eventually cross this threshold.

### persistedOutputPath

> persistedOutputPath returns the file a spilled tool_result was written to, if
> this result is one.
>
> The path is honoured only when its base name is THIS tool call's id, and that
> guard is the point rather than a nicety. Everything inside a tool_result is
> potentially backlog text quoted back at the driver — the whole subject of
> injection_test.go — so an unguarded "read the path you find in here" would let a
> task body point the driver at a file of its choosing and have the contents
> parsed as a claim, which is a claim on a task nobody asked for and a handback
> aimed at it. A tool_use id is minted by the harness for THIS call in THIS
> stream; text written before the call cannot name it.

### readSpilled

> readSpilled reads a spilled tool_result under the same bound every other read
> in this package obeys.
>
> output.go states the invariant plainly - "a stream of any size costs a fixed
> amount of memory", and no dial, because a dial is a way to reintroduce the OOM.
> A whole-file read is bounded by nothing but what the plane chose to return,
> which is precisely the thing that grows without warning: clankerbar's own
> list_tasks reached ~253,000 tokens before anyone capped it. Today's spill is
> 66KB, so this is a latent cost rather than a live one, and the cheap time to
> bound it is while the bound is obviously generous.
>
> A file AT the limit is treated as unreadable rather than truncated, following
> maxStreamLine's own rule that a payload above the bound is a lost event and not
> a shortened one: half a JSON document parses as nothing anyway, and saying so is
> worth more than a parse error that names the wrong cause.

### snippet

> snippet bounds and sanitises a diagnostic that quotes a tool_result.
>
> Both of those follow failureDetail in the loop package, for the same two
> reasons, because this is the same kind of string: text from outside rendered to
> a terminal an operator then copies out of.
>
> Non-printables are STRIPPED because this changes what the text is for. Result
> content has until now only ever been fed to a JSON parser, where a control byte
> is inert; here it is printed, where ESC is executed - and this is backlog text,
> quoted verbatim into the stream, which is the threat model injection_test.go
> exists for.
>
> The cut is in RUNES, not bytes. The payloads quoted here carry "·" and "→" from
> the harness's own rendering, so a byte slice would cut a rune in half and emit
> U+FFFD at the seam - mojibake in the one string whose whole purpose is to be
> pasted into a bug report. "..." rather than U+2026 for the same reason.

### usage

> usage is claude's reported token accounting for a session.
>
> The cache fields are the whole point of counting them. `input_tokens` is
> UNCACHED input only, so summing it with output misses the cache reads and
> writes that dominate a long agentic session — one real run reported 140,387
> tokens against $147.98 of actual spend, about $1.05 per thousand, which is no
> model's price. A budget dial that undercounts by an order of magnitude is worse
> than no dial: max_tokens would silently allow roughly ten times what an
> operator set it to.

### probeResult

> probeResult turns a finished probe run into its Result: parse whatever the CLI
> managed to emit, THEN classify how the run ended (CLA-299).
>
> The order is the whole point. Run can fail with something other than an exit
> status after the process has already printed its answer — the JSON object is
> then sitting in stdout, and only the wait fell over. Parsing first means the
> spend figures survive alongside the error, which is what Probe owes the budget,
> since it charges on every path; the old order parsed AFTER the branch, so a
> probe that emitted and died returned raw Stdout/Stderr and zeroes — exactly
> the structurally-zero-on-error figures CLA-287 documents reading.
>
> Reachability, established rather than assumed: the streams are memory-backed
> tails (they never fail a write), the probe tees to no console, and it sets no
> WaitDelay — so no stub behaviour reachable on darwin or linux produces a
> non-exit Run error after output. A signal death and every context kill arrive
> as *exec.ExitError; a Start failure emits nothing and the zero Result is
> correct there. The realistic carriers are exotic pipe/IO faults, which makes
> the branch rare but not optional: both stream-path adapters parse whatever
> arrived before classifying the failure, and this seam is where the probe does
> the same. Pinned by TestClaudeProbeParsesBeforeClassifyingTheRunError, which
> drives this seam directly because no stub can produce the error end to end.

### output-format json is ONE object, so a trimmed tail does not merely

> `--output-format json` is ONE object, so a trimmed tail does not merely lose
> context here — it loses the answer, and parse would fail into a Result that
> reads exactly like "no limit found". A few hundred bytes never reaches the
> window, so this is the improbable path; it is checked because the whole point
> of this change is that a silent zero is worse than a loud stop.
>
> Either stream, not just stdout: a limit notice reaches a probe as CLI text on
> stderr when the JSON never starts, which is half of why this path exists.

### claudeScope

> claudeScope is how much of claude's output a scan is allowed to read.
>
> Both scans used to run over the whole of Stdout+Stderr, and Stdout is the entire
> raw NDJSON stream: every assistant message, and every `tool_result` block, which
> carries the verbatim MCP response to `claim_task`. So a backlog task whose body
> merely said "hit your" reported a usage limit that never happened — and the loop
> then slept, re-spawned the same paid session, re-claimed the same task and did it
> again, with every budget ceiling sitting inert inside the drain (CLA-258). Same
> defect, same fix, as opencodeErrorText.

### claudeText

> claudeText collects claude's own output at the given width, so a scan never
> reads the agent's narration.
>
> Non-event stdout lines are kept deliberately: under `--output-format stream-json`
> every word the agent says arrives inside a JSON event, so a bare line is the CLI
> talking — which is how a usage-limit notice arrives when the stream never starts.
> A line that starts like an event but does not parse is dropped rather than read
> raw; a half-written event is not something to classify a run on.

### claudeTransientRe

> claudeTransientRe anchors on the "API Error:" prefix (and bare connection
> errors) so a task log that legitimately mentions an HTTP 500 can't be mistaken
> for a dead session. A 400 bad-request is NOT here — retrying won't help it.
> Ported from loop.sh's TRANSIENT_RE.
>
> The anchoring is the second line of defence, not the first: it is applied to
> claudeText, so the agent's narration is not scanned at all.
>
> The mid-response arm is not a guess. Claude Code's error reference documents
> exactly three "the response above may be incomplete" variants, emitted when a
> stream fails after Claude has completed a block of text or a tool call:
>
> 	API Error: Server error mid-response. The response above may be incomplete.
> 	API Error: Connection closed mid-response. The response above may be incomplete.
> 	API Error: Response stalled mid-stream. The response above may be incomplete.
>
> All three missed every arm above. "Connection closed" is not "connection
> error"; "Server error" is not "internal server"; a stalled stream names no
> status code — so the 5\d\d arm has nothing to catch either. Every one of them
> is a network/server fault that a fresh session is exactly the right answer to,
> and the cost of missing one is not a lost iteration but a stopped daemon
> (loop.go treats an unrecognised non-zero exit as fatal).
>
> Matching on "mid-response|mid-stream" rather than on "connection|closed" keeps
> the arm as narrow as the documented wording: those two tokens are the CLI's
> own, they cover all three variants including ones with no connection word in
> them, and they do not fire on the ordinary English an error string quotes.
>
> The same doc says that in headless mode ("--output-format json" or
> "stream-json") this message is reported in the `result` field. claudeText only
> reads `result` when the CLI marked the session failed, which is the correct
> gate rather than a gap: a session that keeps its partial output and exits zero
> is never classified at all, and one that reports the turn as an error is
> exactly the one whose exit code brings the loop here.
>
> See https://code.claude.com/docs/en/errors#the-response-above-may-be-incomplete

### claudeMaxTurnsReason

> claudeMaxTurnsReason is what `claude -p --max-turns N` reports when the cap
> fires, verified against claude 2.1.226:
>
> 	exit 1, {"type":"result","subtype":"error_max_turns","is_error":true,
> 	         "result":null,"terminal_reason":"max_turns", ...}
>
> `result` is null and the text matches neither the usage-limit scan nor the
> transient one, so WITHOUT this classification the loop reaches its
> non-retryable branch and ends the whole run. That is the phase backstop
> killing the daemon it was added to protect.
>
> --max-turns is not in `claude --help` for that version; it is accepted (an
> unknown flag exits 1 with "error: unknown option"), so this is a dependency on
> an undocumented flag, pinned here against a named version deliberately.

### parseClaudeReset

> parseClaudeReset extracts the reset time from a message like
>
> 	You've hit your session limit · resets 9:40pm (Europe/Madrid)
> 	You've hit your weekly limit · resets Sunday 12:00am
>
> The reset is only an upper bound for the supervised wait (the loop still polls
> for an early reset), so this is deliberately best-effort: on any doubt it
> returns the zero time and the loop falls back to interval polling.


## `internal/harness/claude_inbox.go`

### Console - agent message delivery for the claude adapter (CLA-606)

> Console -> agent message delivery for the claude adapter (CLA-606).
>
> A message is plain input: it is written to the session's stream-json stdin as a user turn,
> in the plane's per-run order. The poller is its own goroutine, so a slow or failed plane
> call can only delay a message — never the session. The run id can change mid-session (a
> session may claim a task, settle it, and claim another), so each observed run restarts the
> poll with a fresh cursor; the newest claim wins.


## `internal/harness/claude_input.go`

### sessionInput

> sessionInput serialises the stream-json user messages a claude session reads from stdin.
>
> It exists because TWO writers share that pipe: the initial prompt (written by Invoke) and
> each console message (written by the inbox poller). It also owns the end-of-session rule:
> claude's stream-json input stays open until its stream closes, and closing it tells the CLI
> to "finish the current turn and exit". So every user message written counts as one
> outstanding turn, and each `result` event retires one. When a result retires the last one,
> the session is done and endTurn closes the pipe.


## `internal/harness/claude_stream.go`

### consume

> consume reads the session's stream-json stdout to the end: parsing and
> rendering every line as it arrives, and retaining a bounded tail in keep.
>
> ceiling is the per-session token ceiling (Invocation.MaxSessionTokens, 0 =
> none) and kill cancels the session's context. When the session's accumulated
> usage crosses the ceiling, the session is marked and killed ON THE SPOT —
> mid-stream, while the runaway is still spending — rather than read to the end
> and judged after. The kill closes our end of the pipe, so the scanner ends on
> a clean EOF: no partial events, no untrusted stream, and the spend parsed so
> far is the figure the budget sees (CLA-343).
>
> # A line too long is a wrong answer, not a missing one
>
> bufio.Scanner ends its loop on a line above maxStreamLine with
> bufio.ErrTooLong, and for years nothing here looked. Every consequence is a
> silent wrong decision by the supervisor, which is the one class this loop is
> built to avoid (CLA-262):
>
>   - The `result` event never arrives, so Tokens and CostUSD stay ZERO and a
>     session that may have spent hundreds of dollars reports nothing to the
>     budget breaker.
>   - A claim's settle is never observed, so the driver reads a task the session
>     handed to review as one it abandoned, and posts `ready` over it.
>   - Abandoning the pipe with the child still writing means cmd.Wait closes our
>     end, claude dies on EPIPE, and the exit code reads as a genuine
>     non-retryable failure — which stops the whole run.
>
> So the error is said out loud on the console (and so into the iteration log),
> the Result is marked untrusted for the driver, and the pipe is DRAINED to
> io.Discard so the child finishes its own write and exits on its own terms.
> Draining is not free — the bytes still arrive — but it is a fixed cost with
> nothing retained, and it buys an honest exit code.

### The kill is the end of this stream

> The kill is the end of this stream: the child is dying, and whatever
> bytes are still buffered in the pipe are not events we may read — the
> `result` event among them would overwrite the kill marker and turn an
> orderly ceiling stop into an unclassified failure.
>
> The cost of a killed session is therefore UNMEASURED, not zero: the
> result event is the only carrier of total_cost_usd, and we deliberately
> never see it. The driver says so on the ceiling branch rather than
> printing $0.0000 as if it were real (CLA-343 review).

### renderAndParse

> renderAndParse renders one stream-json event to the console (assistant text and
> tool-use markers) and captures the final result/usage/limit into res.
>
> Token accounting spans both event shapes, verified against claude 2.1.229
> (CLA-343): each `assistant` event carries the API's per-turn usage object
> under `message.usage` — summed here as the session's running total — and the
> final `result` event carries the CUMULATIVE session total, which overwrites
> the sum on arrival. The running total is what the mid-stream ceiling kill
> reads; the result total is what the budget sees.


## `internal/harness/codex.go`

### codex

> codex drives OpenAI's Codex CLI (`codex exec`). Divergences from Claude Code,
> per the spike, that this adapter has to absorb:
>   - Permissions are two axes: --sandbox {read-only|workspace-write|...} and
>     --ask-for-approval {untrusted|on-request|never}. "edits auto, shell gated"
>     ≈ `-s workspace-write -a never`.
>   - Output convention inverts: final message on stdout, events on stderr,
>     unless --json (then a JSONL event stream on stdout).
>   - No stable limit exit code and rate_limits is null in exec --json, so limit
>     detection is fuzzy text-matching and there is nothing to introspect.
>   - Invocation.MCPConfigPath is DELIBERATELY UNUSED. codex has no per-run MCP
>     flag and no reader for Claude's `.mcp.json` schema; it takes its servers
>     from `[mcp_servers]` in config.toml under CODEX_HOME, which ConfigDir
>     already pins. Saying so here because the field is on every Invocation and
>     silently dropping it read as wiring that was never there: `doctor`'s
>     workdir check passed a codex workdir green on the strength of an .mcp.json
>     no codex session would ever see (CLA-263). doctor now states the exclusion
>     rather than implying the wiring - see MCPConfigUse below.

### codexArgs

> codexArgs maps an Invocation to `codex exec`'s CLI dialect. Split out so the
> argument shape is unit-testable without executing anything - the parity with
> opencodeArgs.
>
> The prompt goes LAST, behind a `--` terminator. It is a bare positional, so a
> prompt that is itself a flag token would be parsed as a flag and the session
> would run with no message at all (or read one from stdin). Nothing from the
> BACKLOG can reach argv - Invocation.Prompt is set once from the config's
> `prompt` (loop.go), there is no --prompt flag, and the backlog client decodes
> counts rather than strings - so this hardens the config-file path (the CLA-260
> class), not a live injection hole.
>
> The second thing the terminator buys is subcommand disambiguation, and it is
> codex-specific: `codex exec` takes SUBCOMMANDS - resume, fork, review - in the
> same position the prompt used to occupy. Under the old shape a prompt of
> exactly `resume` was consumed as one, and the session ran somebody else's
> conversation instead of the drain. Behind `--` it is a prompt.
>
> CORRECTION to this commit's first message, which justified the reorder as
> dropping a reliance on "last-wins" for the pinned --sandbox/--ask-for-approval
> posture: that reliance never existed. Both are clap 4 Option args
> (ArgAction::Set), which ERROR on a repeated occurrence rather than taking the
> last one - so a prompt of `--sandbox danger-full-access` made codex exit
> non-zero, not run write-capable. The old shape was fail-loud. This change
> PRESERVES that safety property rather than creating it; only the recorded
> reasoning was wrong, and it is corrected here so the next reader does not build
> on it.

### Invocation.ExtraDirs (CLA-437) is deliberately NOT translated here

> Invocation.ExtraDirs (CLA-437) is deliberately NOT translated here: codex's
> sandbox has no per-invocation flag for extra writable roots — its
> `workspace-write` scope is the cwd plus what CODEX_HOME/config.toml's
> `[sandbox_workspace_write] writable_roots` names, which is operator-owned
> local config like every other grant. A codex session on a multi-repo project
> therefore needs those roots declared there; inventing a config rewrite from
> the driver would widen a sandbox the operator wrote by hand. Documented in
> the README alongside repos/primary_repo.

### p

> Parse as the stream arrives, retain only a bounded tail of it for the text
> scans, and tee live to the console when one is set (the JSONL event stream —
> a readable renderer is a TODO; raw is honest live output).
>
> Parsing live rather than walking a saved copy afterwards is what makes the
> cap safe: the usage figures are read out of events as they pass, so trimming
> the retained text cannot cost the budget a single token (CLA-262).

### p.finish has already run, so res carries everything the stream announc

> p.finish has already run, so res carries everything the stream announced
> before the failure — the CLA-299 ordering: parse whatever arrived, THEN
> classify the run error. A non-exit failure therefore returns a fully parsed
> Result alongside the error, and a launch failure (nothing was ever emitted,
> so nothing was ever parsed) returns an honest zero.
>
> What actually lands in that branch on darwin/linux, established while
> landing CLA-299: a console whose Write fails mid-session (a full disk under
> the iteration log) surfaces as os/exec's copy error — a real ran-and-emitted
> case, pinned end to end by TestCodexInvokeReturnsAParsedResultAlongsideANonExitRunError.
> No WaitDelay is set here, so the grandchild-holds-the-pipe shape cannot
> error: it blocks in Run until the pipe closes (opencode's wall-clock cap is
> the bounded variant). Every signal and context kill arrives as
> *exec.ExitError instead.

### codexParse

> codexParse accumulates FinalMessage and Tokens (for the Budget) from the JSONL
> stream, ONE LINE AT A TIME as it arrives.
>
> Incremental rather than a walk over saved output, so the retained text can be
> capped without the token count depending on how much of the stream survived —
> see output.go.

### Take the LAST usage-bearing event as the session total

> Take the LAST usage-bearing event as the session total — never sum.
> Confirmed against codex 0.144.6 (rust-v0.144.6): `codex exec --json`
> emits one `turn.completed` per turn whose top-level `usage` is the
> CUMULATIVE session running total (built from `total_token_usage`), not a
> per-turn delta — so summing would double-count. Unlike the opencode
> adapter, which sums per-step deltas, here we keep only the final total.
> (`token_count` is a separate rollout/app-server stream not on --json
> stdout; its top-level fallback below is harmless defensive parsing.)

### codexErrorText

> codexErrorText collects the harness's OWN diagnostic text — all of stderr, the
> stdout lines that are not events at all, and the events that carry an `error` or
> announce a failure — so a limit or transient scan never reads the agent's
> narration.
>
> Both scans used to run over the whole of Stdout+Stderr, and under `--json`
> stdout is the event stream: the agent's messages, and its tool output, which
> includes the verbatim MCP response to `claim_task`. A backlog task whose body
> merely said "usage limit" therefore reported a subscription cap that never
> happened, and the loop paused and re-spawned the same paid session on a cycle no
> budget ceiling stopped (CLA-258). Same defect, same fix, as opencodeErrorText.
>
> Non-JSON stdout lines are kept deliberately: every word the agent says arrives
> inside an event, so a bare line is the CLI talking — which is how codex reports a
> cap it has no typed event for.

### Capabilities

> This adapter does not populate Result.Claim — it does not watch the session's
> clankerbar tool calls at all — so the driver's handback, salvage and delivery
> check are all inert under it, and `phases` is refused for it in config.Validate
> rather than half-working. Flip TracksClaims the day claim observation lands here.
>
> ReportsCost is false for a harder reason than "not implemented yet":
> `codex exec --json` reports tokens, never money, so there is nothing for the
> adapter to read. `budget.max_cost_usd` is therefore inert under codex, and
> `doctor` says so when it is the only ceiling set (CLA-288).


## `internal/harness/copilot.go`

### copilot

> copilot drives GitHub Copilot CLI (`copilot -p`) headlessly. Everything below
> is recorded against GitHub Copilot CLI 1.0.112 (the GA line; see
> docs/copilot-cli.md for the flags and event shapes this adapter relies on):
>
>   - `-p/--prompt` runs one non-interactive session and exits. `--output-format
>     json` turns stdout into a JSONL event stream (one object per line); the
>     final answer rides an `assistant.message` event and the session's usage a
>     `session.usage_checkpoint`.
>   - In `-p` mode a tool permission request that would prompt is DENIED, so the
>     posture is fail-closed before any rule is added; `--allow-tool` /
>     `--deny-tool` are what open the working set the driver needs. Deny rules
>     win over every allow, including --allow-all.
>   - `--additional-mcp-config @<file>` registers the operator's `mcpServers`
>     file for one session, which is why this adapter reads `mcp_config_path` in
>     Claude's schema: the shapes are the same object.
>   - COPILOT_HOME is the config/state dir (the CLAUDE_CONFIG_DIR / CODEX_HOME
>     analogue); auth lives in the OS keychain or an env token, so a pinned home
>     is not a pinned login.
>   - `--usage-output-file` also exists and reports AI credits; the JSONL
>     checkpoint carries the same figures, so no temp file is written.
>
> Billing is METERED since 2026-06-01: a monthly AI-credit allowance, then
> per-token overage. There is no rolling window to wait out, so the credit/
> allowance exhaustion this adapter recognises is a Limit.Stop; the budget
> breaker is the control. The CLI's session/weekly/model RATE limits are short
> rolling windows and are retried as transient instead.

### copilotLimitRe

> copilotLimitRe recognises the CLI's documented credit/allowance exhaustion
> statements (`copilot help billing`, `copilot help limits`): the plan's monthly
> credit allowance running out and the session AI-credit cap.
>
> Deliberately NOT the bare word "usage limit" or "quota": the CLI's warning at
> 75% of a weekly allowance contains both and is not a limit.
>
> Deliberately NOT the session/weekly/model RATE limits either: those are short
> rolling windows the CLI states a reset for, the same provider blip that
> arrives as a bare 429 - IsTransient retries them, and a Stop there would end
> a whole run on a wait it could have taken.

### Capabilities

> copilot observes the session's clankerbar tool calls through the shared
> observer (noteToolUse/noteToolResult), so Result.Claim, Reports, Settled and
> ClankerbarCalls are all populated and a phased config is allowed.
>
> ReportsCost is false for a harder reason than "not implemented": Copilot
> reports AI CREDITS (and premium requests), never money. One credit's dollar
> value depends on the plan and on whether the call is inside the included
> allowance, so mapping it to CostUSD would invent a price. `budget.max_cost_usd`
> is therefore inert under copilot, and `budget.max_tokens` - fed by the
> checkpoint's token counts - is the breaker; doctor says so (CLA-288).


## `internal/harness/events.go`

### The per-harness normalizers for the agent event channel (CLA-603, tran

> The per-harness normalizers for the agent event channel (CLA-603, transcript
> tier CLA-887): tool_call, todo_change, clankerbar_mcp_call, tool_result and
> assistant_text. codex and copilot are not mapped in v1 — their Capabilities
> report EmitsEvents false — and the design table is docs/agent-events.md.
>
> The one shared rule: a target is ONE extracted string per tool shape, never
> the raw input JSON.

### noteClaudeToolResult

> noteClaudeToolResult maps one claude `user`/tool_result block onto the
> transcript tier. The stream reports a result on a LATER event than the call,
> which is why the `.env*` decision (drop) is taken from the call id at
> tool_use time (Result.noteEnvRead).
>
> A result Claude Code spilled to a file is rehydrated before sending — the
> model saw the output, not the pointer, and the transcript is a record of the
> session. The path is honoured under persistedOutputPath's own guard; if the
> read fails, the envelope text is what we have and is sent.

### toolTarget

> toolTarget extracts the one identifying string from a tool call's input, by
> tool shape. Both harnesses' spellings of the same shape are listed together:
> claude's Read/Edit take `file_path`, opencode's read/edit take `filePath` (and
> some versions `path`), and a shell tool takes `command` on both.
>
> Unknown tools fall back to the first recognisable among the common keys, and
> a tool carrying none of them sends an EMPTY target (the wire key is required
> and always present, not omitted). That is deliberate: a status line is better
> with an empty target than with a dump of an arbitrary input object, which is
> exactly what the "never the raw input JSON" rule forbids.


## `internal/harness/harness.go`

### IsUnclassifiedTransient

> IsUnclassifiedTransient reports that IsTransient said yes on a HEURISTIC
> rather than on a recognised pattern — "this exit looks more like a blip than
> a stop" — so a caller can bound those retries separately. It must be false
> whenever IsTransient is false, and false for an adapter whose IsTransient is
> entirely pattern-driven.
>
> The distinction is a spending one, not a taxonomic one. A pattern match NAMES
> the failure, so retrying it is a bet on a blip this adapter has seen before,
> and the operator's retry dials are the right bound. A heuristic match names
> nothing, so a DETERMINISTIC failure that satisfies it re-spawns a paid
> session every time, forever — and every dial that would have stopped that is
> either off by default (max_retries, budget) or structurally unable to fire
> (CLA-381: opencode's heuristic requires reported usage, which is exactly what
> resets the zero-spend counter). A caller cannot bound what it cannot
> distinguish, and both used to arrive as a bare `true`.
>
> On the interface rather than an optional one a caller type-asserts for, and
> for the reason MCPConfigUse gives below: an assertion is not coupled to the
> registry. An adapter that grew a heuristic and did not report it would earn
> an unbounded paid retry ladder with no test failing, and any wrapper that
> embeds Adapter would silently drop the assertion besides.

### TurnCapped

> TurnCapped reports whether the session ended because it hit
> Invocation.MaxTurns, rather than finishing or failing.
>
> It has to be its own classification because a capped end looks exactly like
> a fatal one from the outside: Claude exits 1 with `terminal_reason:
> "max_turns"`, which matches neither the usage-limit scan nor the transient
> one, so without this the loop would call it non-retryable and end the whole
> RUN — turning the phase backstop into the thing that kills the daemon.
> An adapter with no turn cap returns false for everything.

### TokenCeilingHit

> TokenCeilingHit reports whether the adapter itself killed this session
> because its cumulative usage crossed Invocation.MaxSessionTokens
> mid-stream. The driver treats it exactly like a turn cap: the phase ends,
> the salvage handles whatever the tree holds, and nothing is retried or
> failed — the kill was the point, not a fault. The marker is the
> adapter's own (never text the CLI or an agent could emit), so it cannot
> be forged by a task body. An adapter with no mid-stream ceiling returns
> false for everything.

### WallClockCapped

> WallClockCapped reports whether the adapter itself ended this session
> because it outlived Invocation.MaxSessionWallClock.
>
> It is the turn cap's stand-in for a harness whose CLI takes no turn flag,
> so the driver treats it exactly as it treats the other two: the phase
> ends, and nothing is retried or failed - the kill was the point.
>
> What it does NOT buy today is the salvage. That runs only on a session
> whose claim the adapter observed (Capabilities.TracksClaims), and since
> CLA-365 opencode has BOTH HonoursSessionWallClock and TracksClaims - so
> a capped opencode session holding its claim IS salvaged (loop.go branches
> on res.Claim.Held(), and the claim-observing arm is the one that fires).
> Before CLA-365 the adapter enforcing this cap did not observe claims, so
> a capped session left its uncommitted work in the worktree; that is the
> historical reason this path says what it says, not its present behaviour.
> The two capabilities are independent on purpose: the day an adapter has
> both, this same path hands it the checkpoint too - exactly the shipped
> present since CLA-365, when opencode gained claim observation. Like the
> token ceiling, the marker is the adapter's own, never text the CLI or an
> agent could emit, so a task body cannot forge one. An adapter with no
> wall-clock cap returns false for everything.

### ZeroUsageUnknown

> ZeroUsageUnknown reports whether the session ended with a FINAL
> step_finish carrying reason "unknown" and all-zero usage — the quiet-death
> signature (CLA-398): no error event, no tokens, no cost, nothing produced,
> which is otherwise indistinguishable from a cheap clean run. The marker is
> the adapter's own (a terminal_reason it writes, never text the CLI or an
> agent could emit), so it exists so the driver can NAME the end instead of
> logging "iteration done (tokens=0 cost=$0.00)". An adapter whose stream
> carries no such signature returns false for everything.

### Diagnostic

> Diagnostic returns the harness-authored text IsTransient and DetectLimit
> were judged on — stderr, the CLI's own non-event output, typed error
> events — with the agent's narration excluded exactly as those scans
> exclude it. It exists so a caller that STOPS on a classification can name
> the message it stopped on: an unrecognised non-zero exit ends the whole
> run, and "exited 1 (non-retryable)" tells an operator nothing about which
> failure that was. Implementations must return the same scope IsTransient
> reads, never a wider one.

### MCPConfigUse

> MCPConfigUse declares what this adapter does with Invocation.MCPConfigPath,
> so a preflight can say something TRUE about the operator's `.mcp.json`
> instead of inferring one from the field's existence.
>
> It is on the interface, not a lookup table in cli, because a table is not
> coupled to the registry: the first version of this lived in doctor as a
> switch with `default: return true`, and registering an adapter that did not
> carry the config would have silently earned a green workdir with no test
> failing (CLA-263). Here the compiler asks the question at the moment an
> adapter is written, which is the only moment the answer is known.

### MCPConfigSchema

> MCPConfigSchema is what a harness makes of the file at Invocation.MCPConfigPath.
>
> The distinction that matters is NOT "does the adapter hand the path over" -
> that is the question doctor used to ask, and opencode answers it yes while
> still dying on the file. It is "does that file MEAN anything to that harness".

### TracksClaims

> TracksClaims reports whether this adapter watches the session's clankerbar
> tool calls and populates Result.Claim.
>
> Everything the driver does about a task hangs off Result.Claim.Held():
> the handback, the CLA-314 salvage, and the CLA-253 delivery check. An
> adapter that never populates it leaves all three inert — which is
> survivable for an unphased run (nothing is being handed across a seam) and
> is NOT for a phased one, where the sequence would implement and push and
> then stop after phase 1 on every task, announced by a log line that reads
> like an ordinary early finish. config.Validate refuses `phases` without
> this rather than let that happen quietly.

### ReportsCost

> ReportsCost reports whether this adapter ever populates Result.CostUSD.
> False means `budget.max_cost_usd` is INERT for this harness - not
> approximate, not late: no code path can ever reach it, because nothing
> feeds the number it compares against. codex exec reports tokens, not
> money, and Copilot reports AI credits, so those are codex and copilot
> today.
>
> It is on Capabilities rather than inferred from a zero CostUSD because the
> two are indistinguishable at runtime: a claude session that genuinely cost
> nothing also reports 0. Only the adapter knows which case it is in, and
> `doctor` has to tell an operator BEFORE the run whether the ceiling they
> configured exists (CLA-288).

### HasSessionTokenCeiling

> HasSessionTokenCeiling reports whether this adapter enforces
> Invocation.MaxSessionTokens mid-session - i.e. whether TokenCeilingHit can
> ever fire. False means CLA-343's per-session runaway ceiling does not exist
> on this harness, so `doctor` must not print a number for it: reporting one
> would announce a guard that cannot fire, which is the same reassuring
> falsehood ReportsCost is on this struct to prevent. Claude alone has one
> today, which doctor used to encode as a literal `== "claude"` - a comparison
> that silently became wrong the moment a phase could name its own harness
> (CLA-366), because the question is about the harness running the PHASE.

### ExtraDirs

> ExtraDirs names additional directories the session must be able to read and
> edit BESIDES its working directory: the project's other declared repos, plus
> any conventional worktree area beside the spawn checkout (agent-rule-scoping
> piece 2 — permissions follow the project, not the cwd). Empty on every run
> whose project declares no repos, which leaves each adapter's policy exactly
> as it was. Each adapter expresses these in its own dialect — opencode's
> pinned `permission` block gains read/edit/external_directory scope,
> claude gets --add-dir — and an adapter with no such concept ignores them;
> see the adapters for what each does and documents.

### Nice

> Nice is how much LOWER than the daemon this session's processes should be
> scheduled: 0 (the default) is unchanged, and a positive value runs the
> harness and everything it forks — test runners, builds, dev servers — at
> that much worse a priority. It is for a machine where the drain fleet
> shares cores with something that must not wait, such as a self-hosted CI
> runner: the kernel serves the other process first under contention and
> still gives the session everything when the machine is idle.
>
> The driver resolves it from Config.SessionNice. Only a supported platform
> accepts a positive value (SessionPrioritySupported); adapters go through
> sessionCommand, which applies it at spawn so the whole tree inherits it.

### MaxTurns

> MaxTurns caps the session's turns so a phase boundary lands even if the
> model works past its brief. 0 = uncapped, which is every unphased run.
>
> It is a BACKSTOP, not the mechanism: the boundary is meant to land because
> the phase's prompt scoped the session, and a session cut off here has by
> definition been stopped mid-thought. What makes that survivable is the
> salvage that already runs on every session end — it commits and pushes
> whatever the tree holds — so a hit cap costs a scruffy commit, not work.
> An adapter whose CLI has no equivalent ignores it, and the phase then
> relies on the prompt alone.

### MaxSessionWallClock

> MaxSessionWallClock bounds this ONE session by elapsed time: when it is
> exceeded the ADAPTER kills the process and marks the Result so the driver
> treats the end like a turn cap (end the phase, salvage what it left, do
> not retry or fail). The driver resolves it per phase from
> Config.MaxSessionWallClock / Phase.MaxWallClock. 0 = no cap for this
> invocation, which is the default and every probe.
>
> It exists because MaxTurns reaches only the claude CLI: under a harness
> that takes no turn flag the phase backstop is otherwise nothing at all.
> Only an adapter reporting Capabilities.HonoursSessionWallClock enforces
> it; opencode and copilot are those adapters today.

### MaxRepeatedToolFailures

> MaxRepeatedToolFailures ends this session when the same tool returns the
> same error on this many CONSECUTIVE tool calls (CLA-739): the adapter kills
> the process mid-stream and records the cause on Result.ToolLoop, and the
> driver classifies the session as dead. The driver resolves it from
> Config.MaxRepeatedToolFailures. 0 = no check, which is also every probe.
>
> Enforced by the adapters that observe tool results as they stream: claude
> and opencode. codex and copilot do not, and the field is inert there.

### MaxSessionIdle

> MaxSessionIdle bounds how long a session may go without ANY stream output
> before the adapter kills it as a silent hang: the adapter kills the
> process mid-session and records the cause on Result.IdleKilled. While a
> tool call is open the idle clock is paused (a build or an MCP call can
> legitimately emit nothing) and MaxToolCallDuration bounds that call
> instead. 0 disables the watchdog for this invocation; probes never arm it.
>
> The loop resolves it per harness from Config.SessionIdleFor and classifies
> an IdleKilled end as dead, like a tool-loop kill. Enforced by claude and
> opencode; inert under codex and copilot.

### ResumeClaim

> ResumeClaim is the claim a PREVIOUS phase left held, seeded into this
> session's Result before its stream is parsed.
>
> It exists because a resumed phase is told not to claim anything — it is
> continuing a run, not starting one — so it never calls claim_task, so the
> adapter would otherwise observe no claim at all. That is not cosmetic:
> Result.Claim.Held() is the gate on the handback, the salvage and the
> delivery check, so an unseeded resumed phase runs with all three inert,
> and it is the phase that does the pushing. Seeding restores them, and
> leaves Settled/HasWIP to be observed from the stream as usual.

### OnClaim

> OnClaim, when non-nil, is called by the adapter's parser the moment it
> observes the session's clankerbar claim state change: a claim_task whose
> result carried both ids (the tracked claim), and a settle of that claim.
> It exists for driver-side lease renewal (CLA-358): the loop starts a
> renewer around Invoke and learns the run id to renew from this callback
> mid-session, instead of only reading Result.Claim after the child exits.
>
> A claim that LOST its race fires nothing - its result recorded no ids,
> so there is nothing to announce. Called on the parser's goroutine;
> implementations must not block.

### Turns

> Turns is an optional ordered list of follow-up turns to run in the SAME
> session as the first turn (Prompt/Model): each is sent as a continuation
> of the session the first turn created, so later turns reuse everything the
> earlier ones read. Empty - every invocation before this field existed -
> means one turn, byte-for-byte the previous behaviour.
>
> Only an adapter that can continue a live session may honour it; one that
> cannot must REFUSE an invocation with Turns rather than silently drop the
> later turns, because a half-run sequence is a wrong answer, not a shorter
> one. opencode is the adapter with that capability today (claude and codex
> refuse it with ErrTurnsUnsupported).

### NewEventSink

> NewEventSink, when non-nil, builds this session's agent-event sink. The
> loop sets it only when the project's events.activity dial is on, the
> machine has not opted out, and the adapter emits events; the sink's own
> Config carries the events.transcript dial, which is what decides whether
> tool_result and assistant_text are sent at all. It is a FACTORY
> because one Invocation may run several processes (an opencode resurrection
> or a multi-turn sequence), each with its own batch goroutine over a shared
> per-run sequence counter.
>
> The adapter MUST close what it builds (Result.closeEvents /
> opencodeParse.closeEvents) when the session ends. Nil means the channel is
> off.

### Inbox

> Inbox wires the console -> agent message channel for this session (CLA-606).
> The loop sets it only for an adapter whose Capabilities.DeliversMessages is
> true and only when the plane origin and key are configured. The adapter polls
> the run's inbox and delivers each message the way its harness can: claude
> writes it to the session's stdin as a user turn; opencode steers it into the
> live session through its private server's session API (CLA-911). The run id
> arrives from ResumeClaim or OnClaim, because the session claims inside itself.
>
> Nil means no delivery: every other harness in v1, and every probe.

### SessionRef

> SessionRef is the harness session this session runs as/into
> (studio-chat.md §3). Empty on every task-drain session, which starts a
> fresh unnamed session exactly as before.
>
> With ResumeSession false it is the id the session is CREATED under —
> claude's `--session-id`, opencode's `run -s` (which creates a missing
> session) — so the planner knows the handle before the model answers.
> With ResumeSession true it names the session to continue.

### ModelArg

> ModelArg is the alias to put after an adapter's model flag, or "" for "emit no
> flag at all and let the harness pick".
>
> Every adapter goes through this rather than reading Model itself, so the
> blank-but-not-empty case is handled in ONE place. A `"model": " "` in a config
> file, or a tier mapped to a whitespace string, passes an `!= ""` check and then
> reaches the child as a model alias no provider has — an unattended run that
> dies on every session, for a space nobody can see in the config. Trimming here
> makes it indistinguishable from the unset case it was obviously meant to be.
>
> A ratchet in the tests sweeps this package for adapters reading `.Model`
> directly, because the failure mode is a NEW adapter, not this one.

### Result is the outcome of one session, both raw and parsed

> Result is the outcome of one session, both raw and parsed.
>
> Stdout and Stderr are the retained TAIL of each stream, not the whole of it: a
> session's output is unbounded and the supervisor has to survive it (see
> output.go). Everything parsed here is taken from the stream as it arrives, so
> the trimming costs the classifiers context they do not use — unless a single
> line overran maxStreamLine, which is what Untrusted is for.

### RecoveredErrorsKey

> RecoveredErrorsKey is the Result.Raw key an adapter uses to record that its
> session's stream carried a typed error event the session then RECOVERED from —
> it retried and carried on — with no error event ending the stream (CLA-700).
> The loop reads it off Raw rather than growing Adapter, the same way it reads
> FinishReasonKey: it is the adapter-to-driver channel, and only opencode emits
> this signal today.
>
> Its value is the COUNT of recovered events (an int), so a diagnostic can say
> how many provider blips the session rode out. Absent — rather than zero —
> when the session recovered from nothing, which is both the common case and
> the one a missing key already means.

### RecoveredErrors

> RecoveredErrors reports how many typed error events the session's stream
> RECOVERED from — events with further transcript after them, and no error event
> ending the stream (CLA-700). Zero covers both "nothing went wrong" and "the
> stream ended on its error": the difference that matters to a classifier is
> whether there is an unexplained exit status to judge, and only the second
> shape has one.
>
> A RESURRECTED session (mergeResume) reports the count of the run that ENDED
> it, not the sum of its runs: the marker answers "did this session recover",
> and a continuation that ended on a real terminal error must read as not
> recovered however many blips the dead run rode out first.

### OutputCapReason

> OutputCapReason is the terminal_reason the opencode adapter writes into a
> Result when the session's final step_finish carried reason "length" with
> ZERO output tokens — the CLA-584 output-cap stall, read back through
> opencode.OutputCapNoOutput. It is a DIFFERENT end from the CLA-398 quiet
> death: the model ran, spent real output budget on reasoning and emitted
> nothing, and opencode ended the turn cleanly — so the session is alive and
> resumable, where the quiet death's stream was dropped. The driver names the
> end with it in place of its generic "never moved the task on" line.

### TokenUsage

> Usage is one session's token accounting in the CLI's OWN normalised
> vocabulary: every adapter's report mapped onto the same five buckets, so a
> consumer can compare two models, or reprice an old session against today's
> rates, without knowing which harness ran it (CLA-691).
>
> The buckets are DISJOINT and sum to the total the harness itself reported —
> that is what makes repricing possible, and why each adapter is mapped by
> MEANING rather than field-for-field by name:
>
>   - claude reports input_tokens exclusive of the two cache figures, so those
>     are the three buckets directly; thinking tokens are inside output_tokens
>     (Anthropic folds them in), so Reasoning stays zero rather than being
>     invented.
>   - opencode's per-step tokens are already disjoint — its own `total` sums
>     input+output+reasoning+cache — so every bucket is reported directly.
>   - codex reports INCLUSIVE counters: cached_input_tokens is a SUBSET of
>     input_tokens, and reasoning_output_tokens a subset of output_tokens. A
>     field-for-field copy of that report would double count both, so the
>     cached reads are subtracted out of Input and the reasoning tokens out of
>     Output. The five buckets then sum to codex's own total_tokens.
>
> A harness that reported no usage at all leaves this zero, and Result.UsageReported
> — not a zero here — is what tells the two apart.

### Total

> Total is the sum of the buckets: the token figure the harness itself
> reported, recomputed here so a consumer repricing bucket by bucket can add up
> what it was given.
>
> For claude and opencode it equals Result.Tokens exactly. codex is the one
> exception TODAY, and the divergence is a known defect of the BUDGET figure
> rather than of these buckets: codex's `reasoning_output_tokens` is a subset of
> its `output_tokens`, while Result.Tokens still adds the reasoning count a
> second time (CLA-692 tracks the fix). These buckets sum to codex's own
> total_tokens, which is the correct figure.

### TurnMessages

> TurnMessages is the final message of EACH turn of a multi-turn invocation,
> in order: TurnMessages[0] is the first turn's (Invocation.Prompt) and the
> last entry is the same text FinalMessage carries. It exists so the caller
> can put a notes turn's answer in the iteration log without confusing it
> with the findings turn that followed it.
>
> Empty for a single-turn invocation (the pre-Turns shape), and shorter than
> the planned sequence when a turn ended it early - len(TurnMessages) is how
> many turns actually ran.

### Usage

> Usage is this session's token accounting normalised across harnesses (see
> TokenUsage's own comment for the buckets and the per-adapter mapping). It is
> the durable, repriceable form of what Tokens only totals, and it rides the
> fleet report's per-session entry (CLA-691) so a per-phase/per-reviewer
> cost analysis has something finer than the iteration's summed figure.
>
> Zero when the harness reported no usage, which UsageReported distinguishes
> from a reported zero.

### CostReported

> CostReported says the adapter actually SAW a cost figure for this session,
> as distinct from CostUSD merely being zero — the same question UsageReported
> answers for tokens, and NOT the same event.
>
> claude is why this is its own field: it reports per-turn TOKENS on the
> stream but the session's DOLLARS only on its terminal result event, so a
> claude session killed mid-stream (the token ceiling, the turn cap) has
> UsageReported true with CostUSD still 0 — a zero nobody measured. The fleet
> record's costUsd is nullable for exactly this reason, and this flag is what
> lets it stay absent instead of claiming "$0 spent".

### UsageReported

> UsageReported says the adapter actually SAW a usage-bearing event on this
> session's stream, as distinct from Tokens/CostUSD merely being zero.
>
> Those are not the same question, and the driver needs this one. A session
> killed before the harness reports anything leaves Tokens and CostUSD at
> zero, so it contributes nothing to the Budget - and a retry ladder made of
> such attempts can never reach a token or cost ceiling, whatever the
> operator set (CLA-288). A session that ran and legitimately spent nothing
> is a different thing entirely: it reported, and the report happened to be
> zero. loop.drainPhase bounds the first and not the second, so what it
> counts has to be the presence of the REPORT, never the size of it.

### Untrusted

> Untrusted, when non-empty, says why this Result's PARSED fields cannot be
> believed: the child's stream could not be read whole, so an unknown number
> of events never reached the parser.
>
> It is a claim about the FIGURES, not about the child. Everything here is
> derived from events, and the ones that matter most arrive last: the `result`
> event carries the whole session's tokens and cost, so a stream cut short
> reports ZERO SPEND for a session that may have cost hundreds of dollars, and
> the settle of a claim never observed leaves Claim.Held() true on a task the
> session actually handed to review. Both are wrong decisions rather than
> missing ones, which is why this is a field and not a log line: a caller acting
> on the numbers has to be able to see that they are not answers.
>
> The driver's contract, in loop.drainWithRetries: do not count the spend, do
> not release the claim, do not classify the exit. See CLA-262.

### ReportResumeFailed

> ReportResumeFailed is the cause when the adapter's bounded re-emission of
> a refused done report failed: it stalled on a silent provider and was
> killed by the REPORT RESUME's own short idle bound, its process errored
> before producing a reply, its reply was still not a report the runner could
> apply, or the phase's wall clock left no room to resume at all (CLA-877).
> Empty for every other end.
>
> It is deliberately separate from IdleKilled: the session's own run ended
> as it did (usually cleanly, with its work pushed), and only the follow-up
> reply that re-emits the report failed. The adapter therefore leaves the
> original process outcome in place and sets this marker instead, so the
> driver can tell a resume-only failure from a dead session and retry the
> phase inside the claim it still holds, rather than ending the lease and
> paying a reclaim. Like ToolLoop/IdleKilled it is the adapter's own verdict.

### OutputDropped

> OutputDropped is how many bytes of this session's output were NOT retained,
> across both streams — the ordinary cost of the rolling window, not a fault.
>
> Worth carrying because it is the difference between "the classifier read this
> session" and "the classifier read the last couple of MiB of it". A run that
> stops on a non-retryable exit says which message it stopped on; without this
> the operator cannot tell whether the message they are being shown was the only
> candidate or merely the last one to survive the window.

### ClankerbarCalls

> ClankerbarCalls is how many clankerbar MCP tool calls this session got a
> SUCCESSFUL result for. A refused call (the tool was not connected yet, the
> plane rejected it) does not count: it says nothing about whether the
> backlog was reachable, and the driver's no-MCP classification is exactly
> the question "was the backlog reachable at all?".
>
> Only adapters that observe a session's clankerbar traffic populate it
> (Capabilities.TracksClaims); on any other adapter it stays zero and the
> driver must not read a zero as "the MCP server was missing".

### CheckpointUnverified

> CheckpointUnverified marks this Result as the hand-off for a phase that
> reached its checkpoint on an UNVERIFIED branch (CLA-576): the phase
> recorded Claim.Branch on the plane, but the driver's read of the origin
> remote failed (network, a GitHub outage, a credential problem), so it is
> unknown whether the branch was ever pushed. The branch is still carried —
> the review phase is the backstop — but the successor's brief must not
> describe it as verified on the origin remote (CLA-457), and the review
> session is told to verify the hand-off itself. False for every verified
> checkpoint and for the branch-less plane-record forms (CLA-497).

### scanCache

> scanCache holds the scoped text a classifier scans, built once per session
> instead of once per call.
>
> DetectLimit, IsTransient and Diagnostic are each handed the whole Result and
> each used to rebuild the same scoped copy of it, three times per session for
> text that never changes. Bounding the retained output (output.go) already turned
> that from hundreds of megabytes into a couple, and this makes it once.
>
> Not safe for concurrent use, and it does not need to be: one Result belongs to
> one finished session, and the driver classifies it in sequence on the goroutine
> that ran it.

### scan

> scan returns the text for key, building it on first use.
>
> A POINTER on Result, deliberately: every Adapter classification method takes
> Result BY VALUE, so a cache stored inline would be filled in on a copy and
> thrown away. Nil is a fully working, simply uncached Result — which is what a
> hand-built literal in a test is, and why nothing may depend on the memo being
> there. `key` is the adapter's own notion of scope (claude has two widths; codex
> and opencode have one), and a Result only ever comes from ONE adapter, so the
> key spaces cannot collide.

### settleReport

> settleReport resolves an armed delivery claim on its tool_result. accepted=false
> (the plane refused the call) drops it: nothing was recorded, so there is nothing
> to verify.
>
> Identical claims collapse. A clanker that carries `branch` on every progress
> update — which the protocol encourages, since the branch is the hand-off record
> — restates the same claim many times in one session, and each restatement would
> otherwise cost a network round trip, a duplicate log line, and a duplicate write
> to the plane.

### Report

> Report is a delivery claim the plane accepted: the branch a session recorded as
> its hand-off, and/or the commit it declared landed on an integration branch.
>
> Both are claims the plane cannot check — it holds the backlog and takes a
> clanker at its word — and both have been wrong in ways that cost real work: a
> branch recorded but never pushed past its first commits reads as a hand-off to
> the next clanker and is a dead end. The driver is local and in the git tree, so
> it can simply look (CLA-253).

### ClaimsMerge

> ClaimsMerge reports whether this is a claim that the delivery has LANDED, which
> is the only thing the ancestor check can sensibly be run against.
>
> A hand-off to `in_review` routinely carries the delivery it is proposing, and at
> that moment the commit has by definition not merged — nobody has reviewed it
> yet. Checking it anyway would fire the loudest log line in the feature on the
> happy path and stamp `mergeVerified: false` on a task that is perfectly fine,
> which is how a warning system trains its operator to ignore it.

### Claim

> Claim is what a session did with the backlog: the task it most recently
> claimed, and whether it let go of that task deliberately.
>
> Only the LATEST claim is tracked, which is the one that matters — a drain
> claims tasks one after another, and every earlier one was necessarily settled
> (or lost to a race) before the next claim was made. An unsettled latest claim
> is exactly the lease that would otherwise expire in silence.

### Releasable

> Releasable reports whether the driver may hand this claim straight back to the
> queue.
>
> A held claim with WIP is deliberately excluded, and the asymmetry is the point.
> With no branch recorded, releasing to `ready` is exactly what the plane's own
> expiry sweep would have done half an hour later, minus the reclaim it would
> have charged — strictly better. With a branch recorded it is strictly WORSE:
> `requiresTakeover` is computed only for an `in_progress` task whose lease has
> died, so moving the task to `ready` throws away the flag that tells the next
> clanker there is pushed work waiting. Letting that lease expire preserves the
> handoff and costs one reclaim; releasing it saves the reclaim and strands the
> work. Until the plane grows a release that keeps the handoff, the driver takes
> the expiry.

### settlesTask

> settlesTask reports whether a terminal call carrying this status would release
> the task plane-side.
>
> Stated as "anything but in_progress" rather than as a list of terminal
> statuses, because that is the plane's own rule: `updateStatus` clears the
> holder for every status except `in_progress`. An allowlist here drifts the
> moment a status is added — and a status this side has not heard of would be
> read as "still held", which is the reading that produces a wrong write.

### Window

> Window is the usage window the limit names, as a human label: "5-hour",
> "weekly" or "daily". Empty when the provider named no window (a bare
> "usage limit reached"), which is NOT the same as an unknown reset: the
> reset says when the limit lifts, the window says which plan cap it is.
>
> It is diagnostic, not decision-making: Stop already carries whether the
> loop may wait, and a weekly or daily window is a rolling cap like the
> 5-hour one, so it is never a reason to stop. The loop prints it beside the
> reason (a multi-day wait should say WHICH cap it is waiting out), and an
> adapter that names no window leaves it blank rather than guessing.

### ProbeResult

> ProbeResult is what a liveness probe learned, and what it cost.
>
> The cost half is the reason this type exists at all. Every adapter implements
> Probe as Invoke-then-DetectLimit, so each poll is a genuine paid session — cheap
> (the prompt is "." with tools denied) but not free, and a cap that lasts a week
> polled every 30 minutes is ~336 of them. Returning only the Limit threw the spend
> away, so the one loop that cannot reach the caller — the supervised wait, which
> polls for as long as the cap lasts — was spending money no ceiling could count
> (CLA-287).

### Tokens

> Tokens and CostUSD are this probe session's spend, for the same Budget
> accumulator an ordinary session's Result feeds.
>
> Carried on the error return too, so the loop counts whatever the adapter
> managed to parse rather than discarding a session that ended untidily. Note
> what that is worth TODAY, which changed with CLA-262: codex and opencode now
> parse as the stream arrives, so a session that emitted its usage and then died
> in Wait carries that spend out through the error return. claude's DRAIN path
> always did (it streams). claude's PROBE path still returns before parsing, so
> it is zero there — the remaining half of CLA-299, and the accumulator here is
> ready for it. A non-zero EXIT is not that path: it parses normally either way.

### ErrMCPNotConnected

> ErrMCPNotConnected marks a session that never reached its first model turn
> because the clankerbar MCP server did not report connected in time. It is an
> INFRASTRUCTURE failure, not a verdict on the phase: the session did no work
> and recorded no review, and the driver must retry it as such rather than read
> an empty result as "the session reviewed nothing" (CLA-703).
>
> It is an error rather than a zero Result on purpose. A zero Result is
> indistinguishable from a session that ran and produced nothing — exactly the
> silent "no record" shape the task exists to kill — while the error carries the
> reason to the driver's log and to whatever retry decision it makes.

### probeVerdict

> probeVerdict turns a finished probe session into what the caller may act on.
>
> A probe exists to answer one question — am I still limited? — and the supervised
> wait resumes spending real sessions on a "no". So an UNTRUSTED probe must not
> answer it: a Limit{} read out of output the adapter could not read whole is
> indistinguishable from a lifted cap, and acting on it resumes the run on a
> reading the drain path would have refused (CLA-262).
>
> It comes back as an error rather than as a third state because the loop already
> waits another interval on one, which is exactly "I still do not know" — but it
> wraps ErrUntrusted, because "I cannot read this harness" does not clear itself
> the way a network blip does, and a wait that polls on it forever is unaccounted
> spend. The spend is on `out` regardless, since a probe that could not be read
> still cost what it cost.


## `internal/harness/idle.go`

### idle.go - the no-output watchdog for a harness session

> idle.go - the no-output watchdog for a harness session.
>
> A session that stops producing stream output is indistinguishable from one
> that is thinking, except by the clock. A silent child is a hang - a wedged
> provider connection, a tool waiting on something that will never answer - and
> nothing else in the driver bounds it: the wall clock is off by default and the
> token ceiling only sees a session that is still SPENDING.
>
> While no tool call is open, the guard kills after `idle` without output. While
> one or more calls are open the idle clock is paused, because a long build or a
> slow MCP call is legitimate work with no output, and instead the oldest open
> call is bounded by `toolCap`. A harness that cannot see a call in flight
> (opencode forwards only settled tool parts) opens a SYNTHETIC call spanning the
> step that may run one (opencode's step_start .. step_finish); see toolHandoff.
>
> Zero idle disables everything, including the tool cap: with no watchdog there
> is nothing to pause or bound.

### idleGuard

> idleGuard fires a kill when a session has been silent too long. seen,
> toolStarted, toolFinished, stop and tripped are all safe on a nil guard and on
> a disabled one, so a parser can hold one unconditionally.
>
> Goroutine-safe: the stream parser feeds it from the read loop while the timers
> fire from their own goroutines.

### toolHandoff

> toolHandoff opens a SYNTHETIC call for a harness that cannot see a real call
> in flight: opencode's step_start, which begins the step whose tools opencode
> forwards only after they settle. The caller closes it with toolFinished at the
> step's step_finish. A repeated handoff for the same id RESTARTS the deadline: a
> step_start always begins a new step, including one whose predecessor ended
> without a step_finish (a provider retry), and an old start must not cap the
> step now running.
>
> It is a no-op when no tool cap is configured: with nothing to bound the call,
> pausing the idle clock would leave a stuck call with no watchdog at all, so the
> idle clock stays armed — the same bound the session had before the handoff
> signal existed.


## `internal/harness/inheritenv.go`

### Filtering what a session inherits (CLA-714)

> Filtering what a session inherits (CLA-714).
>
> Every adapter used to hand exec a copy of os.Environ() unchanged (plus the
> config's `env` overlay). So a config `env` block was ADDITIVE, never a
> whitelist: an unattended, model-driven session could read every variable in
> the shell that started the supervisor, including credentials it has no use
> for. Invocation.EnvAllowlist is the config-declared answer to "what may this
> session see", and inheritEnv is the ONE place the inherited half is filtered.
>
> The config's `env` entries are appended AFTER this filter and are therefore
> always present: they are declarations for the session, not inherited state,
> and that is how GH_TOKEN and CLAUDE_CODE_OAUTH_TOKEN arrive when the ambient
> copy is dropped. A fromCommand that produces one of those values runs in the
> daemon, before this filter, so it may read anything the daemon can.

### inheritEnv

> inheritEnv returns env with only the variable NAMES in allow. A nil or empty
> allow means "inherit everything" and returns env itself, byte-identical to
> the pre-allowlist behaviour — the default no config changes.
>
> The comparison is by exact name: an allowlist naming HOME does not admit
> HOME_DIRECTORY, and it does not admit an entry with no '=' at all (nothing
> can read a name with no value). Values are never inspected, logged or
> copied.


## `internal/harness/opencode.go`

### opencode

> opencode drives the open, provider-agnostic opencode CLI, 2.x line (`opencode
> run`). It is the adapter that proves the loop isn't Claude-shaped: opencode
> points at OpenRouter / opencode Zen / opencode Go / any provider, and the model
> choice lives in opencode's own config (invisible here). The 1.x line is no
> longer supported upstream and this adapter no longer speaks its dialect
> (CLA-675). Divergences it absorbs, verified against opencode 2.0.32 with a
> hermetic fake provider and fake MCP server (docs/opencode-build.md):
>
>   - Headless entry point is `opencode run --standalone --format json [--model
>     m] -- <message>`, one JSON event per line. `--standalone` is load-bearing:
>     2.x is client/server and `run` otherwise attaches to the managed background
>     service, whose own config then wins over everything this adapter sets.
>     Stdin is left nil: a non-TTY stdin left open hangs `run`, and piped stdin
>     is appended to the prompt.
>   - Permissions come from a config `permission` block. 2.x ignores the
>     OPENCODE_PERMISSION env var entirely, and its default with no policy is
>     fail-OPEN (write, shell and webfetch all execute, inside the workdir or
>     not). The adapter therefore writes its fail-closed policy into the
>     OPENCODE_CONFIG_CONTENT layer it pins on every spawn, which 2.x merges
>     last. We never pass --auto, which approves every `ask`.
>   - MCP servers default to Code Mode in 2.x: their tools are not top-level but
>     callable only from inside the `execute` tool, whose stream shows no
>     per-call output (so no claim can be observed) and whose sandbox reached the
>     network with webfetch denied. The adapter sets `codemode: false` on every
>     server it hands over and denies `execute`.
>   - Billing depends on the configured backend, not opencode: metered pay-per-token
>     (OpenRouter / Zen), a monthly-limit subscription (opencode Go), or the Command
>     Code plan's rolling usage windows — 5-hour, daily and weekly. The first two
>     are hard caps; a rolling window names the cap and usually states its reset,
>     so DetectLimit reports it as a WAITABLE limit (Limit.Stop unset, ResetAt
>     parsed when stated, Limit.Window labelled) and the loop supervises the wait.
>     The window is read from the provider's structured `rateLimit.window` in the
>     response body opencode passes through verbatim, falling back to the message
>     wording. The hard cases (402 / "out of credits" / "monthly limit reached")
>     still set Limit.Stop so the loop stops cleanly rather than waiting for a
>     reset that never comes. The self-accounted budget breaker is the primary
>     control; ReadUsage is unsupported.
>   - Usage: 2.x emits step_finish only for tool-call steps, never for the final
>     answer, so the stream under-counts. After each process exits the adapter
>     reads `opencode session export --standalone <sid>` for the authoritative
>     figures (see opencode_export.go).

### MCPConfigUse

> opencode DOES receive mcp_config_path - as OPENCODE_CONFIG and, rewritten, as
> OPENCODE_CONFIG_CONTENT (see env below) - and that is exactly why "does the
> adapter hand it over" was the wrong question to gate a preflight on. What
> arrives has to be an opencode config, whose servers live under `mcp` (1.x
> dialect, still accepted) or `mcp.servers` (2.x dialect). A Claude-shaped
> file is a different schema: 1.18.2 refused to start on it, and 1.18.18 and
> 2.0.32 silently skip its `mcpServers` key, so the session gets no clankerbar
> tools. config.ResolveMCPConfig never hands opencode a .mcp.json; a
> Claude-shaped file under another name is a doctor FAIL (CLA-263).

### Invoke

> Invoke runs one opencode session for the driver, and — CLA-406 — when that
> session dies the quiet death (stream dropped mid-generation, no terminal
> chunk, no usage), tries to RESURRECT it in place rather than hand the driver
> a corpse: back off, probe the session with a coherence question, and on a
> mechanical match continue it where it left off. See opencode_resume.go for
> the rules and their rationale; the loop here is only their orchestration.
>
> Every round re-checks ZeroUsageUnknown on the MERGED result, so a
> continuation that itself dies quietly takes another round (a fresh probe per
> death), while a clean continuation clears the marker and exits the loop. The
> cap bounds the whole loop; after it, the last Result falls through to the
> driver's existing dead-phase path unchanged.

### The claim observation folds ONLY when it is still about THIS task

> The claim observation folds ONLY when it is still about THIS task
> and arrived on a capture we may read: noteClaimed replaces a parser's
> claim WHOLESALE, so a decohered probe that re-claimed a different
> task would otherwise redirect the merged Result's identity — the
> real lease expiring while release/salvage/tally all fire against a
> task the dead session never held. An untrusted probe capture is
> skipped for the CLA-262 reason the loop conjunct below spells out.
> A probe that kept its seed (did nothing, or settled/advanced ITS OWN
> task) always passes both gates — that is finding 6's fix standing.

### An UNTRUSTED probe capture proves nothing

> An UNTRUSTED probe capture proves nothing: its text survived an
> over-cap event discard (output.go), so a ref found in FinalMessage
> may be debris rather than recall. Counting it coherent would send
> Continue into a session whose state was just misread - the same
> CLA-262 reasoning that gates the fold above and the loop conjunct.
> An UNTRUSTED probe capture proves nothing: its text survived an
> over-cap event discard (output.go), so a ref found in FinalMessage
> may be debris rather than recall. Today the discard already empties
> the parse, so this conjunct and probeCoherent agree by accident -
> the conjunct makes "untrusted reply is not evidence" hold BY RULE,
> so a future parser that survives a bad line cannot quietly start
> continuations on debris. Same CLA-262 reasoning as the fold above.

### opencodeArgs

> opencodeArgs maps an Invocation to `opencode run`'s CLI dialect. Split out so the
> probe's read-only shape is unit-testable without executing anything.
>
> The prompt goes LAST, behind a `--` terminator, for the reason spelled out in
> codexArgs: it is a bare positional, and a prompt that is itself a flag token
> would otherwise be parsed as a flag rather than as the message.

### opencodePrompt

> opencodePrompt is the message a working (non-probe) run sends. It is the
> caller's prompt unchanged.
>
> It USED to prepend an MCP warm-up note ("MCP servers connect in the
> background; call the tool again if it is missing"). CLA-703 removed the race
> that note described: the adapter now starts its own server, waits until the
> clankerbar server reports connected, and only then attaches the run, so the
> first model turn already has the tools. Keeping the note would tell every
> session to expect a failure that no longer happens, and would spend prompt
> bytes on a workaround the code no longer needs.
>
> The function is kept (rather than inlined at its two call sites) because the
> prompt shape is a contract this adapter owns, and the tests assert the
> absence of the note through it.

### opencodePermission

> opencodePermission is the fail-closed policy the adapter writes into the
> `permission` key of the config it pins via OPENCODE_CONFIG_CONTENT (see
> opencodeSessionConfig): a path-scoped PermissionConfig, not a flat tool map. The run's workdir subtree is the working
> set — `read`/`edit` are allowed there and `external_directory` (opencode's gate
> for any path-taking tool whose target is outside the project directory) allows
> the same subtree, so the session's own worktrees, which live under the workdir
> and are separate git roots, work normally. Everything else fails closed via the
> `*` catch-all, and the network/exfil tools (webfetch/websearch) are denied in
> both shapes. A read-only run (probe, or the connectivity smoke) additionally
> denies edits and shell — zero writes, just enough to reach the clankerbar MCP.
>
> EXTRA DIRECTORIES (CLA-437, agent-rule-scoping piece 2): each entry in
> extraDirs — the project's other declared repos, plus any conventional worktree
> area beside the spawn checkout — is granted the same reach the workdir has:
> an absolute external_directory allow, and a read/edit allow in the
> instance-relative "../<dir>/**" form, which sorts after the blanket "../**"
> deny and so wins for exactly the declared trees. See scopeExtraDirs.
>
> `shell` (1.x called it `bash`; 2.x accepts both names) stays TOOL-LEVEL: its
> permission patterns match parsed COMMANDS ("git status --porcelain"), not
> paths, so a path rule can never express it. On 1.x, commands whose file
> arguments fell outside the project boundary were gated separately via
> external_directory. 2.x DROPPED THAT: verified against 2.0.32, under this policy
> `touch /abs/outside` and `cat /etc/hosts` both ran, and only the shell tool's
> own `workdir` argument reaches external_directory. The operator accepted that
> gap on 2026-10-18 (CLA-675): an unattended session can already run arbitrary
> commands inside the workdir, so no sandbox and no absolute-path shell deny
> patterns were added (docs/opencode-build.md, "Shell is not path-gated").
>
> `execute` is denied by name. It is 2.x's Code Mode tool: MCP tools left in Code
> Mode are callable only through it, its stream carries no per-call output (no
> claim could be observed), and code run inside it reached the network with
> webfetch denied. The catch-all already denies it; naming it keeps that from
> depending on the catch-all. Every MCP server the adapter hands over is moved
> out of Code Mode instead (opencodeSessionConfig), so `*_*` below is what
> reaches them.
>
> Five quirks the emitted shape is fitted to. They were read out of opencode
> 1.18.16 source (permission/index.ts, session/tools.ts, tool/read.ts,
> tool/edit.ts, tool/external-directory.ts). Against 2.0.32 the first three were
> re-checked behaviourally (last-match-wins, `<server>_<tool>` MCP names,
> external_directory for out-of-tree file tools); the MCP-resource one no
> longer applies the same way, because 2.x registers the resource tools in Code
> Mode only, so with `execute` denied a session has no resource tools at all:
>
>   - Rules are evaluated LAST-MATCH-WINS on a flattened ruleset, so the specific
>     rules must sort AFTER the `*` catch-all or the catch-all overrides them.
>     Go marshals map keys sorted, and "*" sorts before every letter, so the
>     order holds by construction; opencode_test.go pins it.
>   - `*` also matches every MCP tool ask — the MCP adapter asks with the full
>     tool name — so the catch-all would deny the session the plane itself. MCP
>     tool names are `<sanitized-server>_<sanitized-tool>` (opencode 1.18.x,
>     McpCatalog.toolName: "clankerbar_get_backlog_summary", "context7_query-docs",
>     "chrome-devtools_click"), so the policy allows `*_*`. That rule also matches
>     underscore-named built-ins (external_directory, list_mcp_resources,
>     apply_patch, doom_loop, plan_enter/exit). The one that matters is
>     external_directory: its own entry carries an inner `*` deny, which sorts
>     after the `*_*` allow in the flattened ruleset, so an out-of-workdir ask
>     still resolves to deny (the old flat `external_directory: deny`, restored);
>     only an in-workdir ask reaches the allow. The other collisions are benign:
>     apply_patch asks under "edit" (so the probe's edit:deny covers it), and
>     doom_loop / plan_enter / plan_exit are loop/UI guards with no file side
>     effects.
>   - MCP RESOURCE reads do NOT ask under their own tool name, so `*_*` never
>     reaches them: list_mcp_resources, list_mcp_resource_templates and
>     read_mcp_resource all ask under permission **`read`**, with `mcp:`-prefixed
>     patterns — `mcp:<server>:<uri>` for a read, `mcp:<server>:*` for a listing
>     (opencode 1.18.x, session/tools.ts). Against a `read` entry scoped to
>     workdir paths alone, no rule matched them and the `*` catch-all denied every
>     one — which is why a session could call the plane's TOOLS but could not read
>     the served skill that carries the heartbeat cadence (CLA-382). The `read`
>     entry therefore also allows `mcp:*`. That is not a loosening of the
>     fail-closed posture: it grants exactly what `*_*` already grants for the
>     same servers — read-only data from an MCP server the operator configured,
>     which is strictly narrower than the arbitrary TOOL calls `*_*` allows those
>     same servers — and it cannot widen filesystem reach. Not because a path
>     could never look like one of these patterns (a file named `mcp:x` directly
>     under the worktree root would ask with the pattern "mcp:x"), but because the
>     read tool runs its external_directory gate BEFORE the read ask: a path
>     outside the workdir is refused before `mcp:*` is ever consulted, and a
>     relative path that did start with "mcp:" is inside the working set already.
>   - grep/glob (and list, in newer opencode) are read-only SEARCH tools, and
>     their asks are NOT path-scoped the way read/edit are. grep and glob are
>     verified against opencode 1.18.18 source (tool/grep.ts, tool/glob.ts):
>     grep asks with the REGEX itself (`patterns: [pattern]`), glob asks with
>     the GLOB itself. `list` is NOT a builtin in 1.18.18 - the run path's
>     registry carries no list tool, so there is no tool/list.ts to verify
>     against - its ask shape (the resolved absolute directory) is inferred by
>     analogy with grep/glob/read, not source-confirmed. A path rule could
>     never match any of these asks, so for these tools the workdir boundary
>     cannot live in the ask; it lives in `external_directory`, which every one
>     of them is claimed to run BEFORE searching - containsPath returns
>     without asking for an in-workdir target, and an out-of-workdir target
>     asks external_directory, denied here. That claim about grep.ts/glob.ts's
>     internal call order is verified against source and is NOT re-checked by
>     any test in this repo (a future opencode release changing it would not
>     go red here); confirm it holds after an opencode upgrade by checking
>     `~/.local/share/opencode/log/opencode.log` for an `external_directory`
>     deny on an out-of-workdir grep/glob/list call in a live session (see the
>     README's "Verifying the policy against a live session"). Allowing the
>     pattern `**` therefore grants no reach `read` does not already have: the
>     ask governs the search pattern, the reach is still the workdir subtree
>     (CLA-390). `list` is allowed for the same read-only reason so a future
>     opencode upgrade does not re-break directory listing the way grep/glob
>     broke.
>   - read/edit asks carry the path RELATIVE TO THE INSTANCE DIRECTORY on 2.x
>     (verified against 2.0.32: in a git checkout's subdirectory a write of
>     "w1.txt" asked "w1.txt", not "sub/w1.txt", and outside any repo it asked
>     "w1.txt", not the root-relative form 1.x used). An in-tree ask is a plain
>     relative path, so "**" allows the working set, and an escape is
>     "../"-prefixed, so "../**" denies it. The deny sorts first ("*" 0x2a
>     before "." 0x2e), so a declared extra dir's own "../<dir>/**" allow still
>     wins on last-match-wins. external_directory patterns are absolute globs,
>     and external_directory is judged against the instance directory too (a
>     read of "../top.txt" from a checkout's subdirectory was refused there, git
>     worktree or not), so that rule uses the absolute workdir form.
>
> 1.x made those asks relative to the git WORKTREE (or to "/" outside a repo),
> which is why this policy used to carry a root-relative layer and an
> in-checkout layer chosen by walking up for a .git entry (CLA-441). On 2.x the
> instance directory alone decides the shape, so there is one layer, and the
> PWD pin (pinPWD) is what makes the instance directory Invocation.WorkDir -
> which the driver sets to the task's repo checkout (loop sessionDir), not the
> project workdir.

### The working set

> The working set: read/edit scoped to the workdir subtree (root
> included), plus the external_directory carve-out for the same
> subtree. The carve-out carries an inner `*` deny so an ask for a
> path OUTSIDE the subtree still resolves to deny — the old flat
> `external_directory: deny`, restored — while an in-subtree ask wins
> on the later absolute allow.
> `mcp:*` rides on "read" because that is the permission the MCP
> resource tools ask under — see the function doc. A read-only run
> keeps it: reaching the served protocol IS the point of the probe.

### The read-only search tools

> The read-only search tools: grep/glob/list. Their asks carry the
> search pattern (a regex, a glob, or the resolved directory), never a
> workdir-relative path, so a path-scoped rule could not match them and
> the catch-all would leave the tools denied — the CLA-390 bug. The
> pattern `**` is a flat allow of the ASK; the filesystem boundary
> stays in `external_directory`, which each tool runs before searching
> and which is scoped to the workdir subtree above. See the function
> doc's grep/glob/list bullet for the verification.

### scopeExtraDirs

> scopeExtraDirs widens the policy's three path-scoped rules to cover extraDirs:
> the absolute external_directory allow, and the instance-relative read/edit
> form. It mutates the maps in place so the base construction stays the single
> description of the working set.
>
> A tree INSIDE the instance directory needs no read/edit rule ("**" covers it).
> A tree that CONTAINS the instance directory is skipped, and that skip is
> load-bearing rather than tidy. filepath.Rel returns a pure dot-dot chain for
> an ancestor ("..", "../.."), and opencode's matcher reads `*` as any run of
> characters including "/", so "../**" is not "that directory" - it is
> everything outside the instance directory. Emitting it would either overwrite
> the "../**" deny with an allow or sort after it and beat it ("../../**"),
> turning the policy's only stated escape boundary into an allow of the whole
> filesystem. There is no bounded glob to give it ("../*" spans "/" exactly as
> "../**" does), so the choice is that or an escape, and fail-closed wins
> (CLA-441 reviews). The cost is a WITHDRAWN grant: a declared tree containing
> the workdir is unreadable outside the workdir itself.

### and hand the session config over again as OPENCODECONFIGCONTENT,

> ...and hand the session config over again as OPENCODE_CONFIG_CONTENT,
> which opencode merges after every layer this driver or a checkout can
> write (CLA-441; re-verified on 2.0.32, where the content layer's
> `permission` beat a project-level opencode.json). OPENCODE_CONFIG is an
> early layer: the global dir, then that file, then every project-level
> opencode.json and .opencode/opencode.json discovered from the instance
> directory, THEN this. Later layers have been observed redirecting
> `mcp.clankerbar` at ANOTHER project, so naming the file does not pin which
> backlog an unattended session works. The content layer does.
>
> On 2.x it is also the ONLY place the permission policy can live: the
> OPENCODE_PERMISSION variable 1.x read is ignored (the string is absent from
> the 2.0.32 binary), and with no policy 2.x runs every tool. So the content
> is ALWAYS set, with or without an mcp config - see opencodeSessionConfig.
>
> This does not make the PWD pin redundant, nor it this: the content layer
> settles which MCP server the session talks to and what it may do, the
> instance directory settles which repo it reads, edits and asks permission
> for.
>
> Two costs of moving a file's bytes into the environment, neither large
> enough to change the call: a credential written LITERALLY into the mcp
> config (rather than as `{env:CLANKERBAR_API_KEY}`) is now in the child's
> environment, where a process listing or a crash dump reaches it and a 0600
> file did not; and environment plus argv is capped around 1 MB on macOS, so
> an enormous config would fail the spawn with E2BIG rather than merely being
> ignored.

### opencodeSessionConfig

> opencodeSessionConfig builds the document pinned as OPENCODE_CONFIG_CONTENT:
> the mcp config file's own keys, with every MCP server moved out of Code Mode
> and the fail-closed permission policy under `permission`.
>
> The file is parsed as strict JSON, the same rule config applies to every file
> the driver names (readMCPFile): those feed fail-closed gates, and a lenient
> parse here would let the adapter accept what config refused. Numbers are kept
> as written (UseNumber) so a re-marshalled timeout or port is byte-for-byte the
> operator's value.
>
> The file's own `permission` key, if any, is replaced here rather than merged.
> That does not neutralise it: the same file is also loaded as OPENCODE_CONFIG,
> an earlier layer, and opencode merges permission blocks key by key in
> insertion order, so a key the file declares sorts ahead of this policy's `*`
> catch-all. doctor WARNs on a named file carrying one (and config refuses one
> in a DISCOVERED file, checkDiscoveredMCPConfig).

### Pin the same policy on every built-in agent

> Pin the same policy on every built-in agent. Verified against 2.0.32: an
> agent-level permission block in ANY layer (1.x `agent.<name>.permission`
> or 2.x `agents.<name>.permissions`) overrides the top-level one for that
> agent. With `{"agent":{"build":{"permission":{"external_directory":
> "allow",...}}}}` in a project opencode.json - a file a session can write
> into its own checkout for the next spawn - out-of-tree reads and writes
> succeeded and webfetch was offered. With this pin in the content layer
> they were refused again, in both dialects, and in-tree writes still
> landed. The file's own `agent` key is replaced, not merged.

### opencodeMCPOutOfCodeMode

> opencodeMCPOutOfCodeMode rewrites a config's `mcp` block in place so every
> server it declares is in the 2.x dialect (`mcp.servers.<name>`) with
> `codemode: false`.
>
> Why both halves are needed, verified against 2.0.32:
>
>   - With Code Mode on (the 2.x default) a server's tools are reachable only
>     from inside the `execute` tool, which this adapter denies, and whose
>     stream would not show the claim anyway. `codemode: false` puts them back
>     at the top level as `<server>_<tool>`, with input and output on the
>     tool_use event as on 1.x.
>   - `codemode` is only honoured in the 2.x dialect. A 1.x-dialect entry
>     (`mcp.<name>` with a `type`) is still accepted by 2.x, but a `codemode`
>     key on it is ignored and the tools stay in Code Mode. So 1.x entries are
>     moved under `servers`, translating the one key whose spelling changed
>     (1.x `enabled: false` is 2.x `disabled: true`).
>
> A server declared in both dialects under one name is refused rather than
> guessed at. A key under `mcp` that is not a server entry is left alone for
> opencode to judge.

### opencodeTruncatedOutput

> opencodeTruncatedOutput returns the full text of a tool result 2.x truncated
> (see opencodeToolState), ok=false when the result was not truncated.
>
> The path comes from the event's own metadata, which opencode writes, not
> from the output text a tool or the backlog controls. It is still checked
> before anything is read, because a path that is merely read and parsed as a
> claim is a way to hand the driver a claim on a task nobody asked for (the
> threat injection_test.go covers):
>
>   - it must be a file directly inside dir, the session's own
>     <data home>/opencode/tool-output, compared after resolving symlinks;
>   - its name must start with "tool_", opencode's naming;
>   - it must be a regular file, not a symlink;
>   - the read is bounded by readSpilled, the same cap as Claude's spill.
>
> Any failure is returned as an error; the caller fails safe.

### opencodeClankerbarTool

> opencodeClankerbarTool maps an opencode tool name onto the namespaced name the
> shared observer switches on, and returns "" for anything that is not a
> clankerbar tool.
>
> opencode names an MCP tool `<server>_<tool>`, where the server is the key under
> `mcp` in its config; claude names the same tool `mcp__<server>__<tool>`.
> Translating rather than keeping a second set of constants leaves ONE list of
> watched tools: add a tool there and both harnesses see it.
>
> The server name `clankerbar` is HARD-CODED, here as in claude's constants, and
> that is an assumption the driver does not enforce: it never writes an MCP config
> (the operator hand-writes opencode's, per the README), and config.Validate
> deliberately accepts a clankerbar server under any key so long as it is handed
> CLANKERBAR_API_KEY. So `mcp: {"cb": …}` validates, opencode names the tools
> `cb_claim_task`, nothing here matches, and a phased run stops after phase 1 on
> every task looking like an ordinary early finish — the exact silent half-run the
> TracksClaims gate exists to prevent. Filed rather than fixed inside this
> adapter, because the check belongs where the config is read (doctor, or a
> Validate refusal under phases), not where the tool name is parsed.

### The CLA-398 quiet-death signature

> The CLA-398 quiet-death signature: a FINAL "unknown" step whose OWN
> usage was all-zero. The reason alone is the CLA-386 dead-phase marker,
> but a session can end "unknown" after doing real paid work; the final
> step reporting nothing is what makes this the shape that dies with NO
> error event, NO stderr, and nothing produced — indistinguishable from a
> cheap clean run until the adapter names it. The marker is the adapter's
> own (see ZeroUsageUnknown), so the driver can log the end by name.
>
> The step's OWN usage, never the session total: the 2026-08-20 dead
> sessions had done hours of paid work across earlier steps before the
> final "unknown" step that reported nothing.

### CLA-584

> CLA-584: the output-cap stall — a FINAL step that hit the output token
> limit with ZERO output. Distinct from the quiet death above in the two
> ways that matter: the step reported real usage (reasoning and cost),
> and opencode ended the turn itself rather than the stream being
> dropped, so the session is alive and resumable. The reasoning count
> rides along because it is what makes the stall legible — thousands of
> tokens of thinking, nothing emitted — and it is the step's OWN count,
> like every other final-step figure here.

### opencodeBudgetRe

> opencodeBudgetRe recognizes a HARD budget/credit-exhaustion stop from the
> configured provider: out of credits, spend-cap or monthly/billing-limit
> reached, or an HTTP 402 Payment Required. This is a STOP (Limit.Stop) — there
> is no window to wait out.
>
> It carries only phrasings that NAME a hard condition. The generic wordings
> that name an exhausted quota without saying which window it is — "usage limit
> reached", "insufficient quota" — live in opencodeGenericLimitRe, because the
> SAME words arrive on a rolling subscription window that a wait can outlast: a
> provider.quota event whose text is "Usage limit reached ... resets at ..." was
> a Stop here and threw the stated reset away (CLA-736 review). A rolling
> subscription quota (an N-hour/N-minute/session/daily usage limit, or
> error.type "provider.quota") is the separate, WAITABLE case, matched by
> opencodeRollingQuotaRe; a plain 429 "rate limit" with no quota wording is a
> transient blip (IsTransient), not this.

### opencodeRollingQuotaRe

> opencodeRollingQuotaRe recognizes the OTHER kind of provider limit: a rolling
> subscription window that a wait can outlast (CLA-736). The Command Code plan's
> 5-hour quota arrives as an HTTP 429 typed `provider.quota` and states the
> reset instant in its message; the loop supervises that wait and re-runs the
> session once the window lifts, so this must be reported as Limited with Stop
> UNSET rather than falling through to the transient ladder.
>
> Three arms, all read from opencodeErrorText (stderr + a TERMINAL error event),
> never the agent's narration:
>   - 2.x types it: error.type == "provider.quota". This alone is enough — the
>     type is the provider's own statement that the cap is a quota it will lift,
>     so a message carrying no duration/window wording at all is still waitable.
>   - the message names the window BEFORE the limit word: "5-hour usage limit",
>     "Your 5-hour limit has been reached", "30-minute usage limit", "session
>     usage limit", "daily usage limit", "weekly usage limit". The word "usage"
>     is not required — an untyped provider that calls it a "5-hour limit" is
>     the same window. The weekly arm is not optional: it is the window this
>     task adds, and a prose weekly cap must classify exactly like its sibling
>     windows instead of reading as no limit at all (review 1.review-1.1).
>   - the message names the window AFTER the limit word: "Your usage limit has
>     been reached (5-hour window)". Same statement with the clause order
>     swapped; without this arm it matches neither the window-first arm (the
>     limit must follow the window) nor the generic quota arm ("usage limit
>     reached" wants those words adjacent), so a waitable window rode the
>     transient ladder instead of a supervised wait (review 1.review-2.2).
>
> Deliberately narrow. A bare 429 "rate limit" names no window and stays
> transient, and the hard budget arms above are checked FIRST — so a monthly cap
> typed as provider.quota is still a Stop.

### opencodeRateLimitWindows

> opencodeRateLimitWindows maps the provider's structured `rateLimit.window`
> token onto the label the loop reports. The tokens are Command Code's own —
> `fiveHour`, `weekly`, `daily` — not opencode's. Keys are normalised
> (lower-case, separators stripped) because the provider's spelling is not
> stable: `FiveHour` and `five-hour` are the documented token too, and an
> exact-match map made those variants invisible, which is how a plan cap rode
> the transient ladder (review 1.review-1.4). The mapping stays deliberately
> closed: an unrecognised token names no window this adapter understands, and
> guessing one is how a hard stop becomes an unbounded wait.

### opencodeWindowLimit

> opencodeWindowLimit resolves the plan limit a failure states: the LABEL of
> the usage window it names (empty when it names none this adapter knows), and
> whether the failure is a plan limit at all. Two sources, in order:
>
>  1. The STRUCTURED `rateLimit.window` from the provider's response body —
>     top level or nested under the body's `error` object. Its token is
>     authoritative: when it names a window the closed map does not know, the
>     window is UNKNOWN — the wording is not consulted over it, because a
>     window the provider named is not something prose may relabel, and the
>     plan's default is a guess the map's own contract forbids (reviews
>     1.review-1.4, 1.review-2.2).
>  2. The message's "usage limit for your plan" phrase, which is the same
>     statement in prose: weekly when it says weekly, otherwise the plan's
>     default 5-hour window.
>
> Only a rate limit can be a PLAN limit: the structured path requires a 429
> status or the RATE_LIMITED code, and the wording path requires the same. A
> window seen on a 5xx is not a plan cap, and a 429 that names no window stays
> the transient burst limit IsTransient already handles — that is the difference
> between waiting out a plan cap and backing off a burst.

### opencodeNamedWindowRe

> opencodeNamedWindow labels a window the text NAMES, when it names one. It is
> the wording arm's label, used where a rolling window is recognised by its
> shape rather than by the structured token: an untyped "daily usage limit" is
> the daily window. A bare "30-minute usage limit" names a duration, not one of
> the plan's windows, and stays unlabelled rather than being called 5-hour.
>
> The window word only counts inside the limit STATEMENT — the same clause as a
> limit/quota word (either side of it, since the provider swaps the order). The
> scan reads the whole diagnostic text, which includes the terminal error event
> and the provider's raw body, so a stray "weekly" in a session id or a body
> field must not relabel the cap (review 1.review-1.6).

### opencodeErrorFactsOf

> opencodeErrorFactsOf reads the structured facts out of a finished Result's
> terminal error event.
>
> What survives opencode's wrapping is verified against the 2.0.32 binary and
> the 2026-10-20 capture in testdata/opencode-quota-five-hour.jsonl: the
> provider's message and status are lifted onto `error`, and its raw response
> body is passed through as a JSON STRING under `error.response.body` (the
> binary's provider error is built with `response:{body:…}`). The older
> APIError shape puts the same body under `error.data.responseBody`. Neither is
> re-parsed by opencode, so `code` and `rateLimit` are only reachable by
> decoding that string here.
>
> Wrapper first, body second: the transport's own `error.status` is the status
> the request actually failed with, while a provider body may carry an envelope
> `status: 200` (or any other non-HTTP value) that says nothing about the
> failure. Filling the status from the body first masked a wrapper 429 and sent
> a plan cap down the transient ladder (review 1.review-1.5). Code and window
> live only in the body, so the wrapper merge leaves those holes for it.

### buildOpencodeErrorText

> buildOpencodeErrorText appends the stream's error event ONLY when it is how
> the session ENDED — the last typed event on stdout (CLA-700).
>
> 2.x emits a provider error mid-session, retries the request itself and carries
> on: a stream whose error event is followed by a step_start / tool_use / text
> part is a session that RECOVERED, and calling that session failed or
> retryable is how a delivered phase got classified as a failed attempt (both
> 2026-10-19 incidents). So an error event with any typed event after it is
> recovery and contributes nothing here; only a stream that stops on the error
> is a failure. The conformance suite's recovered-provider-error case pins the
> recovery against the real binary (2.0.x error event; 2.2.x retry event).
>
> stderr is scanned unconditionally: it is the harness's own diagnostic channel,
> written as the process winds down, and the fallback for the untyped failures
> the typed-event scan cannot see.
>
> Note this is deliberately stricter than opencodeErrorType (the Raw
> diagnostic key), which records the LAST error event whatever followed it —
> that key is testimony about the stream, not a classification.

### opencodeTerminalErrorLine

> opencodeTerminalErrorLine returns the LAST typed event on the stream when it
> is a {"type":"error"} event, verbatim; "" otherwise.
>
> 2.x emits provider errors mid-session, retries the request itself and carries
> on: a stream whose error event is followed by a step_start / tool_use / text
> part is a session that RECOVERED, and calling that session failed or retryable
> is how a delivered phase got classified as a failed attempt (both 2026-10-19
> incidents). So an error event with any typed event after it contributes
> nothing.

### opencodeFatalWords

> opencodeFatalRe: failures that are NEVER a blip, scanned before the transient
> patterns and before the heuristic below so neither can overrule them.
>
> It exists because the transient scan reads all of stderr, which carries text
> this adapter did not author — third-party stack traces, a provider SDK's own
> error prose — and one word from that text used to be enough to call a stop a
> blip. A revoked key or a dead MCP server must end the run loudly at the first
> attempt: those are the failures an operator has to act on, and a daemon that
> retries them at the backoff ceiling instead reports "transient failure (exit
> 1) — retry 47" all night and never says what is actually wrong.
>
> Deliberately narrow: an auth/permission refusal or a hard 401/403. Anything
> vaguer belongs in neither list, where the heuristic can weigh it under a
> bound.
>
> ANCHORED for the same reason the transient arms are, and it is the reason the
> words below are never matched on their own. This scan runs FIRST, so a match
> vetoes even a recognised blip — which means a bare word here fails in the
> opposite direction to the `transport` bug: a WARN line mentioning "403
> Forbidden" beside a real "Error: 429 Too Many Requests" would stop the daemon
> for the night on a rate limit it should have ridden out. So a refusal counts
> only where the failure is actually being REPORTED: in a typed error event's
> status or message field, or after an `error:` prefix on the same line.
>
> The `authentication( \w+)?` shape is not decoration: gRPC's phrasing is
> "transport: authentication handshake failed", which is both the word that used
> to trip the transient scan and a failure that must stop.

### opencodeTransientRe

> opencodeTransientRe: retryable server/network blips and API-level rate limits.
> Honours opencode's own "isRetryable":true signal, plus HTTP 408/429/5xx and the
> usual connection strings. The transport and stream arms are the CLA-381 shape:
> a stream drop arrives as a bare error event like {"type":"error",
> "error":{"type":"unknown","message":"Transport"}} — a provider-side failure, not
> a config/auth refusal, and retrying it under the existing backoff is exactly what
> riding the blip out means.
>
> The Console Go tool-schema rejection family (`unsupported_tool_schema` /
> `tool_count_limit`, anomalyco/opencode#43374 and #43378) is classified HERE,
> deliberately, rather than as a dedicated class with its own retry ladder: the
> gateway's enforcement is INCONSISTENT — #43374 records the same catalog
> accepted and refused seconds apart — so the identical retry often succeeds,
> which is exactly what the transient path does. The two codes are opaque
> gateway error names, not words a Node stack trace or agent narration would
> emit, so unlike the `transport` arm they need no word-pair anchor, and the
> assistant-text scoping below keeps a task body that merely QUOTES them out of
> the scan entirely. A persistent run of rejections is bounded separately: each
> rejected session reports no usage (the 400 fires at request time), so the
> driver's zero-spend bound stops the run after a few attempts rather than
> retrying forever.
>
> Every arm here is ANCHORED — on a JSON field, or on a word pair that only a
> failure description produces. A bare `transport` alternation was tried and
> reverted: scoping the scan to opencodeErrorText keeps the agent's narration
> out, but stderr still carries library text the adapter did not write, and the
> MCP SDK names its classes StdioClientTransport / SSEClientTransport /
> StreamableHTTPClientTransport — so any Node stack trace from a misconfigured
> MCP server (a config failure, and one this harness can now produce since
> CLA-382 wired opencode to one) matched, and a stop became an unbounded retry.
> The claude adapter anchors its provider arms on `api error:` for the same
> reason.
>
> 2.x's `provider.rate-limit` is the typed form of the 429 arm. 2.0.32 retries a
> 429 itself (about ten attempts over ~82s) before it exits 1 with that type,
> so by the time the adapter sees it the burst has lasted a minute and a half;
> it is still a rate limit and still rides the loop's backoff, and a budget stop
> or a rolling quota window worded inside it is caught first by opencodeBudgetRe
> / opencodeRollingQuotaRe.

### ZeroUsageUnknown

> ZeroUsageUnknown reports whether this session ended with the CLA-398
> quiet-death signature: a FINAL step_finish carrying reason "unknown" whose
> OWN usage was all-zero — whatever the earlier steps cost (the 2026-08-20 dead
> sessions had done real paid work before the silent final step). The marker is
> the adapter's own (a terminal_reason it writes, never text the CLI or an
> agent could emit), and it exists so the driver can NAME the end instead of
> logging "iteration done (tokens=0 cost=$0.00)" over a session that produced
> nothing.

### OutputCapNoOutput

> OutputCapNoOutput reports whether this session ended on the CLA-584 output-cap
> stall: a FINAL step_finish carrying reason "length" whose OWN output tokens
> were zero — the model spent the step's whole output budget thinking and
> emitted nothing. Read back from the adapter's own terminal_reason marker, so
> unlike the CLA-398 quiet death this is not a question about usage being
> absent: the MAK-123 stall reported reasoning=32000 and real cost, and only
> the output figure was zero.
>
> It is the resume gate in Invoke (one resume with a steer, see
> opencode_stall.go) and the driver's marker for naming the end instead of its
> generic "never moved the task on" line.

### Capabilities

> TracksClaims is true as of CLA-365, and on 2.x only because the adapter takes
> every MCP server out of Code Mode (opencodeMCPOutOfCodeMode): opencode's
> `--format json` stream then carries
> each tool call's arguments AND its result on one terminal `tool_use` event, so
> the adapter reads the claim off the session the same way claude does and
> populates Result.Claim. That is what config.Validate gates `phases` on, so this
> flag is what lets a mixed-harness queue run implement on opencode.
>
> HonoursMaxTurns stays false — Invocation.MaxTurns still never reaches the CLI.
>
> ReportsCost is true: on 2.x the authoritative figure is the session export
> read after each process exits (opencode_export.go), with the step_finish
> `cost` siblings summed from the stream as the fallback when the export fails,
> so `budget.max_cost_usd` is a live ceiling here (CLA-288). The cost is only
> non-zero when the model's provider config declares a `cost` block; a model
> without one reports tokens and $0.
>
> HonoursSessionWallClock is true and HonoursMaxTurns is false, which is the
> whole point of the pair: this is the adapter whose CLI takes no turn flag, so
> its phase backstop is the elapsed-time cap Invoke enforces (CLA-368).
>
> RunsTurnSequences is true: `run -s <session id>` continues a live session, so
> Invocation.Turns is honoured (CLA-867).

### A named stop wins over every retry path below, including the heuristic

> A named stop wins over every retry path below, including the heuristic. The
> loop already runs DetectLimit before IsTransient, so the budget arm is
> belt-and-braces there — but this function must not answer "retryable" about
> an exhausted account just because it happens to be asked out of that order,
> and a rolling quota is the same shape: it is WAITED, not retried. The
> generic quota phrasings belong here even though DetectLimit may make them
> waitable on a stated reset: either way there is nothing to RETRY, and a 429
> saying "usage limit reached" must never ride the transient ladder. The
> braces are why the loop's ordering has no test of its own: with both
> classifiers agreeing, no order produces a retry, so there is nothing left
> for a test to distinguish (CLA-381).

### CLA-752

> CLA-752: the same veto for a plan cap stated STRUCTURALLY. A rate-limited
> failure whose body names a usage window — or that says "usage limit for
> your plan" — is a limit the loop WAITS out; DetectLimit says so from the
> same function, and the two classifiers agreeing is what makes the loop's
> DetectLimit-first ordering safe (CLA-381, review 1.review-1.2). Without
> this, the body-only shape was "limited" to one classifier and "retryable"
> to the other, so a caller that asked this one alone retried a plan cap as
> if it were a burst.

### CLA-700

> CLA-700: a session that RECOVERED from its provider errors is not retryable,
> however its exit status reads. 2.x emits the error, retries the request
> itself and finishes the work, then exits 1 anyway (pinned by the conformance
> suite), so there is no failure here for a retry to ride out — the question
> this exit raises is whether the session DELIVERED, and that is the driver's
> to answer, not a classifier's. Both the pattern scan below and the
> usage-gated heuristic are skipped: the heuristic exists to catch failures no
> pattern names, and this one is named (and it is not a failure).

### IsUnclassifiedTransient

> IsUnclassifiedTransient is the CLA-381 heuristic, and it is separate from the
> pattern scan because a caller has to be able to bound it — see
> harness.Adapter.IsUnclassifiedTransient for why that is a spending requirement and not a
> tidiness one.
>
> The judgement: an exit 1 whose cause no pattern has seen yet, on a session that
> REPORTED usage, leans retryable. A usage event means the session authenticated
> and started doing paid work, so the failure came after the startup checks and
> is more likely a version-shaped stream drop this adapter has no pattern for
> than a config/auth refusal — that kind fails BEFORE any usage arrives, which is
> why the no-usage case stays a stop. The opencode error surface moves between
> versions (the CLA-381 incident was a nightly emitting an untyped event), so the
> heuristic is what covers the shapes the patterns above have not caught up with.
>
> What it must NOT be read as is a bounded retry in its own right. It names no
> failure, so a DETERMINISTIC post-usage crash satisfies it on every attempt, and
> the caller's ordinary dials do not stop that: max_retries and budget are both
> off by default, and max_zero_spend_attempts cannot fire at all here, since its
> counter resets on exactly the reported usage this requires. The loop bounds it
> separately.


## `internal/harness/opencode_export.go`

### Usage accounting on opencode 2.x (CLA-675)

> Usage accounting on opencode 2.x (CLA-675).
>
> The 2.x stream under-reports. Verified against 2.0.32: step_finish is emitted
> only for steps that ended in tool calls, never for the step that ends the
> session with a plain answer, and never for the title-generation request a new
> session makes. A plain text answer emits no usage at all. Summing the stream
> therefore misses at least one request per session, and all of a text-only
> session.
>
> `opencode session export --standalone <sid>` reads the session record out of
> the local database after the process has exited. Its `info` block carries the
> session's cumulative cost and tokens (title generation included), and every
> assistant message carries its own `finish`, `cost` and `tokens`. That is the
> authoritative source, and it also supplies what the stream no longer does:
> how the final step ended. The export spawns a private server but reads only
> the database; it does not start the session's MCP servers (verified: no MCP
> process launched, ~0.4s).
>
> Attribution. One Invoke can run several processes into ONE session (the
> CLA-406 resurrection probe and continuation, the CLA-584 stall resume), and
> their Results are summed by mergeResume and the probe fold. The export is
> cumulative, so each process is charged only the assistant messages created
> after it started. The difference between the session total and the sum of
> all its assistant messages (title generation) is charged to the process that
> created the session, which is the one not started with `-s`.

### settleUsage

> settleUsage replaces a finished run's streamed usage with the session
> export's figures when the export can be read, and leaves the stream sums in
> place when it cannot. It runs on every path where the process ran (clean
> exit, non-zero exit, our own kill), never on a launch failure.
>
> The export is run on a context detached from the caller's cancellation: a
> session killed by the wall-clock cap or a Ctrl-C still spent money, and the
> budget has to see it. opencodeExportTimeout bounds it instead.


## `internal/harness/opencode_inbox.go`

### Console - agent message delivery for the opencode adapter (CLA-911)

> Console -> agent message delivery for the opencode adapter (CLA-911).
>
> Claude's delivery writes a user turn to the session's stdin (claude_inbox.go).
> opencode has no such channel: `opencode run` takes its prompt as an argv
> argument and reads nothing further from us. What it DOES have is the private
> `opencode serve` the adapter already starts for every non-probe session with a
> clankerbar MCP server (opencode_mcp.go), and that server's own session API:
> a message is POSTed to /api/session/<id>/prompt with the server's basic auth.
>
> STEER, never queue. In a headless `opencode run` a queued item waits for the
> current execution to end — which is the moment the run process exits — so a
> queued message would be lost or start a turn nobody reads. Steer lands in the
> running turn, which is what steering from the task page means.
>
> The target is the LIVE process: every process a session runs (the original
> run, a CLA-406 resurrection, a turn continuation) starts its own private
> server and streams its own session id, so runSession attaches the one it is
> running and detaches it when that process ends. With no live target a message
> is simply not polled for: it stays queued on the plane, and delivery resumes
> against the next live server rather than posting to a stopped one.
>
> ACK only after a 2xx. inbox.Run advances its cursor on a nil error from the
> deliver callback, so a non-2xx answer or a transport error is returned as an
> error — the message stays un-ACKed and a fresh poller is offered it again.

### opencodeInbox

> opencodeInbox is one Invocation's console -> agent delivery: the run-id
> channel Invoke feeds from ResumeClaim/OnClaim, the plane half of the poll,
> and the live process target runSession attaches while it runs.
>
> The goroutine starts on the FIRST attach, not at construction, so a session
> that never gets a private server (a probe, or one whose config declares no
> clankerbar MCP server) runs no delivery goroutine at all.


## `internal/harness/opencode_interrupt.go`

### opencodeinterrupt.go

> opencode_interrupt.go — the ONE resume a silent exit 130 earns.
>
> opencode 2.0.32 has been seen to exit 130 with no error event when a route's
> quota ran out mid-turn, so the stream carried no evidence for the route
> switch; resuming the same session is what surfaces the real error.


## `internal/harness/opencode_mcp.go`

### opencodemcp.go

> opencode_mcp.go — the MCP preflight for opencode sessions (CLA-703).
>
> opencode 2.x connects MCP servers asynchronously, while `opencode run`
> submits the first model turn as soon as it has a session. On 2026-10-19 that
> race cost half a review panel: two members were offered no clankerbar tools
> on their first turn, decided they could not review, and ended in seconds —
> and the driver, reading a clean exit that recorded nothing, had no way to
> tell that from a reviewer that did the work badly.
>
> The fix is to remove the race rather than describe it: the adapter starts its
> OWN server (`opencode serve`) with the same pinned config, asks that server's
> `/api/mcp` endpoint until the clankerbar server reports `connected`, and only
> THEN attaches the run to it with `--server`. The model's first turn is
> therefore the first turn that can use the tools. If the server never reports
> connected, the session fails with ErrMCPNotConnected — a named infrastructure
> error the driver retries — instead of producing a silent "no record".
>
> The server API is opencode 2.x's own, verified against 2.0.32:
>
> 	GET /api/mcp?location[directory]=<dir>
> 	{"location":{"directory":"..."},"data":[
> 	  {"name":"clankerbar","status":{"status":"connected"}},
> 	  {"name":"other","status":{"status":"pending"}}]}
>
> Statuses are connected | pending | disabled | failed | needs_auth. `disabled`
> is an answer (there is nothing to wait for); `failed` and `needs_auth` are a
> verdict, not a race, so they fail immediately with the status in the message.

### opencodeWorkDir

> opencodeWorkDir canonicalises a session's directory the way the SERVER will
> key its location: absolute, with symlinks resolved.
>
> This is load-bearing, not tidiness. On macOS a temp dir is `/var/...`, a
> symlink to `/private/var/...`; the run's session is recorded at the resolved
> path, while an `/api/mcp?location[directory]=/var/...` query makes the server
> create a SECOND location keyed by the raw path. Polling that one reports
> `connected` for a location no session will ever use, the real location's MCP
> state finishes connecting later, and the first model turn still races — the
> exact failure this file exists to remove (verified against 2.0.32).
>
> EvalSymlinks needs the path to exist; a path it cannot resolve falls back to
> the absolute form, which is the best key available and still matches what the
> CLI records for an ordinary directory.

### startOpencodeServer

> startOpencodeServer launches `opencode serve` on a free loopback port and
> reads the address and password it announces. The process is put in its own
> group, so stopping it also reaps any MCP child it spawned.
>
> niceValue is applied here as well as to the run: the SERVER is what executes
> the session's tool calls, so a bash tool's build or test run is forked from
> this process, not from the `opencode run` client that asked for it.

### waitForMCP

> waitForMCP polls the private server until every wanted MCP server is
> connected (or disabled), and fails with ErrMCPNotConnected if one cannot be.
>
> A missing entry is treated as pending, not as success: the server loads
> config per location, and the project layer may not have arrived on the first
> poll. `failed`/`needs_auth` fail at once — those are answers, and waiting the
> full timeout on them only delays the retry the driver owes.

### The server keys its MCP state by location

> The server keys its MCP state by location. An answer for ANY OTHER
> location is not about the session that is about to run, so treating it as
> this one's status is how a `/var` vs `/private/var` query reported
> connected while the real session still had no tools (see opencodeWorkDir).
>
> An answer that does not identify its location at all is WORSE than a
> different one: it cannot be told apart from a status for some other
> session, and accepting it releases the first turn on no evidence. So both
> "empty" and "different" are refused; only exact identity is accepted
> (CLA-703 review, finding 2.1).

### opencodeEffectiveMCPServers

> opencodeEffectiveMCPServers names the clankerbar server(s) a session must have
> connected before its first turn, resolved across the config layers opencode
> actually loads.
>
> When the caller names an MCP config, that file is also pinned into
> OPENCODE_CONFIG_CONTENT — the layer opencode merges LAST — so the servers it
> declares are the effective ones. Only that file is read in that case: an
> ambient clankerbar entry cannot displace a server of the same name, and
> waiting for a differently-named ambient entry would stall the session on a
> server the backlog does not depend on.
>
> When the caller names NO file, however, opencode still loads its own layers,
> and they are then the only ones there are: the global config dir
> (OPENCODE_CONFIG_DIR when the caller pins one, else $XDG_CONFIG_HOME/opencode
> or ~/.config/opencode) and the project-level `opencode.json` /
> `.opencode/opencode.json` discovered from the INSTANCE DIRECTORY, then the
> content layer. A clankerbar server declared there is the session's backlog
> server just the same, so it is waited for here rather than discovered missing
> after the first model turn (CLA-703 review, findings 1.1 and 3.1).
>
> FAIL CLOSED: a layer that EXISTS but cannot be read or parsed leaves the
> effective config unknown, and "I could not tell whether a clankerbar server is
> declared" is not evidence that none is. A file that does not exist is simply
> not a layer. An empty result is a different, established answer: every layer
> was read and none declares a clankerbar server, so there is no name to wait
> for (the run still gets no backlog tools, which the driver's own no_mcp
> backstop classifies).

### collectOpencodeMCPServers

> collectOpencodeMCPServers adds every server in one document's `mcp` block that
> IS the clankerbar server: the documented name, or any entry handed
> CLANKERBAR_API_KEY (the config's own rule for which entry is the backlog's).
>
> The 2.x container (`mcp.servers`, an object of entries) and a 1.x server
> literally NAMED "servers" are told apart by the VALUE, the same rule config's
> mcpFile.UnmarshalJSON applies: the container's value carries none of the entry
> fields, while a 1.x entry carries at least one. Excluding the key outright
> skipped that supported legacy shape entirely (CLA-703 review, finding 1.2).

### addOpencodeMCPServer

> addOpencodeMCPServer adds a server that IS the backlog's: the documented name,
> or any entry handed CLANKERBAR_API_KEY.
>
> A DISABLED entry is deliberately added too. Excluding it at config-read time
> would let a config-disabled clankerbar server through the preflight and only
> fail at the driver's backstop — after a full paid session with no backlog
> tools — while a runtime `disabled` status fails the preflight immediately.
> Adding it lets the status wait observe `disabled` and fail fast with
> ErrMCPNotConnected (CLA-703 review, finding 4.3).


## `internal/harness/opencode_report.go`

### opencodereport.go

> opencode_report.go — the runner's done-report check, and its bounded resume.
>
> A runner-claimed session ends with a marker line and a JSON object the runner
> parses. A report the runner refuses normally ends the phase fail-closed and
> the session's work is wasted, so a refused report earns the same steer resume
> the output-cap stall does: the turn is replayable and the transcript is
> intact, only the report needs re-emitting.

### opencodeReportResumeIdle

> opencodeReportResumeIdle bounds ONE report-resume process for silence. The
> resume is a no-tools reply that re-emits one JSON object, so it should answer
> in seconds; the phase's own idle limit is tuned for a working session and was
> 15 minutes in the CLA-868 incident, so it let a hung resume hold the whole
> phase open until the watchdog killed it — the resume's silence then read as a
> dead session. Three minutes leaves room for a slow provider without ever
> being the phase's own bound. The phase bound is long partly because opencode
> streams a tool part only once it SETTLES, so a running build emits nothing;
> the steer forbids tools here, which is what makes a short bound safe.
>
> A var purely so the tests can shrink it: the production value is what they
> test against.

### resumeSkippedReason

> resumeSkippedReason names the end that forbids resuming this session, or ""
> when it may be resumed. ONE list serves two callers: the done-report resume,
> which must not talk over an end of the adapter's own, and the multi-turn
> sequence, which must not send a later turn into a session that ended badly.
> The reasons are the Result.TurnStopReason vocabulary, plus "probe", which
> only the report resume can see (a probe never reaches the turn sequence).
>
> A claim that is not held is one of those ends: the runner writes no record
> under a claim the session does not hold, so there is nothing to re-emit. A
> runner phase is seeded with in.ResumeClaim, so Held() is true whenever the
> runner would apply the report. A standalone invocation sets AllowUnclaimed:
> it neither claims nor writes a record, so it has nothing to guard against a
> missing claim, and its later turns would otherwise stop at turn 1.

### resumeBadReport

> resumeBadReport runs the runner's report check on the session's final message
> and, when it refuses the report, resumes the session with the check's error as
> the steer. A resume that stalls on a silent provider earns ONE fresh attempt
> with the same steer (CLA-877): a hang is usually transient, and the alternative
> is losing the phase — and every finished, pushed commit in it — to a bound
> tuned for a working session. A second stall, a resume process that errors, a
> reply the runner still cannot apply, or a wall clock with no room left to
> resume all fail closed: the report stands refused, the session's OWN end is
> left untouched, and Result.ReportResumeFailed names what really failed. That
> marker is STICKY once a stall is seen — no later return may leave the phase
> looking like a clean end.
>
> mergeResume replaces FinalMessage with the continuation's, so a corrected
> report is what the runner reads off this Result.

### The resume ran

> The resume ran. Judge BEFORE merging: the runner applies a report only
> on an orderly end or a recovered one, so a reply that still fails the
> check, or an end no report is ever read on (a cap kill, a non-zero exit
> with nothing recovered, a tool loop, an untrusted capture), produced
> nothing the runner can act on. Merging that end would brand the whole
> phase with the resume's process outcome, and NOT merging it while
> leaving the marker empty is the fail-open this finding is about: the
> session's own clean exit would read as a finished phase.


## `internal/harness/opencode_resume.go`

### CLA-406

> CLA-406: resurrecting a quietly-dead opencode session.
>
> The failure this answers was measured across 2026-08-20/21: the gateway drops
> a stream mid-generation — no terminal chunk, no usage, nothing on stderr —
> and opencode's own loop-exit bug (prompt.ts treating an "unknown" finish as a
> completed answer) ends the process cleanly with exit 0. The adapter sees the
> CLA-398 quiet-death signature: a final step whose reason is "unknown" and
> whose OWN usage is all-zero. Until now that was terminal.
>
> It does not have to be. The session transcript survives on opencode's side —
> only the in-flight turn died — so the session can be RESUMED in place via
> `opencode run -s <id>`, exactly as an interactive operator types
> "continue" after a cutoff. One small probe call risks recovering minutes to
> hours of accumulated session work; a failed probe costs only the wait it
> already took to reach this branch.
>
> The rules, agreed with the operator and recorded on the task:
>
>   - the resume prompt INFORMS THE AGENT of the convention (mirroring the
>     operator's own interactive pattern): you were cut off, your transcript is
>     intact up to the cut, confirm task ref + last action, then continue;
>   - coherence is decided MECHANICALLY: the reply must name the claimed task's
>     ref ("EZY-196" form). An amnesiac model cannot guess it; a genuinely
>     resumed session answers immediately. No judgement calls;
>   - ONE probe per death. A probe that fails or times out is not re-probed —
>     that death counts, and the existing dead-phase path takes over unchanged;
>   - every FUTURE death gets its own fresh probe (surviving one resurrection
>     does not spend the next);
>   - an overall cap bounds the whole loop, so a pathological night cannot turn
>     this into a perpetual churn of probe-and-die;
>   - a resurrected-and-recovered phase counts as never-dead for the dead-phase
>     counters; only FAILED probes reach that accounting.

### opencodeResumeRoundFloor

> opencodeResumeRoundFloor is the smallest slice of the Invoke's wall-clock
> budget worth STARTING a resurrection round against. A round's own uncharged
> overhead is opencodeResumeBackoff plus up to opencodeProbeTimeout BEFORE any
> continuation runs, and the continuation itself wants
> opencodeResumeWallClockFloor — so a slice under their sum can only buy a
> probe whose answer is never acted on. Checked BEFORE the round spends
> anything; the pre-continuation check re-verifies with the bare floor.
>
> A var purely so Invoke-level tests can shrink it (the production value above
> is the tested-against-default value), same as opencodeResumeBackoff.

### opencodeResumeBackoff

> opencodeResumeBackoff is the pause between noticing a quiet death and
> sending the probe. The gateway is dropping streams under load; prodding it
> the same instant the corpse lands rolls the dice against the same
> congestion. Short enough not to matter when it works, long enough to step
> over the worst of a burst.
>
> A var rather than a const purely so the Invoke-level tests can shorten it:
> the production value above is the tested-against-default value.

### resumeTargets

> resumeTargets extracts what a resurrection needs from a quietly-dead Result:
> the session id to resume INTO, and the claimed task's ref to verify the
> resumed agent against. Either missing means no resurrection is possible:
>
>   - no session id: the stream died before its first parseable event, so there
>     is no session on opencode's side to continue — nothing to attach work to;
>   - no held claim: the session died before claiming, so there is no work at
>     stake worth a probe (those deaths land in the first seconds anyway) AND
>     no ref to verify coherence against. Both disqualifiers are deliberate.

### resurrectionProbePrompt

> resurrectionProbePrompt is the prompt a resumed session is asked FIRST. It
> does two jobs at once, and both are load-bearing:
>
>   - it INFORMS the agent of what happened and of the convention, mirroring
>     the operator's own interactive pattern ("continue means you were cut
>     off") — an informed agent resumes confidently where an uninformed one
>     guesses at why the conversation has a hole in it;
>   - it demands a one-line answer naming the task ref, which turns "is this
>     agent actually resumed?" into a mechanical string match instead of a
>     judgement call. The ref is knowable only from the intact transcript: a
>     session that lost its context cannot produce it. The prompt deliberately
>     carries NO concrete example of the answer - real ref or placeholder, any
>     example is something a parroting model can echo back - so it describes
>     only the answer's SHAPE: "the ref your claim_task call returned".

### opencodeResumeArgs

> opencodeResumeArgs builds the args for a continuation run into an existing
> session: the same dialect as opencodeArgs, plus `-s <id>` and WITHOUT the
> phase prompt — a continuation carries its own short message. The adapter's
> server preflight (CLA-703) applies to a continuation exactly as it does to a
> first run: every non-probe session waits for the clankerbar MCP server before
> its first turn.
>
> sid must be an id this adapter SAW on a session's stream (resumeTargets reads
> it off Result.Raw, which only the stream parser writes). 2.x does not refuse
> an unknown id: verified against 2.0.32, `-s <unknown>` silently creates a new,
> empty session, so a made-up id would "resume" into amnesia. Every caller
> checks for an empty sid before it gets here.

### mergeResume

> mergeResume folds a successful continuation run's Result back into the base
> result of the original (dead) run, so what the driver sees is ONE session
> that was interrupted and carried on — not two.
>
> Field by field:
>
>   - Process outcome takes the CONTINUATION's wholesale: it is the run that
>     actually ended the merged session, and the dead run exited 0 with empty
>     stderr by definition of the quiet death. Keeping the dead run's zeroes
>     would hide a continuation that hit budget exhaustion (DetectLimit's scan
>     reads these streams), died non-zero, or was killed by a cap.
>   - Stdout/Stderr APPEND rather than replace: a post-mortem wants both halves
>     of the session. Each run's capture is already tail-bounded on its own, so
>     the sum stays bounded by two tails, not by the whole session.
>   - scans is reset: the classifier text just changed under any memoized
>     cache, and stale cache would feed DetectLimit/IsTransient pre-merge text.
>     Nil is a fully working, simply uncached Result.
>   - Untrusted/OutputDropped: an untrusted continuation makes the MERGED
>     result untrusted (the CLA-262 gates read this); dropped-output counts add.
>   - Tokens/CostUSD SUM: the budget must see every request the fleet made,
>     including the ones around the hole;
>   - UsageReported ORs: either run reporting is enough to count spend;
>   - FinalMessage takes the continuation's (the latest text wins, matching
>     the parser's own last-text-wins rule);
>   - Claim: the continuation's claim state stands only when it OBSERVED
>     something (held or settled) — `opencode run -s` streams only the new
>     turn, so an unobserved continuation knows NOTHING about the claim and
>     must not wipe the dead run's. Reports APPEND with sameClaim dedupe for
>     the same reason: neither run saw strictly more of the session than the
>     other, so both halves' delivery reports deserve to reach verification.
>     Both rules live in mergeObservation, shared with the probe's fold;
>   - Raw copies the continuation's (summing the numeric usage-breakdown keys
>     rather than overwriting them — Tokens sums, so the breakdown should too),
>     and TerminalReasonKey is RECOMPUTED rather than inherited: the key is
>     SHARED between the quiet-death marker and wall_clock_capped, so the
>     continuation's own terminal verdict — whatever it is — stands, and its
>     absence clears the dead run's. A clean continuation therefore clears the
>     quiet-death mark (the loop and the driver both read it), while a
>     continuation that itself died quietly keeps it — which is what lets the
>     caller's loop take its next round.
>     OutputCapReasoningKey (CLA-584) is recomputed the same way: it is the
>     stalled STEP's own count, so a second stall must replace the first's
>     figure rather than add to it.
>     RecoveredErrorsKey (CLA-700) is recomputed with them, for the same
>     shape of reason and a sharper one: it answers "did the run that ENDED
>     this session ride out a provider error", and that answer decides whether
>     a non-zero exit is judged on delivery rather than by its status. Summing
>     it inherited the dead run's count into a continuation that ended on a
>     REAL terminal error — the parser withholds the key in exactly that
>     shape — so the merged session read "recovered" over a stream that ended
>     on its error, the one classification the recovery arm must never make.
>     Recovery is a property of how the session ended, not a quantity that
>     accumulates; the continuation's own count stands, and its absence clears
>     the dead run's.

### mergeObservation

> mergeObservation folds one run's CLAIM OBSERVATION - claim state plus
> delivery reports - into a base Result, by mergeResume's rules: an unobserved
> claim wipes nothing (`run -s` streams only the new turn, so a run that saw
> no clankerbar tool events knows nothing), reports append with later-wins
> dedupe. Split from mergeResume so the PROBE's observations can take the same
> road: the probe turn runs with MCP tools available, so it can settle or
> advance the claim exactly as the continuation could, and dropping what it
> observed would re-create ERROR 1 one turn earlier - releaseHeldClaim posting
> ready over a task the probe just moved.


## `internal/harness/opencode_routes.go`

### Model route fallback inside ONE opencode session (CLA-782)

> Model route fallback inside ONE opencode session (CLA-782).
>
> A config may name an ordered route list for a model — the free provider route
> first, the same model's paid route after it. When the free route is denied
> mid-session, switching to the paid route is not a new session: the transcript
> lives on opencode's side and `-s <sid> --model <next>` resumes it, carrying the
> history. Verified on 2.0.32 against the free/paid deepseek pair: a resumed run
> recalled a codeword from the first turn.
>
> The rules:
>   - only a LIMIT switches (DetectLimit): a transient 429 stays on the existing
>     retry ladder, because a blip is worth retrying on the route that already
>     has the context and the budget for it;
>   - a Stop on a route that is NOT the last one is just a switch: a hard budget
>     exhaustion on the free route says nothing about the paid route's account;
>   - a limit on the LAST route is left on the Result unchanged, for the driver's
>     existing limit branch (wait or stop) — this function never decides that;
>   - a Probe never falls back: it is a liveness check, not work.
>
> Every run is charged EXACTLY ONCE: a successful continuation through
> mergeRouteSwitch (mergeResume sums its tokens and cost), and a failed one
> explicitly before the error is returned. A resume re-sends the transcript, so
> it is real money even when the answer is a limit again.

### switchRoutesOnLimit

> switchRoutesOnLimit walks the remaining routes while the session's Result keeps
> classifying as a usage limit, resuming the same session id on each next route.
> It reports every abandoned route on the Result (see RouteFallback) and leaves
> the final Result as the session's real outcome — clean when a later route
> finished the work, still limited when none could.
>
> A runSession error on a next route is returned after its spend is charged: a
> launch failure is the driver's failure to act on, not a limit to hide.

### setRouteReport

> setRouteReport writes the fallback report onto the Result. Nothing is written
> when the session never switched, so a single-route run's Raw is exactly the
> shape it had before this feature existed.
>
> The report is CUMULATIVE across switches: a multi-turn session can exhaust more
> than one route, and the driver marks EVERY abandoned route from this report
> (routes.go). A later switch therefore keeps the route the session started on
> and appends to the fallbacks already reported; RouteFinishedKey is the latest
> route, the one that ended the session.

### mergeRouteSwitch

> mergeRouteSwitch folds a continuation run made on a DIFFERENT model route into
> the result of the run it continues. It is mergeResume plus one deliberate
> difference: the abandoned route's streams are REPLACED, not appended.
>
> mergeResume appends Stdout/Stderr because a resurrection's two halves belong to
> one session on one provider and a post-mortem wants both. A route switch does
> not: DetectLimit and IsTransient scan those streams, so keeping the free
> route's "usage limit" text beside the paid route's clean finish would leave a
> successful session classified as limited. The abandoned stream is already in
> the iteration log (the adapter tees it live), so the Result keeps only the run
> that actually ended the session.


## `internal/harness/opencode_stall.go`

### opencodestall.go

> opencode_stall.go — CLA-584: the output-cap stall, and its one resume.
>
> MAK-123 (2026-09-24, makespdf) is the measured failure this answers: asked to
> embed a screenshot in a review deck, the model tried to type a 7,076-char
> base64 string into a tool argument, spent its whole output budget reasoning,
> and emitted nothing. Its final step_finish carried reason "length" with
> output 0 and reasoning 32000; opencode ended the turn and the session stopped
> 11 seconds later without ever moving the task on. All the daemon logged was
> its generic "the phase finished but never moved the task on", which names no
> cause.
>
> This is a DIFFERENT end from the CLA-398 quiet death that CLA-406 resurrects
> (reason "unknown", final step all-zero usage, the stream dropped
> mid-generation) and from opencode's own loop-exit bug. Here the harness ran
> to a normal turn end and reported real usage: the model hit its output
> ceiling. So the response is a resume too, but on its own terms:
>
>   - NO coherence probe. Nothing was lost — the session is alive and its
>     transcript is intact — so probing would only spend the turn the resume
>     exists to save.
>   - ONE resume, never a loop. A plain retry would re-spend the same output
>     budget the same way, so the resume carries a STEER that names what
>     happened and redirects the approach.
>   - A second consecutive "length"/zero-output stop ends the session exactly
>     as it did before this existed. The adapter writes the stall marker
>     (OutputCapReason) onto the Result and the driver's existing paths take
>     over; the marker is what lets the daemon name the end instead of logging
>     a generic hand-off failure.

### opencodeStallSteerPrompt

> opencodeStallSteerPrompt is the steer the single resume carries: what
> happened, do not repeat it, and the specific redirect the MAK-123 shape
> needs. Kept short because the session keeps its whole transcript — everything
> else it needs is already there.
>
> The redirect names the commands (CLA-581) rather than only saying "upload it
> by path": nothing else the session reads does — the served review-deck skill
> still teaches data: URIs, the MAK-123 path itself — so a steer without the
> command leaves the model to guess, and a second stall is terminal.

### resumeOutputCapStall

> resumeOutputCapStall performs the one resume the stall earns. It mirrors the
> CLA-406 continuation's rules where they apply and drops the ones that do not:
>
>   - never on an unreadable stream: the marker itself was parsed from the
>     bytes that may have been dropped whole (CLA-262), and the same gate keeps
>     the adapter and the driver's classifications agreeing about what ended;
>   - never once the run is being torn down: a cancelled context means the
>     phase is already ending (Ctrl-C, SIGTERM, a supervised drain), and the
>     resume would be killed on arrival — the sibling CLA-406 loop returns on
>     ctx.Done for the same reason. Nothing is said in that case: an operator's
>     teardown is not a stall diagnostic;
>   - no session id, no resume — a stream that died before its first event has
>     nothing on opencode's side to continue. NO CLAIM GATE, deliberately,
>     unlike CLA-406's probe path: the probe needs a held ref to verify
>     coherence against and declines without one, while this resume asks
>     nothing of the session and carries a steer that answers the shape itself.
>     A session that burned its whole output budget before claiming is the same
>     failure (and re-sends a tiny transcript, since the stalled step emitted
>     nothing), so it is resumed on the same terms;
>   - the wall-clock budget is re-checked first, then INHERITED by the resume
>     (MaxSessionWallClock = what remains), so a stall cannot hand the session a
>     fresh full cap the original never had;
>   - the parser is seeded with the dead run's live claim, so any
>     progress write/heartbeat the resumed turn makes is observed on the same
>     terms as the session it belongs to;
>   - the resume's spend is never lost: mergeResume sums it on the success
>     path, and an errored resume charges it explicitly — a resume re-sends the
>     whole transcript, so it is real money even when it fails to help;
>   - the resumed turn's CLAIM observation is folded even when the resume
>     ERRORS, under the probe's own guard (trusted capture, same task): the
>     turn may have recorded a branch or settled the task before the failure,
>     and dropping that would let the driver release a task on the strength of
>     the pre-resume claim — posting `ready` over work the resume just pushed.
>     The guard's `Names` conjunct means a claim-less stall (see above) cannot
>     fold a claim its resume made — a conservative miss, matching the probe's
>     fold exactly, and not a guard to "fix" without changing both;
>   - a clean resume is merged with mergeResume, so the driver sees ONE session
>     that was stalled and carried on rather than two.
>
> A resume that itself ends "length"/zero-output leaves the merged Result
> carrying the marker (mergeResume recomputes terminal_reason from the
> continuation), and NO further resume is attempted — the second consecutive
> stall is terminal, as designed.


## `internal/harness/opencode_stream.go`

### runSession

> runSession executes ONE `opencode run` process end-to-end and parses its
> stream. Split out of Invoke so the CLA-406 resurrection loop can run the
> probe and the continuation through exactly the same exec/parse machinery as
> the original session — same env, same permission policy, same capture — and
> merge the results, rather than maintaining a second code path that would
> drift the first time either half changed.
>
> delivery is this Invocation's console -> agent channel, or nil when the
> adapter is not delivering messages (see opencode_inbox.go). A process with a
> private server attaches it for the life of the process, so a message is only
> ever posted to the server that is actually running.

### The per-session wall-clock cap, enforced on the exec context so the ki

> The per-session wall-clock cap, enforced on the exec context so the kill
> needs nothing from the stream: opencode reports no turn count and takes no
> turn flag, so elapsed time is the only backstop this adapter can offer a
> phase (CLA-368). The derived context is ALWAYS created, cap or no cap, so
> there is one cancellation path rather than two shapes of ctx below.
>
> Never on a PROBE: a probe is a one-word liveness check, not a session doing
> work, and a capped probe would report a phase-shaped cap for something no
> phase ran. The driver never sets the field on a probe either — this is the
> belt to that braces, so a future caller reusing an Invocation cannot turn a
> probe into a capped session by accident.

### CLA-703

> CLA-703: a working (non-probe) session gets a private server of the
> adapter's own, and does not run its first model turn until that server
> reports the clankerbar MCP connection established. The OLD shape —
> `run --standalone`, which spawns its own private server and submits the
> prompt immediately — is exactly the race: opencode 2.x connects MCP in the
> background, so a fast model could take its first turn with no clankerbar
> tools at all.
>
> A PROBE keeps `--standalone`: it is a one-word liveness check of an
> already-created session and does not use the backlog, so waiting for MCP
> would only eat its time box.

### Only a session with a clankerbar server gets the preflight

> Only a session with a clankerbar server gets the preflight. That
> server is resolved across the config layers opencode actually loads:
> the named file when there is one, otherwise the global config dir and
> the project-level files discovered from the instance directory. A
> layer that exists but cannot be read fails CLOSED — an unknown
> effective config is not evidence that no backlog server is declared —
> and a session that does declare one never gets its first turn until
> that server reports connected (CLA-703 review, findings 1.1 and 3.1).

### A cap that can hang is not a cap

> A cap that can hang is not a cap. The capture points Stdout/Stderr at
> io.MultiWriter values, so os/exec makes its own pipes and cmd.Run waits for
> every WRITER of them to close — and CommandContext kills the direct child
> only. A runaway session is exactly the case with a bash-tool grandchild (a
> build, a test run) or an MCP server holding the inherited fd, which survives
> the SIGKILL and keeps the pipe open: without a delay, Invoke blocks past its
> own deadline in the one scenario the cap exists for. WaitDelay force-closes
> the pipes and lets Wait return.
> The orphan itself is killed by the process-group Cancel below; this is
> the backstop BEHIND that kill, for a descendant that escaped the group -
> a daemonised child (setsid) lands in a group of its own that no signal
> of ours reaches (the same escapee claude's session path has no backstop
> for at all, CLA-423). Set whenever the exec context carries ANY deadline -
> one this function created, or one a caller handed in (the CLA-406
> probe's 30s box arrives that way, and the old `sctx != ctx` test missed
> exactly that case): an fd holder would otherwise keep the pipe open past
> the kill and hang Run past its own box. An ordinary uncapped session's
> I/O is never cut short by it.
>
> CLA-739 arms it too whenever the repeated-failure guard is armed, because
> that guard is a cancellation with no deadline, and a kill that can hang is
> not a kill either. The idle watchdog is the same shape of cancellation, so
> it arms it too. What that changes for a session neither guard fires on: a
> grandchild still holding the pipes after opencode exits on its own now has
> Run return exec.ErrWaitDelay 5s later instead of blocking until the
> grandchild lets go - the CLA-414 branch below already reads that as the
> clean end it is.

### Kill the whole process group on ANY cancellation of sctx - the

> Kill the whole process group on ANY cancellation of sctx - the
> wall-clock cap, or the caller's own cancellation (a Ctrl-C that stopped
> reaching the session subtree through the tty the moment Setpgid moved
> it out of the foreground group). Through exec.Cmd.Cancel, not a monitor
> goroutine: os/exec calls Cancel only while it still considers the
> process live, so the post-reap window shrinks from "every capped
> session" to the instant between Process.Wait returning and the
> watchdog's handshake, where os/exec's own Kill reads os.ErrProcessDone.
> killProcessGroup's raw syscall.Kill narrows the recycled-pid hazard by
> orders of magnitude without eliminating it - unlike Process.Kill it
> enjoys no pidfd/reaped-status protection, so a group id reused inside
> that window would be signalled in error. Accepted rather than hardened:
> skipping the group kill whenever the direct child looks already-reaped
> could strand exactly the in-group descendants this file exists to kill,
> trading the bar away for a microseconds-wide hazard. The direct child
> is killed too (the trailing Kill mirrors CommandContext's own default
> Cancel), and a Kill on an already-dead child reads as os.ErrProcessDone,
> which os/exec treats as nothing to report.

### Parse as the stream arrives, retain only a bounded tail of it for the

> Parse as the stream arrives, retain only a bounded tail of it for the text
> scans, and tee live to the console when one is set (the JSON event stream —
> a readable renderer is a TODO; raw is honest live output).
>
> Live parsing is load-bearing here rather than merely tidy: this adapter SUMS
> per-step usage across the whole session, so a capped copy of stdout walked
> afterwards would silently drop the early steps and under-count the budget by
> however much of the stream had scrolled away (CLA-262).

### Our own deadline, not the caller's

> Our own deadline, not the caller's: ctx.Err() distinguishes a session we
> cut off from a run-wide Ctrl-C / SIGTERM / supervised-wait cancellation,
> which cancels the parent too and is NOT this phase reaching its backstop.
> Marked after p.finish, which rewrites Raw wholesale when it saw usage.
>
> runErr is the third conjunct because the deadline is read AFTER Run
> returned: a session that exited cleanly a hair before it, with the deadline
> passing in the window while the output was parsed, would otherwise be
> marked as capped. Nothing would misbehave today — the driver takes its
> exit-0 branch first — but the Result would be lying about how the session
> ended, and the next reader of this marker inherits the lie.

### The process exited cleanly on its own

> The process exited cleanly on its own — ExitCode 0 — but a grandchild
> (a backgrounded build, a test run) inherited the output pipes and held
> them open past cmd.WaitDelay, so os/exec force-closed them and Run
> returned exec.ErrWaitDelay instead of an *exec.ExitError. p.finish has
> already run (the CLA-299 ordering: parse whatever arrived, THEN
> classify), and every event the session emitted was consumed by the live
> parse as it arrived — only grandchild bytes written after the parent's
> exit are lost, and those were never this session's output. This is a
> clean end, not a failure: returning it as an error failed a completed
> attempt in drainPhase with no retry classification (CLA-414). Residual
> risk, accepted: force-closed pipes mean this cannot be distinguished
> from a parent killed while pretending to succeed — ExitCode 0 plus a
> fully parsed live stream is the strongest evidence available either way.
> The same holds for a run-wide cancellation landing inside the window:
> sctx reads Canceled, not DeadlineExceeded, so !timedOut is true and the
> attempt ends "clean" during teardown. Nothing durable turns on it —
> salvage, handback and spend are computed from res before any error
> check either way, and the cancelled driver ctx stops the run one spawn
> later — so the noise is left standing rather than guarded against.

### opencodeEvent

> opencodeEvent is one line of `opencode run --format json`. The stream is a
> sequence of typed events, each wrapping a message part; this captures only the
> fields the loop needs and tolerates the rest.
>
> Shapes, recorded against opencode 2.0.32 (every event also carries a
> top-level `timestamp`):
>
> 	{"type":"step_start","sessionID":"ses_...","part":{"id":"prt_...","messageID":"msg_...","type":"step-start"}}
> 	{"type":"text","sessionID":"ses_...","part":{"id":"prt_...","type":"text","text":"DONE",...}}
> 	{"type":"step_finish","sessionID":"ses_...","part":{"type":"step-finish","reason":"tool-calls",
> 	   "cost":0.055563,"tokens":{"input":18511,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}}}
> 	{"type":"error","sessionID":"ses_...","error":{"type":"provider.auth","message":"...","status":401,...}}
>
> step_finish is emitted for TOOL-CALL steps only: the step that ends the
> session with a plain answer emits none, so neither the final reason nor the
> final step's usage is on the stream. That is what the session export after
> exit is for (opencode_export.go); the stream figures are the fallback.

### opencodeToolState

> opencodeToolState is a tool call's lifecycle slot on its part. Where claude
> splits a call across an `assistant` tool_use block and a later `user`
> tool_result block, opencode emits the part once it has settled and carries
> the arguments and the result TOGETHER (recorded against opencode 2.0.32 with
> the MCP server out of Code Mode):
>
> 	{"type":"tool_use","part":{"partID":"prt_...","type":"tool","id":"call_0",
> 	  "tool":"clankerbar_claim_task","state":{"status":"completed",
> 	    "input":{"taskId":"T-9"},
> 	    "output":"{\"task\": {\"id\": \"T-9\", \"ref\": \"CLA-1\"}, \"run\": {\"id\": \"run-1\"}}",
> 	    "title":"clankerbar_claim_task","metadata":{...}}}}
>
> A refusal takes the other terminal status and puts the error where the output
> would have been. There is no `is_error` flag to read — the STATUS is the flag,
> which is why a status this adapter does not recognise must never be treated as
> success:
>
> 	{"state":{"status":"error","input":{...},
> 	  "error":"No tool named \"clankerbar_claim_task\" is currently available. ..."}}
>
> `output` is a STRING, not an object: an MCP tool's result arrives flattened to
> its text content, which is the shape noteClaimed already parses.
>
> A large result is TRUNCATED on 2.x, the same failure Claude Code's spill
> caused on the phase seam (CLA-330, docs/large-tool-results.md). Verified
> against 2.0.32: an MCP result above 51,200 bytes arrives as
>
> 	"output":"[showing 0 lines of 1; full output saved to <data>/opencode/tool-output/tool_...]",
> 	"metadata":{"metadata":{"truncated":true,"outputPath":"<the same path>"},...}
>
> and the full text is in that file. Plane payloads reach that size (a
> claim_task carrying decisions, a get_task at 56 KB). opencodeTruncatedOutput
> rehydrates it from the file, under the guards that function states.

### opencodeParse

> opencodeParse accumulates FinalMessage, Tokens and CostUSD from the event
> stream, ONE LINE AT A TIME as it arrives.
>
> Tokens/cost are SUMMED across every step-finish part: opencode reports usage
> per step (one step = one LLM turn), not cumulatively, so a multi-turn drain
> emits several step-finish parts that each cover their own turn. FinalMessage is
> the last non-empty text part — the final assistant answer, after any
> intermediate per-step messages.
>
> Because it sums, it has to see EVERY step — which is exactly why it runs on the
> live stream instead of over the retained tail (see Invoke).

### lastStepReason

> lastStepOut and lastStepReason are the MOST RECENT step_finish's OWN
> output and reasoning token counts (CLA-584). lastStepOut is the
> discriminator for the output-cap stall — a final "length" step with ZERO
> output — and it is deliberately not the all-zero test above: the recorded
> MAK-123 stall carried reasoning=32000 and a nonzero total, so only the
> output figure sees it. lastStepReason rides onto the Result for the
> daemon's named log line. Both move only with a step_finish that carried a
> reason, so they always describe the same event as lastReason (deadscan's
> rule, kept in step so the two classifiers cannot disagree).

### recoveredErrors

> recoveredErrors counts the typed error (or 2.2.x retry) events the stream
> did NOT end on:
> an event that is followed by any later typed event is one the session
> recovered from, because 2.x retries a provider failure itself and carries
> on (CLA-700). lastEventWasError is whether the most recent typed event was
> itself an error — exactly the question buildOpencodeErrorText asks of the
> finished stream, tracked here as the stream arrives so the two cannot
> disagree about how the session ended.

### idleStepCall

> idleStepCall is the synthetic tool call an opencode STEP opens in the idle
> guard: the step IS the call window this stream exposes.
>
> opencode forwards a tool part only once it has settled and gives no
> request-side tool event at all (see noteTool), so a long call's silence sits
> somewhere between the step's step_start and its step_finish. The window
> therefore spans the WHOLE step: step_start opens it and step_finish closes it,
> and nothing in between does. A text part or an earlier settled tool inside the
> step can precede another tool run in the same step (the incident: a
> "Blocking on it with a bounded wait" text line followed by a 15-minute tool
> wait), so closing on any such line would hand that call's silence back to the
> idle clock - the bug this task is about.
>
> Opening the window at step_finish instead (the obvious-looking signal: reason
> "tool-calls") would pause the clock for the model's NEXT request, the one thing
> that must stay on the idle clock (CLA-900).

### CLA-900

> CLA-900: the step window is CLOSED above, before the Part check. A
> step_finish must never OPEN one: it is published after the step's tools
> have returned, so the silence that follows it is the model's NEXT
> request - an idle-clock question, never a tool-cap one.
>
> The reason rides on the same part as the usage. The LAST one wins: it
> is the step the session's stream ended on, which is the step that says
> whether a final answer was produced.

### tokens and cost are siblings on the part; count each independently so

> tokens and cost are siblings on the part; count each independently so
> a step that reports one without the other still lands in the budget.
>
> The LAST step's OWN usage is tracked separately from the session sum:
> the CLA-398 quiet-death signature is a final step that reported
> NOTHING (all-zero), whatever the earlier steps cost — so the sums that
> feed the budget are not the discriminator, the last step's own block
> is. Reset per step; the final value is the one that survives.

### noteTool

> noteTool feeds one tool event into the shared clankerbar observation, which is
> what populates Result.Claim and the delivery Reports.
>
> Only a TERMINAL state is acted on for that observation, and that is
> FUTURE-PROOFING rather than a filter this stream needs today: `opencode run
> --format json` forwards a tool part only once it has settled (true of 1.x
> source, cli/cmd/run.ts, and of every 2.0.32 stream recorded for this adapter),
> which is why the recorded fixtures carry exactly one event per tool part. The
> check is here because the status is the ONLY error flag
> opencode gives — there is no is_error sibling — so a version that started
> forwarding in-flight parts would otherwise have us read a call that had not
> returned as a success. An unrecognised status is ignored for the same reason:
> the cost of being wrong that way is an unobserved claim (the driver hands the
> task back — one expiring lease), against a claim recorded for a call that
> failed, which is a task the driver believes it holds and does not.
>
> A part still in flight is not a claim, but it DOES open a tool call for the
> idle watchdog: the session is working rather than hanging, and the call's own
> cap bounds it (see idleGuard). On today's stream this branch never fires —
> opencode forwards only settled parts — and the call window comes from the step
> itself instead (step_start .. step_finish; see idleStepCall).
>
> A tool part is acted on ONCE. noteClaimed rebuilds Result.Claim wholesale, so
> a re-delivered terminal event for a claim already settled by a later
> a status write would resurrect it — and the driver would post `ready` over a task
> that is in review. This is insurance, not an observed bug.
>
> # What this shape cannot do, and what it costs
>
> claude sets Claim.HasWIP on the REQUEST (see noteToolUse), deliberately: a
> session killed between issuing `record_progress(branch: …)` and its result has
> still pushed work, and erring towards "there is WIP" only costs the reclaim an
> expiring lease already costs. opencode gives no request-side event at all — the
> arguments arrive fused to the result — so that arm cannot fire early here. A
> session killed mid-call (SIGTERM, a cancelled context) therefore records no
> WIP, and releaseHeldClaim posts `ready` over a branch that was pushed. The
> truncated-stream variant of this is caught by the capture's untrusted mark; the
> killed-mid-call one is not, and it is a property of opencode's stream rather
> than something this adapter can fix.


## `internal/harness/opencode_timeout.go`

### opencodetimeout.go

> opencode_timeout.go — the provider stream timeout the adapter pins (CLA-877).
>
> opencode 2.0.32 takes per-provider settings under `providers.<id>.settings`
> (the 2.x native dialect; the deprecated `provider.<id>.options` dialect is
> normalized onto it). `chunkTimeout` is the web-request SSE read bound: when no
> chunk arrives within the window opencode aborts the request with "SSE read
> timed out" and the run ends with an error event and a non-zero exit. Its own
> default is 900000 ms (15 minutes) — the very bound of the phase idle watchdog,
> so a hung stream stayed silent until the watchdog killed the session, and the
> kill read as a dead phase rather than as a provider timeout. A shorter bound
> turns that silence into an error the driver can classify and retry.

### opencodeProviderChunkTimeout

> opencodeProviderChunkTimeout is the longest silence between streamed chunks
> the adapter tolerates. Five minutes is well inside the idle watchdog's 15m,
> so a hung stream fails as a provider error before the phase is killed, and is
> far above the gap between chunks of a healthy streamed answer (including one
> that thinks for a while: reasoning itself streams). Deliberately NOT the
> full-request `timeout`: a long generation that keeps streaming must not be
> cut off.
>
> A var purely so the conformance test can shrink it rather than wait five
> minutes; the production value is what the unit tests pin.

### opencodeProviderTimeouts

> opencodeProviderTimeouts pins chunkTimeout on every provider this invocation
> can NAME, in the 2.x native dialect. A route is `provider/model`, which is the
> only provider identity an Invocation carries; a model with no `/` (the
> harness's own configured default) names no provider, and opencode's default
> applies there. An existing entry is merged, never replaced: the MCP config may
> itself carry `providers.<id>` (or the deprecated `provider.<id>`), and wiping
> its settings would lose the operator's apiKey/baseURL/models.
>
> Verified against 2.0.32: the content layer is merged last, the setting reaches
> the request wrapper, and a stalled stream then aborts with "SSE read timed
> out" instead of going silent (docs/harness-conformance.md, `_chunkTimeout`).


## `internal/harness/opencode_turns.go`

### opencodeturns.go

> opencode_turns.go — planned multi-turn invocations in ONE opencode session.
>
> A reviewer that first takes notes and then writes findings needs the later
> turn to see everything the earlier ones read, and a config may want a
> different (stronger or cheaper) model per stage. Both are one opencode
> session: each turn is a `run -s <same id>` continuation, exactly the shape
> the resurrection, route-switch and report resumes already use, so the
> transcript survives and only the new turn's prompt is sent.

### runTurns

> runTurns runs the follow-up turns of a multi-turn invocation in the session
> its first turn created. The first turn has already run and its message is the
> sequence's first entry.
>
> It returns the invocation the LAST turn ran on and whether every turn ran.
> The caller's recovery passes and done-report check must use that invocation,
> not the first turn's: a final turn with its own model must be resumed and
> re-steered on that model, and must never be swapped onto the invocation's
> route list.
>
> A turn is followed by another only after it ends cleanly. The guards are
> resumeSkippedReason's own list - never a second copy - and a usage limit on a
> turn running the invocation's route list first follows the existing
> switchRoutesOnLimit rules. The first turn that fails a guard stops the
> sequence: Result.TurnStopped names it and no later turn runs.

### afterNonFinalTurn

> afterNonFinalTurn judges a non-final turn's end and returns the invocation a
> later turn would run on.
>
> The turn's OWN invocation decides the route behaviour: a limit first follows
> switchRoutesOnLimit on the turn's list (a property of the stage's configured
> route), and a turn with its own model carries no list (Invocation.OnTurn), so
> its limit stands and stops the sequence - the stage's model is never silently
> swapped. Only a turn that ran the invocation's route list advances base onto
> the route that finished it. Every other bad end is resumeSkippedReason's
> decision, named with its own vocabulary.


## `internal/harness/output.go`

### This file holds the two bounds every adapter puts on a child's output,

> This file holds the two bounds every adapter puts on a child's output, and the
> reason they exist at all.
>
> A session is an agentic run that may last hours, and its stdout is the whole
> event stream: every assistant message, every tool_use, and every tool_result —
> which includes the verbatim content of any file the agent read. Hundreds of
> megabytes is an ordinary night's work. The adapters used to accumulate all of
> it, both streams, for the life of the drain, so a runaway child could OOM the
> SUPERVISOR — the process whose entire job is to outlive the child (CLA-262).
>
> Nothing needs the whole of it. The parsed figures (tokens, cost, the claim, the
> delivery reports) are taken line by line as the stream arrives, and the only
> consumer of the retained bytes is the limit/transient classification, which is
> a regex scan over the harness's OWN output and whose answer is always at the
> END: a usage-limit notice, a terminal_reason, the last error the CLI printed.

### maxStreamLine

> maxStreamLine is the largest single line an adapter will assemble from a
> child's stdout. One stream-json event carrying a large file read can be
> megabytes, so this is generous; it is a bound, not a budget.
>
> A line ABOVE it is not silently shortened. The event it carried is lost to
> the parser, so the Result's figures are incomplete in ways nothing
> downstream can see — which is why crossing this marks the Result untrusted
> rather than trimming and carrying on. See Result.Untrusted.

### retainedTail

> retainedTail is how much of each stream (stdout, stderr) a session KEEPS,
> for the classifiers and for the operator-facing diagnostic.
>
> Two MiB per stream, so a session RETAINS at most 4 MiB whatever the child
> does. (The process's peak is higher: assembling one line costs up to
> maxStreamLine on top, in the scanner's buffer or a lineSink's. Those are
> transient per-line working space; this is what is held for the session.)
> Two MiB is far more than any classification needs — the widest pattern here
> matches inside one line, and failureDetail renders 400 runes of it — and it
> keeps enough context that an operator reading the tail can see what led up
> to the failure.
>
> The cost, stated rather than implied: a signal emitted MORE than a window
> ago is no longer read. A session that announced a retryable API error in its
> first minute, then produced 6 MiB of stream and exited non-zero with nothing
> retryable near the end, is now classified non-retryable and stops the run
> where it used to back off and retry. Accepted, because the alternative is
> unbounded retention in a process built to outlive its child, and because
> what the CLI says LAST about how a session ended is the better evidence
> anyway. Not configurable: a dial here is a way to reintroduce the OOM.

### tail

> tail is a bounded io.Writer that keeps the LAST retainedTail bytes written to
> it and discards the rest as it goes, so a stream of any size costs a fixed
> amount of memory.
>
> It hands back whole lines only. When anything has been dropped, the leading
> partial line goes with it: a half line does not parse as an event, and the
> classifiers deliberately read a non-event stdout line as the CLI's own words
> (that is how a usage-limit notice arrives when the stream never starts), so
> handing them the back half of a truncated tool_result would re-open exactly
> the injection CLA-258 closed — the agent's narration, and the backlog text it
> quotes, read as a harness diagnostic.
>
> Not safe for concurrent use: each stream gets its own tail, which is how
> os/exec already treats the writers it is given.

### reserve

> reserve makes room for need more bytes, growing by doubling and CLAMPING at max
> so the buffer's capacity is bounded and not merely its length.
>
> Both halves matter. Unclamped, append's growth factor overshoots the window by
> about a quarter for the life of the session; allocated eagerly at max, every
> stream pays for the whole window up front — including a probe, which produces a
> few hundred bytes and of which a week-long wait runs hundreds. Doubling with a
> clamp costs a handful of copies on the way up and nothing after.
>
> Callers must have trimmed first: this never allocates past max, so it relies on
> len(buf)+need <= max, which is what the trims in Write establish.

### lineSink

> lineSink turns a byte stream into whole lines for a live parser, with an
> explicit cap on how long one line may get.
>
> It exists so an adapter can parse as the stream arrives — rather than
> accumulating the whole of stdout and walking it afterwards — while writing to
> the same os/exec-managed writer it always did. bufio.Scanner is the equivalent
> on the reader side, and claude uses it because it reads the pipe itself;
> maxStreamLine is the cap for both, so neither path holds an order of magnitude
> more than the other (they may disagree by a byte at the exact boundary, which
> nothing depends on).
>
> A line that overruns max is DISCARDED, not shortened, and Overran reports it:
> half an event parses as no event, and the caller marks its Result untrusted
> rather than reporting figures with a hole in them.
>
> onLine is handed a slice that is REUSED by the next line: copy anything you
> keep, as both parsers do. It must not be nil — a sink with nowhere to deliver a
> line is a programming error, not a mode.

### Drop what we have and everything up to this line's newline

> Drop what we have and everything up to this line's newline. Keeping the
> head would hand the parser a truncated JSON object, which is
> indistinguishable from a line the CLI wrote itself.
>
> nil rather than cur[:0]: the array behind an overrun line is up to
> maxStreamLine, and keeping it alive for the rest of the session is the
> memory this file exists to bound.

### keepLineBuffer

> keepLineBuffer is how large an assembly buffer may be and still be REUSED for
> the next line. Above it the array is released instead.
>
> Reuse is what keeps the ordinary case allocation-free: a stream is thousands of
> events of a few KiB each, and re-growing for every one of them would be silly.
> But a single legal 15 MiB event would otherwise leave 15 MiB pinned behind a
> `cur[:0]` for the rest of a session that may run for hours — the same
> hold-forever shape this file exists to remove, just moved from the tail to the
> line buffer. So the buffer survives while it is cheap and is dropped once it is
> not.

### capture

> capture is one child's output, wired the way an adapter that lets os/exec do
> the copying needs it: a bounded tail per stream, a live line parser fed from
> stdout, and the console tee.
>
> Shared by codex and opencode because the wiring is the whole of what they had
> in common here, and because a helper is drivable by a test without executing a
> binary — the alternative left the untrusted arm of both adapters unpinned.
> claude does its own reading (it needs the pipe to render progress and to drain
> after a failed scan), so it composes the same two pieces by hand.

### result

> result closes the capture and returns the Result skeleton for the adapter to
> fill: the retained tails, a scan memo, and — if a line overran the cap — the
> reason the figures that follow cannot be believed. `harness` names the CLI in
> that message, since it is read by an operator.
>
> Call it after cmd.Run/Wait: that is what joins os/exec's copy goroutines, and
> so what makes everything written here safe to read.


## `internal/harness/priority_unix.go`

### sessionCommand

> sessionCommand builds the exec.Cmd for one session process, applying the
> invocation's scheduling priority to it.
>
> The priority is applied to the CHILD, not to this daemon: everything a session
> starts — test runners, builds, dev servers, MCP servers — is forked from the
> harness process and inherits the value, while the driver's own plane calls stay
> at its priority. The kernel then gives another process (a CI runner sharing the
> machine) the CPU when they contend, and gives the session the whole machine
> when nothing else wants it — what a nice value does that a CPU quota does not.
>
> nice(1) lowers its own scheduling priority (raises its nice value) and then
> execs the target, so the child keeps one pid: the adapters' process-group kill,
> WaitDelay and Cancel all still see exactly the process they started. It is used
> rather than a setpriority call after Start, which would leave a window in which
> the child runs at the daemon's priority, and rather than a SysProcAttr field,
> which Linux does not offer.
>
> A value of 0 is the byte-for-byte old shape — a plain exec.CommandContext with
> no wrapper between the daemon and the harness. A negative value is REFUSED: it
> would ask to raise the session above the daemon, which config validation does
> not allow and which would fail for an unprivileged daemon anyway.


## `internal/harness/processgroup_unix.go`

### killProcessGroup

> killProcessGroup sends SIGKILL to the negative PID (the whole process
> group), so descendants that survived the direct-child kill are also
> terminated. It is a no-op when the process has not yet started.
>
> Straight SIGKILL with no SIGTERM grace period is deliberate: the direct
> child already got SIGKILL from CommandContext's default cancel, so the
> subtree had no grace before this change either, and a half-dead tree is
> exactly what holds pipes open past the kill. Descendants get no chance to
> clean up (a grandchild build leaves partial artefacts) - accepted, rather
> than spend opencode's 5s WaitDelay budget on a graceful shutdown we cannot
> bound.


## `internal/harness/pwd.go`

### pinPWD

> pinPWD returns env with the child's PWD pinned to workDir - the directory the
> caller is about to set as cmd.Dir - and OLDPWD dropped. An empty workDir
> returns env untouched: there is nothing to pin it to, and inventing one would
> move a session the caller deliberately left in the driver's own cwd.
>
> exec.Cmd.Dir changes the child's REAL working directory and nothing else. The
> inherited PWD still names wherever the daemon was started, because a shell -
> not the kernel - is what keeps that variable honest, and no shell is involved
> in an exec. Most tools ignore PWD. opencode does not.
>
> CLA-441, verified live against opencode 1.18.19: opencode bootstraps an
> instance at the real cwd, then creates a SECOND instance at $PWD and runs the
> session THERE. Reproduced without the driver - `opencode run` with cwd
> ~/dev/ezyapp and PWD=/Users/jason/dev/clankerbar-cli lands in clankerbar-cli;
> the same command with no PWD lands in ezyapp. So every session every daemon
> spawned ran in the daemon's start directory, whatever the project's workdir
> said, and three separate defects were all this one:
>
>   - the wrong project's MCP server, because opencode merges the project-level
>     opencode.json it discovers from the INSTANCE directory after the file the
>     driver names, so the start directory's repo redirected `mcp.clankerbar`
>     (14 CLA-* leases were held by sessions labelled [ezyapp]);
>   - read/edit asks in a shape the permission policy cannot match, because
>     opencode makes them relative to the git worktree it resolves at the
>     instance directory, not at cmd.Dir (see opencodePermission);
>   - repo files - AGENTS.md, CLAUDE.md - read from the wrong repo.
>
> Every adapter here inherits the parent environment wholesale, so every adapter
> carried it; the fix is applied to all three rather than to opencode alone,
> because "which harness honours $PWD" is not a fact this repo controls.
>
> OLDPWD is dropped rather than rewritten. It means "the directory a shell cd'd
> FROM", and a value inherited from the daemon's shell describes a jump the
> child never made; there is no honest value to give it here.
>
> The pin is ABSOLUTE. Whoever honours $PWD reads it as a path with no cwd to
> resolve it against except the child's own - the very thing being pinned - so a
> relative cmd.Dir must not travel as a relative PWD. An unresolvable workDir
> falls back to the value as given: still the caller's directory, merely
> unnormalised, which is strictly better than the daemon's.
>
> It filters the INHERITED environment only. Callers apply it to os.Environ()
> before appending Invocation.Env, so an explicit caller-supplied PWD still wins
> on the last-duplicate-wins rule os/exec applies - the same precedence the
> adapters give every other variable they set.


## `internal/harness/routes.go`

### Model route fallback (CLA-782)

> Model route fallback (CLA-782).
>
> A model field may name an ORDERED list of routes for the same model — a free
> provider route first, its paid twin after it — and the opencode adapter uses
> that list inside ONE Invoke: when a route hits a usage limit it resumes the
> SAME session on the next route, so the history survives and the work carries
> on. The adapter reports what it did through Result.Raw (the same channel the
> resurrection counter uses), and the driver records the marks so the REST of
> the run stays on the route that worked until the limited one's reset.
>
> The types live here rather than in the opencode adapter because the driver
> reads them and must not import an adapter package for the shape. Only opencode
> implements the switch today; a single-route Invocation on any adapter behaves
> exactly as it did before this existed.


## `internal/harness/signal_unix.go`

### exitSignal

> exitSignal returns the number of the signal a finished child died on, or 0
> for a normal exit. The wait status only knows a signal on platforms with a
> POSIX-ish WaitStatus (darwin, linux); elsewhere exitSignal always reports
> none, which is the honest "not a signalled exit".
>
> It exists as a function rather than an inline type-assertion so the unix-only
> syscall decoding stays behind the build tag, and an adapter's ExitError
> branch stays one line.


## `internal/harness/toolloop.go`

### toolloop.go - CLA-739

> toolloop.go - CLA-739: kill a session that keeps making the same failing tool
> call.
>
> The measured failure (2026-10-20): nine review-panel sessions on one model each
> called a clankerbar task tool with an argument the plane refuses, got the same
> `invalid_release` error back, and called it again - 65 to 316 times per
> session, 1,538 calls in all. Every retry is a model request that re-sends the
> whole context (~180K tokens), so the nine used ~273M tokens in ~45 minutes and
> exhausted the provider's 5-hour window. Nothing here noticed: the turn cap is
> claude-only, the wall-clock cap is off by default, and the token ceiling is a
> 150M floor sized for a legitimate long session, not for a loop.
>
> This is the backstop, and it has to hold whatever the plane or the model does,
> so it keys on the one thing every such loop has in common: the SAME tool
> returning the SAME error on consecutive calls.
>
>   - The INPUT is not compared. In the incident the `branch` argument varied
>     across three distinct inputs while the error stayed identical; a model
>     varying an argument it does not understand is still looping.
>   - Any other tool call in between, or a success of the same tool, breaks the
>     streak. A session that reads a file between two refused updates is doing
>     something else between them, and an occasional repeated refusal is how a
>     session finds out what the plane wants.
>   - The comparison is on the full error text, not a prefix: two refusals that
>     differ only past the first line are different refusals.

### toolLoopGuard

> toolLoopGuard counts consecutive failing calls of one tool with one error. A
> nil guard is disabled, and every method is safe on nil, so an adapter can hold
> one unconditionally and an Invocation that set no threshold costs nothing.
>
> Not safe for concurrent use: each session's stream is parsed by one goroutine.


## `internal/harness/turns.go`

### OnTurn

> OnTurn returns a copy of the invocation pinned to one follow-up turn: the
> turn's own model when it names one, otherwise the invocation's current one.
>
> A turn that names a model also DROPS the route list, deliberately: a fallback
> is a property of the stage's configured route, and walking it after a per-turn
> model hits a limit would run the stage on a model its caller did not choose.

