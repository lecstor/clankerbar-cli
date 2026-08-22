# Which opencode build we run, and why not opencode2

Short answer: **the Homebrew stable line** (`/opt/homebrew/bin/opencode`, 1.18.x). Not
`opencode2`. This page exists because that was decided on 2026-08-19 and never written
down, and re-deriving it a day later cost real time.

## The adapter runs a bare name, so PATH decides

`internal/harness/opencode.go` execs `"opencode"` with no path. There is no config dial for
the binary, so **whatever `opencode` resolves to on the daemon's PATH is what runs**, and
pointing the fleet at a different build is a code change, not a settings change.

**Never answer "which build ran?" from PATH.** Read it off the session:
`~/.local/share/opencode/log/opencode.log` records `version=` on the line where it creates
the session, e.g. `message=created id=ses_... version=1.18.18`. `clankerbar doctor` also
reports it, as `pass  harness[opencode]  /opt/homebrew/bin/opencode (1.18.18)` - the same fact
from the same resolution the driver will do. The `[opencode]` qualifier appears because
opencode is a phase harness here; were it the run harness the label would read bare
`harness`.

## Why not opencode2

We ran the `opencode2` nightly (`0.0.0-next-17403`) on the first mixed-harness drain,
2026-08-19. At 12:48 an implement session about five minutes and 21 tool calls into a task
emitted a single untyped error event and exited 1:

```json
{"type":"error","error":{"type":"unknown","message":"Transport"}}
```

A transport drop is the canonical transient, but the driver treated any opencode exit 1 as
terminal, so one provider hiccup stopped **the whole daemon** and needed a hand restart.
That is **CLA-381**, and it fixed our classifier - transient stream failures now retry under
the existing backoff. The classifier fix stands on its own and is not a reason to avoid
opencode2.

**No decision record exists for the switch back to stable.** What follows is reconstructed
from CLA-381 and can be corrected by whoever actually made the call.

The likely reason is narrower and duller than the incident: the nightly's error surface is
undocumented and changes without notice, and an unattended fleet cannot classify failures it
cannot predict. CLA-381's note is *consistent with* that, though it warns about both builds in
the same breath - *"the error surface may be version-dependent: this was the opencode2 nightly
... stable 1.18.x may report stream failures differently, so classify by pattern"*. Read as
written, it pins neither surface; it is an argument for pattern-matching, not for stable.

**There is a live trap from that experiment.** `~/.config/clankerbar/bin/opencode` is a
symlink to `opencode2.exe`, created 2026-08-19 12:43, five minutes before the incident above.
It is **abandoned**, and inert only because that directory is on nobody's PATH. If anything
ever puts it there, the whole fleet silently switches binaries with no log line saying so.
Delete it, or if you keep it, say here what it is for.

## Stable is not free of defects either

The 1.18 line had a known one through **v1.18.18**: a turn that returns an
indeterminate finish reason made opencode **exit 0 with no error**, discarding
the turn - `packages/opencode/src/session/prompt.ts:1113` omits `"unknown"`
from the loop-exit exclusion that the same file applies ~180 lines later at
`:1295`. Reproduced against a local fake provider by patching v1.18.19 and
watching the behaviour flip. Filed upstream as **anomalyco/opencode#43622**;
it read as a guard dropped in a refactor rather than a missing feature.

**That silent exit is fixed as of v1.18.20** (we run v1.18.21): upstream changed
the behaviour to RETRY the silently-empty stream instead of ending the turn —
the shape an anomalyco maintainer credits in the #43622 thread, part of the same
family as still-open PR #43881. What that leaves us is the *other* half of the
original warning: against a **persistently** empty stream the retry never
terminates on its own (verified empirically: ~18 provider requests/second with
no backoff, no halt, the message history growing one empty turn per attempt).
The wall-clock cap (`max_session_wall_clock` / per-phase `max_wall_clock`) is
therefore now **load-bearing for every opencode run**: it is the only backstop
that can end such a session, and the loop's salvage preserves everything it
wrote when the cap fires. See `docs/harness-conformance.md` for the observable
per-build contract and the "capped spin" marker-precedence note.

For the fleet before v1.18.20, the silent exit looked like a session that ended
with no branch and no error (it had usually done paid work first - only the
final step is empty) - what the driver calls a dead phase. It hit 6 of 16
implement sessions on 2026-08-20. **CLA-406** shipped the mitigation (v0.9.1+):
on the quiet-death signature the adapter RESUMES the same session in place
(`opencode run --session <id>`) after a 25s backoff, probes the agent with an
informed coherence check (name your task ref), and continues it on a mechanical
match - bounded at 5 resurrections per Invoke, one probe per death, falling
through to the dead-phase path unchanged when a probe fails. On v1.18.20+ a
spin no longer *ends* with the quiet-death signature (it ends by wall-clock
cap if at all), so the resurrection path is dormant there; it stays live for
older builds and as the last line of defence if a build regresses.

If we ever pin a patched build rather than relying on the tap: **do not vendor
the one-line fix**. Adding `"unknown"` to the list at `:1113` removes the silent
exit but produces an unbounded retry loop - 138 steps and climbing in the test,
with no cap observed; the shipped v1.18.20 change is the same family, bounded
only by a provider that eventually answers. The shape that works is `:1301`,
where a `content-filter` finish is surfaced as an error.

## See also

- [`docs/opencode-tool-schema-limits.md`](./opencode-tool-schema-limits.md) - the separate
  Console Go tool-schema rejection family. A different failure with a different cause; do not
  reach for it when a session dies quietly with zero usage.
- CLA-381 (the opencode2 incident and the classifier fix), CLA-401 (the investigation that
  cleared the gateway and the model), CLA-406 (our mitigation).
