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

The line we run has a known one, and upgrading within 1.18.x does not escape it: a turn that
returns an indeterminate finish reason makes opencode **exit 0 with no error**, discarding the
turn - `packages/opencode/src/session/prompt.ts:1113` omits `"unknown"` from the loop-exit
exclusion that the same file applies ~180 lines later at `:1295`. Reproduced against a local
fake provider by patching v1.18.19 and watching the behaviour flip.

Filed upstream as **anomalyco/opencode#43622**, still **open** as of 2026-08-28 with no
maintainer response. Present in every release from v1.15.0 through v1.18.19, and absent from
the one older checkout available here (1.2.27, at `prompt.ts:323`), so it reads as a guard
dropped in a refactor rather than a missing feature.

For us that failure looks like a session that ended with no branch and no error (it had
usually done paid work first - only the final step is empty) -
what the driver calls a dead phase. It hit 6 of 16 implement sessions on 2026-08-20.
**CLA-406** shipped the mitigation (v0.10.1+): on the quiet-death signature the
adapter RESUMES the same session in place (`opencode run --session <id>`) after a
25s backoff, probes the agent with an informed coherence check (name your task ref),
and continues it on a mechanical match - bounded at 5 resurrections per Invoke, one
probe per death, falling through to the dead-phase path unchanged when a probe fails.
The upstream loop-exit bug still stands; this catches its victims instead.

## Why the fleet is pinned to 1.18.19, deliberately

An earlier version of this page warned: if we ever pin a patched build, **do not vendor the
one-line fix** - adding `"unknown"` to the list at `:1113` removes the silent exit but
produces an unbounded retry loop, 138 steps and climbing in the test with no cap observed.
The shape that works is `:1301`, where a `content-filter` finish is surfaced as an error.

**Upstream then shipped exactly that one-line fix, in v1.18.20, with exactly that
consequence.** So the trade on the 1.18.x line is:

| Build | Indeterminate finish reason | Consequence |
|---|---|---|
| **<= 1.18.19** | exits 0, silently, discarding the turn | a dead phase - which **CLA-406 catches and resurrects** |
| **>= 1.18.20** | does not exit; retries without bound | burns paid steps with no error and no cap; CLA-406 never fires, and the wall-clock cap is the only backstop |

A death we detect and recover beats a loop that spends until the clock runs out, so **the
fleet runs 1.18.19 on purpose**. Homebrew has 1.18.21 installed but **unlinked**;
`/opt/homebrew/bin/opencode` is a symlink into the version-pinned `opencode@1.18.19` formula.

**Do not "fix" that pin by relinking.** It looks like drift - a newer version installed and
not in use - and it is not. If you see `brew list --versions` showing a newer opencode than
`opencode --version` reports, that is this pin working.

**What lifts it:** a build where an indeterminate finish reason is *surfaced as an error*
rather than either swallowed or retried - the `:1301` shape. Watch #43622. Until then, do not
upgrade past 1.18.19, and check `opencode --version` (not `brew list`) before concluding
anything about which build produced a failure.

## See also

- [`docs/opencode-tool-schema-limits.md`](./opencode-tool-schema-limits.md) - the separate
  Console Go tool-schema rejection family. A different failure with a different cause; do not
  reach for it when a session dies quietly with zero usage.
- CLA-381 (the opencode2 incident and the classifier fix), CLA-401 (the investigation that
  cleared the gateway and the model), CLA-406 (our mitigation).
