# Per-route pricing windows

`route_schedules` and `route_windows` (CLA-947) keep new work off a route whose
provider bills peak rates, **without** stopping the routes that do not.

`pause_windows` is the operator's daemon-wide "start no new work" schedule: quiet
hours, machine load. A route schedule answers a narrower question — "do not pay
*this* provider's peak price" — and is a filter on route **selection**, not a
gate on the driver. Both keys may be set, and they mean different things.

## Config

```json
"route_schedules": {
  "deepseek-peak": {
    "timezone": "Australia/Brisbane",
    "lead": "0s",
    "windows": [
      { "days": "mon-fri", "from": "11:00", "to": "14:00" },
      { "days": "mon-fri", "from": "16:00", "to": "20:00" }
    ],
    "when_all_blocked": "wait"
  }
},
"route_windows": {
  "opencode-go/deepseek-v4.1-flash": "deepseek-peak",
  "commandcode/deepseek/deepseek-v4.1-flash": "deepseek-peak"
}
```

- **Named schedules, not inline windows per route.** Peak hours move as a set, so
  one definition attached to N routes is one edit.
- **`route_windows` is keyed by the route ID** — the exact model string the driver
  iterates — never by provider alone or model alone.
  `synthetic/hf:deepseek-ai/DeepSeek-V4.1-Flash-1017` is the same DeepSeek model as
  `opencode-go/deepseek-v4.1-flash` but has no peak tier, so a model-keyed
  schedule would wrongly pause it.
- The timezone, lead, day and `HH:MM` handling is `PauseWindows`'
  (`internal/config/pausewindows.go`), including the `from > to` overnight form.
- A route with **no** entry is always available: an empty/absent schedule never
  pauses anything, so a newly added model cannot inherit a schedule by accident.
- `route_windows` naming a schedule that does not exist is a **Validate error**
  naming the route and the missing schedule.
- A route ID that matches no configured route is a **doctor WARN**
  (`clankerbar doctor`, `route_windows`), not a Validate error: a typo must be
  visible, but a config listing a route from a paused experiment must still start.

## Where it applies

The filter is the same one that applies usage marks:
`Driver.routableRoutes` / `config.RouteChoiceAt`. A route inside its window is
dropped exactly like a marked route, then the first remaining route in configured
order wins.

| spawn | covered by |
| --- | --- |
| every phase (first, next, handoff respawn, retry) | `invocationFor` -> `routeRoutesFor` |
| a sequential review-panel member | the same, since each member is a `drainPhase` pass |
| a parallel review-panel fan-out | `holdThroughPanelRouteWindows`: the whole panel is held over EVERY member's route list, not only the first member's |
| the supervised-wait probe | `probeInvocation` aims at the route the next spawn would use, and refuses the probe when every route is windowed |
| the supervisor's conflict-only session | `startConflictSession` -> `config.RouteChoiceAt` |
| the deterministic close-out | not applicable: it runs no agent session |

A **running** session is never interrupted; the filter decides only which route a
new spawn may start on. A mid-session adapter route switch (CLA-782) is inside the
session unit and is not re-filtered.

The deadline for a supervised usage-limit wait's **next** probe — its first
probe is `probeWait` alone, so the route term applies only to successors — is
the earliest of:

- the poll interval;
- a stated reset plus a short grace, when that is sooner;
- the instant one of the phase's routes clears **both** its usage mark and its
  window, plus the same short grace.

A window that ends mid-interval is therefore probed when it opens, instead of a
usable route sitting idle until the next interval tick (CLA-986: a wake at 13:59
held the probe to 14:29 when the window ended at 14:00). If the walk itself ends
with a route already clear — its usage mark expired while a later route was
being probed — the next probe is one grace away rather than the whole interval.
A wake that ended on a probe error keeps the full interval, so a failing probe
cannot tight-loop (CLA-988).

A usage MARK and a window are different answers to different questions, so both
apply: a mark only ORDERs the candidates (a phase still starts, on the route whose
mark lifts first), while a window removes them. When every route is marked the
first-unlifting marked route is still used, but never one that is also inside its
window under `"wait"` — the phase then holds until the window ends.

## All routes blocked

`when_all_blocked` is per schedule and answers "every route this phase may use is
inside a window":

- `"wait"` (the default) — spawn nothing. The phase boundary holds until the
  earliest blocking window ends, through the same hold as a scheduled pause: the
  claim is kept, its lease renewed, presence beaconed, and STOP / RESTART / HALT /
  RELOAD stay responsive. The hold RE-READS the config at every wake, so a RELOAD
  that removes or shortens a schedule ends the hold at once and one that adds a
  window puts it back. The log names the resume instant and the schedule.
- `"run"` — start on the first route whose own schedule allows `run` (the first
  configured route when they all do), accepting the peak price.

`"run"` degrades **only** the window filter. A route carrying a **usage mark** is
still skipped: the mark is a provider denial, not a price.

If a window opens in the instant between the boundary check and the spawn,
`invocationFor` refuses the spawn (no session, no spend) rather than starting on a
paused route. That refusal is a scheduling condition, not a failed phase: it is
recorded as `windowHeld` (never as the evidence-gate `empty` shape), a panel member
it happens to is named INCOMPLETE but charged to neither the member's failure
streak nor the dead rate, and the next drain's boundary holds the spawn.

## Identity

Both keys are `omitempty`, so a config that sets neither marshals and hashes
exactly as it did before this existed (`Config.Identity()`). Route schedules are
machine-local, like `pause_windows`: a stored run-config document applied mid-run
neither sets nor clears them.
