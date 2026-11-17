# CLA-913: do CLI-driven sessions still heartbeat themselves?

Measured 2026-11-17 against the CLI's local per-attempt logs under
`~/.local/state/clankerbar/loop/roster/<instance>/iteration-*.log`, with the
driver's own `daemon.log` lease-renewal lines as corroboration. No code or
prompt was changed: this is the diagnosis record, and the fix is filed as
CLA-917.

## Method

- **opencode** logs are JSONL. A heartbeat call is a `tool_use` event whose
  `part.type` is `tool` and whose `tool` is `clankerbar_heartbeat`.
- **claude** logs are rendered text. A heartbeat call is a line matching
  `^\s*→\s*mcp__clankerbar__heartbeat\s*$`. Text mentions (grep output, source
  comments) are not calls.
- Excluded from every resumed phase: its one `heartbeat(runId)` resume step.
  Fresh implement sessions require no resume beat, so every beat they issue is
  self-issued.

## Full-population scan

1,470 iteration logs scanned; 486 contain at least one real heartbeat call.
Every one is dated 2026-10-21 through 2026-11-12:

| date | logs with a real call |
|---|---|
| 2026-10-21 | 145 |
| 2026-10-22 | 73 |
| 2026-10-27 | 22 |
| 2026-10-28 | 241 |
| 2026-11-12 | 5 |
| 2026-11-13..17 | 0 |

The runner took over on 2026-11-13 (`daemon.log`: "runner claimed ..." starts
then); every run since is runner-claimed and issues no heartbeat.

## Sample

Durations are the first-to-last session event in the iteration log; the two
claude logs are rendered and take their durations from `daemon.log`.

| task | phase | harness | mode | length | hbs | resume | self-issued |
|---|---|---|---:|---:|---:|---:|---:|
| CLA-908 | implement | opencode | runner | 61.6m | 0 | 0 | 0 |
| CLA-908 | review | opencode | runner | 25.6m | 0 | 0 | 0 |
| CLA-911 | implement | opencode | runner | 21.3m | 0 | 0 | 0 |
| CLA-911 | review | opencode | runner | 15.1m | 0 | 0 | 0 |
| CLA-911 | fix | opencode | runner | 8.9m | 0 | 0 | 0 |
| CLA-911 | verify | opencode | runner | 5.6m | 0 | 0 | 0 |
| CLA-912 | implement | opencode | runner | 12.9m | 0 | 0 | 0 |
| MAK-331 | implement | opencode | session-claimed | 47.3m | 1 | 0 | 1 |
| CLA-848 | implement | opencode | session-claimed | 22.6m | 1 | 0 | 1 |
| MAK-331 | implement (takeover) | opencode | session-claimed | 35.2m | 1 | 0 | 1 |
| MAK-331 | review | claude | session-claimed | 1.2m | 1 | 1 | 0 |
| CLA-848 | review | claude | session-claimed | 0.7m | 1 | 1 | 0 |

The three session-claimed opencode runs start with `next_task` then
`claim_task` — they are fresh claims, not resumed phases — and their single
heartbeat lands at 46.6m, 22.2m and 10.6m in. Each of those sessions had also
fetched `https://clankerbar.com/skills/clankerbar.md`, whose diagram says
"heartbeat every ~10 min (a third of the 30-min lease)".

## The driver was renewing at the same time

`daemon.log` for the same windows:

- CLA-908 implement: "lease renewal stopping after 6 beat(s)" — 6 driver beats,
  0 session beats.
- MAK-331 implement (2026-11-12): "renewing the lease of MAK-331 every 10m0s"
  then "lease renewal stopping after 4 beat(s)" — 4 driver beats, plus the
  session's 1.
- CLA-848 implement: "lease renewal stopping after 2 beat(s)" — 2 driver beats,
  plus the session's 1.

## Answer

Current runner-claimed sessions do **not** heartbeat themselves: their briefs
(`internal/config/prompts_runner.go`: `runnerForbidden`, `runnerResumeNoClaim`)
say "do NOT call heartbeat - the lease is the runner's", and every sampled run
from 2026-11-13 onward confirms it.

The last session-claimed runs (2026-11-12, before the runner took over) **did**
issue one self-issued renewal each, while the driver's `leaseRenewer`
(`internal/loop/heartbeat.go`) was already renewing the same run. That is the
redundant model turn the task was looking for, and it is why CLA-917 (ready,
with a doneWhen) was filed: add one line to the session-claimed phase briefs
telling the session the driver renews its lease, keeping the single resume
heartbeat and leaving the runner briefs alone.
