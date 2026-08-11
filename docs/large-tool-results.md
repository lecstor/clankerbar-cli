# A tool_result too large to inline is replaced by a pointer

Claude Code does not put an oversized tool result into the event stream. It writes the
result to a file and substitutes an envelope naming it:

```
<persisted-output>
Output too large (66.4KB). Full output saved to: /Users/<u>/.claude/projects/<slug>/<session>/tool-results/<tool_use_id>.json

Preview (first 2KB):
[
  {
    "type": "text",
    "text": "{\"task\":{\"id\":\"e2c1b1f1-...
...
</persisted-output>
```

Three properties matter to the driver, and all three are why this was expensive to find:

- **It is not an error.** The block carries no `is_error`, so every check that keys on
  refusal passes it straight through. The result has simply stopped being JSON.
- **The content arrives as a bare string**, not the usual array of typed blocks.
- **The preview is truncated at the front of the payload.** Parsing the preview is not a
  repair: `run.id` sits at the end of a `claim_task` payload and is never inside the first
  2KB.

The file at that path holds the original `content` verbatim - the same array of blocks the
stream would have carried - so reading it and flattening it recovers everything exactly.

## What it cost (CLA-330)

`claim_task` returns the project's standing decisions **in full**, so its payload grows with
the project's age. At 103 decisions it reached 66KB, over the threshold. The 2026-08-11
live phased run then went like this:

1. The session claimed CLA-321. The plane recorded the claim and held it.
2. `noteClaimed` got the envelope instead of the payload, could not parse it, and returned
   **in silence** - `res.Claim` stayed at its zero value.
3. `Claim.Held()` is `TaskID != "" && !Settled`, so it read false.
4. Two later `update_task` calls were **also** dropped, because `noteToolUse` gates on
   `res.Claim.Names(args.TaskID)` and `Names` is false on an empty `TaskID`. So `HasWIP`
   never got set either.
5. `releaseHeldClaim` returned silently for the same reason.
6. The driver logged *"the implement phase ended without holding the task - nothing for the
   next phase to resume, so this drain ends here"* and stopped. Phase 2 never ran.

Phase 1 had done everything right and stopped at its checkpoint exactly as instructed. The
scope instruction worked; the driver could not see that it had. 46.6M tokens of real work
sat on pushed branches with no PR.

**The failure was invisible at every step**, which is why localising it cost a whole live
run. That is now fixed twice over: the envelope is rehydrated, and both of `noteClaimed`'s
give-up paths say what they dropped.

## The guard on the path, and why it is not optional

The path is honoured **only when its base name is the id of the tool call carrying it**
(`<tool_use_id>.json`).

Everything inside a tool_result is potentially backlog text quoted back at the driver -
that is the whole subject of `internal/harness/injection_test.go`, where a task body
containing `hit your` used to fake a usage limit. An unguarded "read the path you find in
here" is strictly worse than that: a task body could point the driver at a file of its
choosing and have the contents parsed as a claim, giving the driver a claim on a task
nobody asked for and a handback aimed at it.

A `tool_use` id is minted by the harness for that call in that stream. Text written before
the call cannot name it.

## This is not a one-off shape to paper over

Any agent-facing payload that grows without bound eventually crosses the threshold. That is
the same failure clankerbar's own `docs/token-budget.md` exists to prevent from the other
side, reached from this one: the plane grew a payload, and the *driver* broke.

Two consequences worth keeping in view:

- **A green unit suite proved nothing here.** Every existing claim-tracking case builds its
  tool_result from a fixture of a couple of hundred bytes, so the threshold was never in
  reach. `internal/harness/persisted_test.go` drives the verbatim envelope from the failing
  run's transcript instead.
- **Only the `claude` adapter is covered.** `codex` and `opencode` have their own output
  handling and have not been checked for an equivalent spill.

## Still owed

An **end-to-end demonstration** driving a real two-phase sequence through a spawned harness
binary. The parse seam (`claude.consume`) is unexported, so `internal/loop` cannot reach it;
the loop's phase tests script `harness.Result` values directly and the harness tests stop at
`renderAndParse`. The bug fell precisely into the gap between those two, and it is now
covered from the harness side - but the gap itself is still there. Decision `50979b2e`
already records this as owed.
