# Doctor Rationale

The doctor checks: what each one reads, what it refuses and why.

This file holds the design and process reasoning retired from the code comments in
`internal/cli (doctor)` when those comments were cut to the one-to-three-line rule (CLA-809). The
comments beside the code keep the invariant, trap or constraint an editor needs at
that site; the fuller rationale - why a default is the number it is, how a
failure was measured, which alternative was rejected - lives here. Task refs and
dates are provenance for the reasoning, not a changelog.

Each section is one source file. Within it, a heading names the declaration the
comments belonged to, or quotes the first line when they were a free-standing
section note.


## `internal/cli/doctor.go`

### doctor.go

> doctor.go — preflight diagnostics for unattended runs.
>
> Every failure mode this checks for is one that otherwise surfaces only as
> DEGRADED BEHAVIOUR, hours in: a rejected key drops the loop into blind mode, a
> missing binary kills the first session, an unreachable endpoint is
> indistinguishable from an empty queue. The point of `doctor` is to convert all
> of those into one cheap, loud answer before an overnight run starts.
>
> The checks are deliberately split into small functions over an injectable
> doctorEnv: PATH lookups, version execs and the backlog read are the three
> things a test cannot do for real, and they are the three things most worth
> testing.


## `internal/cli/doctor_backlog.go`

### checkFleets

> checkFleets reports whether fleet activity reporting (CLA-466) is wired and
> its endpoint reachable — the same PASS/WARN table shape as the MCP wiring
> checks (the CLA-448 precedent). One check per project, like backlog's.
>
> Every outcome is PASS or WARN, never FAIL: reporting is telemetry, so an
> outage or a gap degrades console visibility while the loop runs exactly as
> before. A rejected key already FAILs in the backlog check above it, where it
> actually stops the run; saying it twice here at FAIL strength would make
> doctor exit non-zero over a condition that costs nothing but a picture.

### checkInstanceIdentity

> checkInstanceIdentity warns when two config files in THIS config's directory
> would resolve to the same Fleet-page instance identity (CLA-501).
>
> The collision is invisible from inside one daemon  -  every daemon believes its
> own name is fine  -  so the check has to look sideways at its siblings: each
> top-level *.json in the config's directory is read for instance_name and
> resolved through the SAME function the loop beacons under
> (config.ResolveInstanceName), and any identity claimed by two or more files
> warns. Files that do not parse as JSON are skipped: a file the loader cannot
> read cannot start a daemon, so it cannot beacon anything to collide with.
>
> Since CLA-501 the default identity embeds the config basename, so distinct
> filenames cannot collide; what remains is two siblings declaring the same
> explicit instance_name (or a truncation-induced prefix clash)  -  exactly the
> silent overwrite this check exists to catch before an overnight run.


## `internal/cli/doctor_budget.go`

### A cost ceiling on a harness that never reports cost is not a weak ceil

> A cost ceiling on a harness that never reports cost is not a weak ceiling
> but an ABSENT one: no code path can reach it, because nothing populates the
> figure it compares against (codex exec reports tokens, not money). So every
> verdict below reasons about the ceilings that can actually FIRE, not the ones
> written down - otherwise a codex run whose only dial is cost reports a set
> budget while having none, and a codex run with cost plus wall clock escapes
> the wall-clock-only warning that in reality describes it exactly (CLA-288).
> ANY harness the run spawns that cannot report cost makes a cost ceiling
> unreliable, not just the top-level one: in a mixed sequence a share of the
> run's spend never reaches the accumulator the ceiling reads, so a dial
> reported as live would be holding a line it cannot see past (CLA-366).

### Per-harness blocks (CLA-367), read the same way

> Per-harness blocks (CLA-367), read the same way: what can FIRE, not what is
> written down. Two ways one cannot. Its cost dial may name a harness that
> never reports cost - the case above, one level down. Or the block may name a
> harness THIS config never spawns, whose ceilings are then unreachable however
> sane the numbers are. Both are annotated in place, and neither counts toward
> the "there is a live ceiling" reasoning below.
> "Does this run SPAWN it" - not "is it THE harness". A phase names its own
> harness since CLA-366, so a block for the harness the implement phase runs on
> is perfectly reachable while not being cfg.Harness, and calling it inert would
> be a false statement about a ceiling that will fire.

### Turn cap

> Turn cap: since CLA-343 the effective CLAUDE config ALWAYS resolves one —
> the operator's, else the built-in default — so "no turn cap" cannot
> happen; what can happen is that the cap is the DEFAULT, a runaway
> detector tuned to the largest measured session, not a budget the
> operator chose. The codex adapter has NO turn cap at all (its
> Invocation.MaxTurns never reaches the CLI), so for it the warning is
> skipped rather than claiming a guard that does not exist there.
> runsHarness, not cfg.Harness == "claude": on a mixed sequence the claude phase
> is bounded by the built-in default turn cap exactly as a claude-only run is
> (CLA-366), and the opencode phase beside it does not make that untrue.

### The advice rides in the NOTE, not in c.remedy

> The advice rides in the NOTE, not in c.remedy: the remedy line is one
> per check and the guard block's generic "set <dials>" usually claims it,
> so advice parked there is advice an operator routinely never sees. The
> tail clause is gated on the harness actually HAVING a turn cap: under
> codex MaxTurns never reaches the CLI either, so telling a codex operator
> the turn cap is their live backstop is advice that WOULD BE FOLLOWED
> straight back into the inert-dial hole this note exists to warn about
> (the reasoning harnessReportsCost uses below).
> `on`, not cfg.Harness: the dial is inert on the harness of the PHASE that
> would have used it (CLA-366), which on a mixed sequence is routinely not
> the run-wide one - and the backstop advice that follows is that harness's
> too, so naming the wrong one is advice that would be FOLLOWED wrongly.

### The per-session runaway ceiling still applies whatever the run-level

> The per-session runaway ceiling still applies whatever the run-level
> dials say, and the operator should know the number (CLA-343). Asked of
> the harnesses that RUN A PHASE rather than the run-wide name (CLA-366):
> a sequence implementing on opencode and reviewing on claude still has the
> ceiling active on its claude phase, and reporting on cfg.Harness alone
> would either hide that or claim it for a phase where TokenCeilingHit
> never fires. Harnesses with no mid-session ceiling are omitted, not
> listed as zero - claiming one is "still active" there would be false.

### The usual advice is "add maxcostusd" - which under a harness that

> The usual advice is "add max_cost_usd" - which under a harness that
> cannot report cost is advice to set a dial that does nothing, and it
> would be FOLLOWED, landing the operator in the cost-only case the
> sibling branch below warns about. Gated on the HARNESS, not on
> inertCost: the commonest codex config is a bare wall clock with cost
> unset, and that is the one that needs the corrected advice most.

### The run-wide breaker is checked BETWEEN sessions

> The run-wide breaker is checked BETWEEN sessions: it cannot see a single
> huge session coming, which is exactly what happened to the 285.9M run
> under max_tokens=75M. Say so on the line that reports it (CLA-344).
> Gated on a token ceiling from EITHER place: a run whose only token
> ceiling is in its harness's per_harness block is enforced between
> sessions in exactly the same way, so it needs the same warning and the
> same number (CLA-367).

### sessionTokenBounds is EMPTY when no spawned harness has a mid-session

> sessionTokenBounds is EMPTY when no spawned harness has a mid-session
> ceiling, which is every opencode-only and codex-only run. Appending it
> unguarded printed a bare "()" there - a broken sentence in the one
> surface whose whole job is not making sloppy or false statements. The
> two cases need different sentences, not one sentence with a hole:
> with a bound, name it; without, say plainly that nothing bounds the
> overrun, which is the sharper finding and was previously unsaid.

### A missing guard turns the verdict WARN (a FAIL above stays a FAIL

> A missing guard turns the verdict WARN (a FAIL above stays a FAIL — a
> broken budget is worse than a bare one). The findings ride as info lines
> so the detail line stays the budget's own story; the remedy names the
> dials, whose specifics are one info line each — unless the branch above
> already had a more specific one (the wall-clock-only case's "add
> max_cost_usd" is the better advice there).

### harnessReportsCost

> harnessReportsCost asks the registry whether this harness ever populates
> Result.CostUSD. An UNKNOWN harness is treated as reporting cost - i.e. the
> warning is withheld - because config.Validate has already refused that name, so
> the only way to get here is a doctor run over a config too broken to run at
> all, and warning about an inert dial on a harness that does not exist would bury
> the real finding under a speculative one.

### inertSessionWallClock

> inertSessionWallClock names the wall-clock dial this config sets that will not
> be enforced, and the harness it is inert ON - two empty strings when there is
> nothing to say. The phase's own `max_wall_clock` is named in preference to the
> run-wide `max_session_wall_clock`, so an operator is pointed at the line they
> wrote.
>
> Judged PER PHASE-HARNESS since CLA-366, because that is the level at which the
> question has an answer. A phase's `max_wall_clock` is inert exactly when THAT
> phase's harness will not enforce it - so a sequence implementing on opencode
> and reviewing on claude gets the warning for its claude phase and not for the
> opencode one, where the dial does fire. The run-wide `max_session_wall_clock`
> reaches every phase, so it is only worth calling inert when NO harness the run
> spawns honours it; if one does, the dial fires somewhere and saying otherwise
> would be the reassuring falsehood this check exists to remove. Judging the
> whole config by cfg.Harness would get both of those wrong in opposite
> directions - a false INERT on a mixed run whose opencode phase honours the
> dial, and silence on a claude phase where it truly does nothing.
>
> PARTIAL inertness of the run-wide dial is deliberately NOT reported, and this
> is the one place the check chooses silence over a true statement. On the
> implement-on-opencode / review-on-claude sequence the dial fires on the opencode
> phase and is dead on the claude one - true, and not worth saying, because the
> claude phase is bounded by its turn cap, so it is not the CLA-344 "bounded by
> nothing" case that the sibling noSessionBackstop note exists for. Saying it
> anyway would put a WARN on the shipped mixed-harness config that has no defect
> and no action behind it, and a warning an operator learns to ignore costs more
> than the sentence buys. If a harness ever appears with no turn cap AND no
> session cap, noSessionBackstop is what catches it - per phase, by name.
>
> An UNKNOWN harness says nothing, for the reason harnessReportsCost gives:
> config.Validate has already refused the name, and a speculative inert-dial
> warning would bury the real finding.

### Phase dials first, so the operator is pointed at the line they wrote

> Phase dials first, so the operator is pointed at the line they wrote - and
> read off cfg.Phases, NOT EffectivePhases, for exactly that reason: the
> effective phases have the run-wide dial resolved into every one of them, so
> iterating those would report an operator's `max_session_wall_clock` back to
> them as `max_wall_clock (phases)`, a line they never wrote. The HARNESS still
> comes from the resolver, since that is what a phase inherits.

### noSessionBackstop

> noSessionBackstop names a harness whose sessions in THIS run are bounded by
> NOTHING - it takes no turn flag and kills on no token ceiling, and the phases
> it runs leave unset the one dial that could bound it. "" when every spawned
> harness has some backstop. Gated on the harness being able to enforce the
> dial, so the advice ("set it") is advice that would work.
>
> Per phase-harness since CLA-366, and per phase within it: an unbounded
> opencode implement phase is exactly as unbounded when a claude review phase
> runs after it, so asking only about cfg.Harness would go silent on the very
> sequence this warning is for. A phase carrying its own max_wall_clock is
> bounded and does not count against its harness; the run-wide dial is resolved
> into every phase by EffectivePhases, so setting it clears all of them at once.

### sessionTokenBounds

> sessionTokenBounds renders the per-session runaway token ceiling for every
> harness this run spawns that actually HAS one, or "" when none does.
>
> Per harness for two reasons since CLA-366. The ceiling itself is resolved per
> harness (CLA-367's SessionTokenCeilingFor reads that harness's own block), so
> one number cannot describe a mixed run; and whether the ceiling exists at all
> is a capability - codex has no mid-session ceiling, so listing it with a number
> would claim a guard that never fires. A harness without one is OMITTED rather
> than shown as zero, which is the same choice the check makes everywhere else.

### Gated on the run spawning ONE harness, not on one harness having a cei

> Gated on the run spawning ONE harness, not on one harness having a ceiling.
> The bare number is only unambiguous when there is a single session kind for
> it to bound: on a mixed run it reads as a bound on every session, when in
> fact the opencode phase beside it has no mid-session ceiling at all. So the
> common unphased config's line is unchanged byte for byte, and a mixed run
> always names whose ceiling it is quoting.

### anyPhaseRunsTheDefaultTurnCap

> anyPhaseRunsTheDefaultTurnCap reports whether the effective config bounds any
> session by the built-in default rather than an operator-chosen cap — i.e.
> whether CLA-343's default is load-bearing for this run. The effective phases
> resolve phase → top-level → default, so a phase carrying DefaultMaxTurns is
> exactly a phase the operator never capped (an operator who set the same
> number explicitly is indistinguishable, and the statement "the default
> applies" is still true for them).


## `internal/cli/doctor_config.go`

### panelLabels

> panelLabels renders the review panel as `role -> harness/model` entries, in
> the order the driver spawns them. It reads the EFFECTIVE sequence, so a
> `review_panel` entry's own model (CLA-639) is shown as it will actually run —
> an operator's preflight should not have to re-derive the panel from config.
>
> The role is POSITIONAL (`review-N`, counted over the review-shaped phases,
> exactly as the driver derives the durable record's role). A member's `name` is
> only its log label, so a named member is printed `review-N (label)` when the
> two differ; printing the label alone would suggest the record is keyed on it.
> A TRIAL seat is marked `(optional)`, the same marker the daemon's INCOMPLETE
> line uses (CLA-784), so the operator's preflight shows which seat is being
> evaluated and which ones the panel relies on.
>
> Empty when no panel is configured or none expands: a config whose review
> carries its own prompt, an unphased run, or the absent key (the single
> reviewer) has no panel to report rather than an empty one to apologise for.

### singleReviewerLabel

> singleReviewerLabel renders the single reviewer a config runs when it sets no
> panel (CLA-639): the built-in, non-final review phase at the harness/model it
> will spawn on, parenthesized and followed by its review method when it names
> one (CLA-869). Empty when no such phase exists, so an unphased config or one
> whose review runs the final combined brief reports nothing extra — matching
> how doctor reports a phase detail only when the phase is there to report.

### reviewMethodLabel

> reviewMethodLabel renders the review method doctor reports after a reviewer's
> harness/model (CLA-869): empty for the shipped single-pass review, and for
> maps-session the model each stage will actually run on — an unset stage model
> inherits the phase's, and with no phase model the findings stage inherits the
> resolved notes stage (the live-session inheritance). Only a review naming no
> model at any level runs the harness's default.


## `internal/cli/doctor_harness.go`

### authMarkers

> authMarkers are files that suggest a harness config dir has actually been
> initialised, rather than being an empty directory pointed at by mistake.
>
> Their ABSENCE is only ever a WARN: on macOS the credential may live in the
> system keychain instead of the config dir, so a missing marker means "I cannot
> confirm this", never "this is broken".


## `internal/cli/doctor_mcp.go`

### checkMCPServers

> checkMCPServers names the MCP entries that start a LOCAL PROCESS in every
> spawned session.
>
> `mcp_config_path` defaults to `<workdir>/.mcp.json` - a file inside a checkout,
> which the sessions themselves can write - and the harness starts every server
> it declares at MCP init, before any allow/deny rule is consulted. CLA-257
> constrains where that file may send the API key; nothing constrains what it may
> RUN, and nothing said so out loud. This says so.
>
> A WARN, never a FAIL: a local MCP server in a repo's .mcp.json is a normal,
> wanted thing, and failing the preflight over one would block runs that work.
> What an operator needs is to have seen the list once.

### checkOpencodeAmbientConfigs

> checkOpencodeAmbientConfigs reports the opencode config files the driver never
> names but opencode merges anyway, whose `clankerbar` MCP server points at
> another project (CLA-441). The detection - and the reason it WARNS rather than
> failing the run - lives in config.OpencodeAmbientConflicts; this renders it.
>
> The PASS line says nothing about the mechanism: a check that explains a hazard
> nobody has is a line the operator learns to skip past.

### shaping

> The two kinds are counted and remedied SEPARATELY. They share a scan and
> nothing else: one is a file naming the wrong backlog, mitigated for
> spawned sessions by the content pin; the other is a file that shapes
> what the session may do, which the pin does nothing about. A single
> sentence covering both said the mitigation out loud on findings it does
> not apply to (CLA-441 second review).


## `internal/cli/doctor_opencode.go`

### sqliteOpencodeCredentials

> sqliteOpencodeCredentials counts the ACTIVE rows of opencode.db's credential
> table per integration. It selects the integration id, the `active` flag and a
> count only: no column holding a secret is ever read. The database is opened
> read-only. A row whose `active` is 0 is not counted; NULL counts as active.
> The table records no expiry, so a counted credential may still be expired or
> revoked at the provider - presence is not validity, and the check says so.

### checkOpencodeAuth

> checkOpencodeAuth reports on 2.x's credential store when the run spawns
> opencode. A missing store or an empty one is a WARN, not a FAIL: a provider
> key can also come from the provider config (`{env:...}`), which this cannot
> see.
>
> A NON-EMPTY store is not by itself a green (CLA-687). Credentials are stored
> per provider (the `integration_id` column) and a session only works on the
> provider its model names, so a store holding `google` says nothing about a
> config whose models run on `opencode-go`: every session dies with
> `provider.auth` (401) on its first call, and the check used to report PASS
> because it counted rows rather than matching them. The providers are therefore
> taken from the models the run will ACTUALLY use - Config.ModelIDsFor, which
> resolves the phase/tier/panel chain - and compared with the ids the store
> holds. A named provider with no stored credential is a WARN rather than a
> FAIL, for the same reason an empty store is: the key may be declared in the
> opencode config instead, which this cannot read.

### checkOpencodeMCPDisables

> checkOpencodeMCPDisables flags MCP entries in the opencode mcp config files
> that carry no `type`: partial entries such as `"context7": {"enabled":
> false}`, meant to switch off a server the global config declares. Verified
> on 2.0.32 (CLA-675 review), a partial entry in a later layer does NOT stop
> that server, in either dialect (`servers.X: {"disabled": true}` included);
> only a full entry with its `type` and `command`/`url` and the off switch
> does. The adapter cannot repair it either: it moves only typed entries out of
> the 1.x dialect, because a typeless one is not a server it can describe.


## `internal/cli/doctor_pausewindows.go`

### checkPauseWindows

> checkPauseWindows reports the operator's recurring "start no new work"
> schedule and, right now, whether it is holding.
>
> The verdict is always PASS: a configured pause is a setting, not a fault, and
> an invalid schedule never reaches here (config.Validate refuses it, so doctor
> has already stopped at the config FAIL). What this adds is the one thing a
> schedule cannot tell you by reading it — whether the loop would claim work in
> the next minute, and when it will next stop.


## `internal/cli/doctor_permissions.go`

### opencodeEnvOverride

> opencodeEnvOverride finds a declaration of key that would override the
> adapter's own export of it — exec takes the last duplicate key, so ANY
> declared value wins — across all four env levels. On 2.x the key that matters
> is OPENCODE_CONFIG_CONTENT, which carries the fail-closed policy. It
> scans MOST-SPECIFIC-FIRST inside each project's scope (the
> project-per-harness block for this harness, then the project block), then the
> harness block, then top level: SessionEnv composes the child env per key by
> exactly that precedence, so when two levels declare the same name it is the
> more specific one the session actually carries. Naming a layer whose value
> loses the overlay would send the operator to edit the wrong line. Across
> projects the first declaring project is reported — each session carries only
> its own project's layers, so no single entry wins a whole multi-project run.
> The value reported is the declared source — the literal, or the command that
> produces it — never the resolved output.


## `internal/cli/doctor_planner.go`

### doctorplanner.go

> doctor_planner.go — the planner role's preflight (studio-chat.md §2).
>
> D23's "reports that and does not fall back" is enforced here, at the machine
> boundary: the stored document says WHICH harness and model, and this machine
> says whether it can run them. A planner whose harness is missing or
> unconfigured is a FAIL — never a silent substitution of the local default.
>
> The role check is REPORTING plus one aggregate refusal, exactly as the design
> words it: every project whose resolved document has no `roles.planner` is
> named, but the instance is refused only when NONE of them does. One project
> that opted out must not stop the instance serving the others.


## `internal/cli/doctor_power.go`

### unknownSleepRemedy

> unknownSleepRemedy is shared by both ways doctor can fail to learn the sleep
> policy — `pmset -g` not running at all, and running but reporting no
> idle-sleep field. The two states are the same state, so they say the same
> thing; keeping one string means a future edit cannot improve the advice for
> one of them and leave the other behind. It has to be actionable for a reader
> who has just been told the command doctor would send them to did not answer,
> so it names the mitigation as well as the manual check.

### checkPower

> checkPower answers the most basic precondition of an unattended run, and the
> one nothing else checks: will this machine still be awake to do the work?
>
> Timers run on the monotonic clock, which does not advance while a machine is
> suspended. So idle sleep does not pause a run, it FREEZES it — mid-wait,
> silently, in a way that looks exactly like a hang. A real run lost 5h31m of a
> 10h window to this, waking only for 45-second Power Nap bursts.
>
> Two facts decide it: whether anything holds a no-idle-sleep assertion, and what
> the active power source's sleep timeout is. `clankerbar run` now takes an
> assertion itself, so a green line here usually means that is working — but
> doctor is also run standalone, before any loop exists, which is exactly when
> the answer is worth having.

### Informationally identical to the read failing outright

> Informationally identical to the read failing outright: the command
> exited zero, but no idle-sleep field came back, so doctor does not know
> the answer. A renamed, re-cased or locale-shifted field, or a VM whose
> `pmset` omits it, used to render a green line on the exact question this
> check exists to answer. Failing open on the unknown case is worse than
> not checking, because the operator gets a green line telling them not to
> look. Note what the WARN does and does not buy: only `fail` stops
> `doctor && run`, so the documented cron gate opens either way — what
> changes is that a human reading the output has a line worth reading. Do
> not "restore consistency" by making this a FAIL; that would block the
> gate over a machine that may well be fine, which is why the read-failure
> branch above is a WARN too.

### holdsNoIdleSleep

> holdsNoIdleSleep reports whether `pmset -g assertions` shows a live
> PreventUserIdleSystemSleep. Only two line shapes count as evidence, because
> they are the only two pmset actually emits for this assertion (verified against
> live output on 2026-08-22):
>
>   - the summary row: exactly the assertion name followed by an integer count
>     and nothing else — held only when that count is non-zero;
>   - a per-process detail line under "Listed by owning process:", which begins
>     `pid <n>(<proc>):` and names the holder — held whenever one appears.
>
> Anything else mentioning the name is NOT PROVEN HELD and falls through, so
> checkPower goes on to the `pmset -g` settings read rather than reporting PASS
> from a shape it does not recognise. Falling through cannot invent a problem —
> it only defers to the more direct question — whereas the previous test ("the
> last token is not an integer means a detail line") answered YES, held for every
> unrecognised shape, including Apple appending a token to the summary row or a
> locale shifting its number format (CLA-306).
>
> Both branches match structure, not whitespace-split tokens, because splitting
> is where the original guess went wrong: the detail-line regex runs on the raw
> line since pmset prints the holding process's name verbatim inside the parens,
> and a name containing spaces ("Google Chrome Helper") leaves no single field
> carrying the `<n>(<proc>):` tail; the summary row demands exactly two fields
> because any token beyond name-plus-count is an unknown in BOTH directions —
> "1 (inactive)" no less than "0" (CLA-306 review).

### Per-process detail line

> Per-process detail line: `pid 81237(caffeinate): … named: "…"`. Its
> presence means a live process holds the assertion. Matched on the raw
> line so a process name containing spaces or even nested parens
> ("Google Chrome Helper (Renderer)") still reads as one
> `<pid>(<name>):` head; greedy to the LAST `):` is safe because the
> anchored pid head and the assertion-name pre-filter above already
> exclude every other line shape.


## `internal/cli/doctor_prgate.go`

### prGateProbeWorkdirs

> prGateProbeWorkdirs lists the directories whose repositories the delivery
> gate judges, so the preflight exercises the SAME ones. For every configured
> project that is its effective workdir (its own, falling back to the
> top-level one — see projectWorkDir) plus the checkouts its repos map
> declares, because a session starts in a resolved checkout rather than in the
> project root. A config with no projects probes the top-level workdir and its
> declared checkouts. Paths are deduplicated so a project inheriting the
> top-level workdir, or a checkout that is also inside it, is probed once.

### checkPRGate

> checkPRGate reports CLA-310's delivery gate and its prerequisites: the
> driver's verifier reaches GitHub through the `gh` CLI, a delivery whose PR is
> CONFLICTING or carries NO checks is refused when it runs, and — since CLA-776
> — gh's `--json` output must actually be parseable, because the shipped
> GH_FORCE_TTY=0 made gh colour it and every real delivery's PR went unjudged
> for two months.
>
> A missing `gh` is a WARN, not a FAIL — the check degrades to an explicit
> refusal-to-verify and the run carries on, which is the package's fail-open
> discipline — but the operator should see the gate's prerequisite BEFORE the
> first delivery goes out unchecked. The probe's own outcomes are classified by
> what they say about the GATE:
>
>   - output that does not parse is a FAIL: gh ran, and every delivery's PR is
>     unjudgeable now. It is the one fault an operator must fix before a run.
>   - gh cannot reach GitHub (auth, network) is a WARN: the prerequisite is
>     unavailable, not the gate broken, and the delivery path degrades the same
>     way it always did.
>   - there is no pull request to read is a WARN too: the probe did not exercise
>     the parse path, so the check has not validated the thing it exists to
>     validate. Leaving it PASS (with an easily-missed INFO line) would let a
>     pr_gate PASS imply the parse path was tested — the blind spot that let
>     the CLA-776 fault pass doctor for two months.
>
> The probe searches every configured project's effective workdir plus its
> declared checkouts (prGateProbeWorkdirs), because that is what the delivery
> gate judges: probing only cfg.WorkDir missed a project whose checkout lives
> elsewhere and reported its PR untested.


## `internal/cli/doctor_priority.go`

### checkSessionPriority

> checkSessionPriority reports the scheduling priority the run will start its
> harness sessions at.
>
> The dial's whole purpose is INVISIBLE at runtime: a lowered session priority
> shows itself only as CI finishing sooner on a machine something else shares,
> so nothing on the terminal would say whether the dial is on. Both ordinary
> values PASS — a priority is a setting, not a fault — and the line names what
> the value actually does to the tree, because "10" alone does not tell an
> operator which direction 10 goes.
>
> A positive value also needs nice(1) on the daemon's PATH. Without it every
> spawn fails before the harness starts, so doctor FAILs rather than promising a
> priority the run cannot apply; the platform build tag alone cannot see that.


## `internal/cli/doctor_projectroot.go`

### checkRepos

> checkRepos reports, per project (or once for a single-project run), every repo
> the config declares and whether it resolves to a local checkout (CLA-437).
>
> This is the preflight for two silent failures at once. A declared repo whose
> checkout is missing fails every iteration that names it with repo_not_found —
> better seen here than in an overnight log. And a project that declares NO
> repos keeps the legacy workdir behaviour, which is correct but easy to mistake
> for "sessions already start in the task's repo"; saying which mode is live is
> the difference.

### checkProjectRoot

> checkProjectRoot reports on the per-project layout (CLA-678): with
> `project_root` declared, every workdir in the config is a fleet project root
> — <root>/<project slug>, holding that project's clones under
> <project root>/<repo name> and its worktrees under
> <project root>/worktrees — and this check FAILS when the layout does not
> hold, because every one of those facts is load-bearing at run time:
>
>   - a configured repo resolving OUTSIDE the project root is a session
>     reading a tree its permission scope does not cover, or one the fleet
>     will try to clone over;
>   - a clone whose origin names a different repo than the identity it sits
>     under is the wrong-repo read the layout exists to prevent (the same
>     attribution rule the supervisor's derivation enforces, checked here
>     before a daemon is refused at spawn time);
>   - a missing or outside-the-root worktrees directory is where a task
>     worktree fails to be created, which costs an iteration its whole session.
>
> A config that does not declare `project_root` gets no check: there the
> workdir is whatever the operator pointed it at, and its repos are wherever
> the `repos` map says, by design.

### The project root and everything the layout puts under it are checked o

> The project root and everything the layout puts under it are checked on
> CANONICAL paths: a lexical comparison says <root>/worktrees is inside
> <root> even when that name is a symlink to a directory on another disk.
> The supervisor refuses a symlink at any of these paths (see
> supervisor.DeriveWorkdir), so doctor must fail for the same layout rather
> than pass a config the derivation will refuse at reconcile time.

### Two FULL identities that end in the same name derive the SAME director

> Two FULL identities that end in the same name derive the SAME directory
> (<project root>/<name>), so only one of them can ever be the clone there
> and the other reads as a wrong-origin checkout. Name the conflict here,
> where the operator can fix the config, rather than letting the layout
> report the symptom. A bare key beside a full identity of the same name is
> NOT a conflict: it is that repo spelled short, and the derivation
> materializes the full identity (the only spelling with a clone URL).


## `internal/cli/doctor_routewindows.go`

### checkRouteWindows

> checkRouteWindows reports every route carrying a pricing schedule
> (CLA-947): its schedule's windows, whether it is blocking right now, and when
> it next will. It also WARNS on a `route_windows` key that names no route the
> config can run — a typo must be visible, but a config that lists a route from
> a paused experiment must still start, so this is not a config FAIL.
>
> The verdict is PASS (or WARN for an unmatched id) by design: a configured
> schedule is a setting, not a fault, and an invalid one never reaches here
> (config.Validate refuses it).


## `internal/cli/doctor_runconfig.go`

### checkRunConfigs

> checkRunConfigs reports, per target, WHICH execution config the loop will run
> under — the local file, or a stored plane document overlaid on it — plus any
> machine-fit warnings about the STORED dials. The plane validates shape; only
> this machine can see whether a dial it ratified can actually fire (a turn cap
> under opencode, a cost ceiling on a harness that never reports cost), which is
> the validation split the design memo draws.
>
> A stored STEP this machine cannot spawn is the one refusal that
> earns FAIL: the document is ratified policy that silently will not
> run here, and the operator has to know before an unattended drain
> quietly falls back to local rules instead (CLA-598). That FAIL is
> the point — `doctor && run` refuses to start on such a document.
> Every OTHER refusal stays WARN, because plane trouble must not make
> `doctor && run` unusable (applyRunConfig's own posture).


## `internal/cli/doctor_session.go`

### checkSessions

> checkSessions inspects the directory each project's sessions are spawned in.
> ONE CHECK PER PROJECT, for the same reason the backlog check is per project: a
> multi-project instance can have one queue pointed somewhere that teaches its
> sessions nothing while the others are fine, and an aggregate line would hide
> exactly that.
>
> The failure this exists for is silent and expensive rather than loud. A
> harness reads its instruction file, its skills and its project settings from
> the session's cwd AND UPWARD — never from repos below it. Spawn in a multi-repo
> parent and every session starts with no protocol, no conventions and no
> permissions beyond the ambient ones, then spends its opening minutes
> rediscovering the layout. It still works, which is why nobody notices for
> thirty iterations.
> checkSessions reports on each session the loop will spawn: its workdir, and
> what the harness running there makes of the MCP config it will be handed.
>
> One check per project PER HARNESS (CLA-366), because both axes change the
> answer: the project decides which file (its `/mcp/<slug>`) and the harness
> decides what that file MEANS — a Claude-shaped `.mcp.json` is the session's
> tools for one adapter and a refusal to start for another. Qualified names only
> when a second harness is in play, so a single-harness report is unchanged.

### projectWorkDir

> projectWorkDir and projectMCPConfig resolve a project entry EXACTLY as
> loop.Driver.invocation does — the project's own value, falling back to the
> top-level one. Doctor answering a differently-resolved question is the whole
> failure mode it exists to prevent: read p.WorkDir raw and an empty entry
> resolves to the current directory, so a config like
>
> 	{"workdir":"~/dev","projects":[{"slug":"acme"}]}
>
> would be diagnosed against wherever the operator happened to run doctor. That
> never FAILs (a cwd always resolves), so it reports green for a directory the
> loop will never use — the dangerous direction.

### mcpConfigIsClaudeShaped

> mcpConfigIsClaudeShaped reports whether the file at path declares Claude's
> `mcpServers` block - the shape config.Validate auto-discovers from a workdir's
> `.mcp.json`.
>
> It answers only on POSITIVE evidence. An empty path, an unreadable file or one
> that will not parse all return false, because each of those is already someone
> else's check (config.Validate fails closed on an unparseable MCP config) and a
> preflight that guesses at a file it could not read is how this arm went wrong in
> the first place.

### The .mcp.json arm, and what the configured harness makes of that file

> The `.mcp.json` arm, and what the configured harness makes of that file.
>
> The question this used to ask was "does the adapter HAND the path to the
> session", and that question certifies opencode, which does hand it over and
> then dies on it. The question it asks now is the one the check actually rests
> on: does the file MEAN anything to this harness. Only the adapter knows, so
> only the adapter is asked (harness.Adapter.MCPConfigUse) - see CLA-263.

### When a path IS configured, the file is the statement - and doctor can

> When a path IS configured, the file is the statement - and doctor can
> now read the opencode schema well enough to verify the statement says
> clankerbar (CLA-448). The old "not checked" caveat existed because the
> schema was unreadable; the opencode file is JSON like any other, and a
> configured file that is silent about clankerbar is exactly the
> tool-less-session config that burned CLA-351 and CLA-377 to parked.
> FAIL, not WARN: a green workdir must not certify a session that cannot
> see the backlog.

### A session with no .mcp.json reaching it gets no clankerbar tools at al

> A session with no .mcp.json reaching it gets no clankerbar tools at all — it
> starts, burns tokens, and cannot see the backlog. config.Validate has already
> defaulted this to <workdir>/.mcp.json when that file exists, so an empty value
> here means none was found and none was configured.
>
> This is NOT gated on the multi-repo-parent shape: a plain single-repo workdir
> with no .mcp.json blinds its sessions just as completely. `parent` only
> selects the wording, because that is the case an operator is most likely to
> have reached by accident.


## `internal/cli/doctor_sessionenv.go`

### requiredSessionEnv

> requiredSessionEnv is the minimum a triggered allowlist must keep (CLA-714).
> A session without PATH cannot run a command, without HOME cannot read git's
> or ssh's configuration, and without CLANKERBAR_API_KEY cannot reach the
> plane (the MCP config carries it as `Bearer {env:CLANKERBAR_API_KEY}`).
> CLANKERBAR_WORKDIR_ROOT joins them while the workdir derivation is on: the
> fleet's layout is what every brief names, and `doctor` has no way to tell an
> operator's shell from a supervisor's without the one signal both carry
> (`project_root` on a materialized config, the variable in a base config's
> environment).

### checkSessionEnvAllowlist

> checkSessionEnvAllowlist reports the shape in force for every (project,
> harness) pair the run will spawn (CLA-714). With no allowlist declared it
> PASSes and says so: full inheritance is the documented default, not a
> warning. With one declared it names the variables that will reach the
> session - names only, never values - and FAILs when the list omits one of
> requiredSessionEnv, because that failure otherwise appears later as an
> unexplained repo_not_found or an unauthenticated plane call inside a session.
>
> It asks SpawnedHarnesses, not PhaseHarnesses, for the reason the two exist:
> PhaseHarnesses seeds the run-wide `harness` unconditionally, which is right
> for "is every declared harness registered and usable" but wrong for "what is
> true of the sessions this run actually runs". A config whose every phase
> names its own harness never spawns the run-wide one, and this check is about
> what a session INHERITS - so reporting (and FAILing) that harness's scope
> would send the operator to fix a harness no session ever runs on, and a false
> FAIL blocks a run that works. Same doctrine as runsHarness and
> costBlindHarnesses in the budget check.

### checkSessionEnv

> checkSessionEnv verifies every DECLARED env entry that needs resolving can be
> resolved right now (CLA-462): each fromCommand is executed and each "@path"
> literal is re-read under its owner-only rule. Outputs are discarded — doctor
> reports the verdict and the variable's name, never a credential. One FAIL
> here means every spawn that overlay reaches will refuse until it is fixed,
> so seeing it in the preflight is the cheap copy of the same fact.
>
> Plain literals need no check and get none: there is nothing to resolve.

### checkTokenSources

> checkTokenSources warns when a scope whose repos PUSH has declared no source
> for GH_TOKEN anywhere its sessions' overlays look — top level, any harness
> block, or the project's own two blocks (CLA-462). This is exactly the shape
> of the 2026-08-24 incident: a daemon launched without the wrapper spawned
> sessions with no token, which died later at `git push` with an error naming
> nothing. A WARN, not a FAIL: a repo may push over ssh or not at all, and the
> operator owns that call — the heuristic (repos declared = pushes happen) is
> only allowed to speak up, not to stop the run.


## `internal/cli/doctor_setup.go`

### setupSkillSection

> The served `clankerbar-setup` skill (`clankerbar.com/skills/clankerbar-setup.md`)
> is doctor's remediation half: doctor says WHAT is wrong, and the skill's
> numbered sections say how the operator's agent fixes it (CLA-197). A
> setup-precondition remedy therefore names the section it means — the workdir
> check's missing agent-instructions file points at section 1, and the toolchains
> check's absent or denied grants point at section 2.
>
> The numbers belong to the SKILL, not to this package: a renumbering there makes
> these pointers stale, so the skill stays the source of truth and the remedy is a
> pointer rather than a paraphrase of the fix. The two checks the skill
> deliberately has no section for — the queue gated behind the operator's
> unanswered questions, and whether the machine will stay awake — carry no
> section here either, because neither is an agent's to fix. Every other check
> diagnoses the machine wiring around the loop (harnesses, credentials, state,
> delivery, budgets) and has no counterpart among the skill's five structural
> preconditions, so its remedy stays in doctor's own words.


## `internal/cli/doctor_statedir.go`

### WHERE the state dir sits is a capability question, not a tidiness one

> WHERE the state dir sits is a capability question, not a tidiness one: STOP
> and HALT live there, and a session may write anywhere under the workdir it is
> spawned in. A state dir inside one hands every spawned session the daemon's
> own stop switch. CLA-259 moved the DEFAULT out; an explicit state_dir still
> wins, including one pointed back inside, so the operators this catches are
> exactly the ones the new default never reached.

### workDirMatch

> workDirMatch is the configured workdir a state dir was found at or under. dir
> is "" when it is outside every one of them.
>
> session says whether the loop actually SPAWNS sessions in that directory, and
> the two cases cost different things - see stateDirInWorkDir. Reporting the
> wrong one is not a wording detail: telling an operator a session can write
> their state dir when no session runs there is a claim they can check and
> disprove, which is how a warning gets skimmed past.
> outer is the OUTERMOST containing workdir, which is what a remedy has to name:
> every match is an ancestor of the same state dir, so they form a chain, and
> "point it outside ~/dev/acme" is satisfied by ~/dev/state - which warns again.

### containingWorkDir

> containingWorkDir finds the configured workdir the state dir sits at or under.
>
> EVERY configured workdir is considered, not just the ones sessions run in: the
> top-level `workdir` and each project's (resolved with the same fallback the
> loop uses). A `projects[]` entry that omits `workdir` inherits the top-level
> one, so a state dir inside it is a capability waiting for the next config edit
> even when nothing is spawned there today.
>
> A session workdir wins over a merely-configured one, and the DEEPEST match wins
> within each group: for `workdir: ~/dev` with `projects[0].workdir: ~/dev/acme`,
> the answer an operator can act on is ~/dev/acme, the directory their sessions
> are really in.
>
> Both a lexical and a symlink-resolved comparison are made, and either one
> counts. The lexical pass is what an operator can check against their own config
> by eye; the resolved pass catches the case where two different-looking paths are
> the same directory - /var vs /private/var on macOS, or a workdir reached through
> a symlinked parent. Resolution is best-effort: an unresolvable path falls back
> to its lexical form rather than dropping the comparison. Both passes are
> CASE-SENSITIVE, so on a case-insensitive filesystem `~/Dev` against `~/dev` is a
> known miss - the same lexical assumption config and statedir already make, and
> a miss here costs a warning rather than a guarantee.

### stateDirInWorkDir

> stateDirInWorkDir words the in-workdir warning. Two populations, because they
> are owed different sentences: a SESSION workdir is a live capability, while a
> configured-but-unused one is a trap set for whoever next adds a `projects[]`
> entry without a workdir of its own.
>
> The remedy is the half that has to survive being FOLLOWED, so it is checked
> against the config rather than assumed. "Remove state_dir" is only offered when
> the default would actually land outside - a workdir of ~ contains
> ~/.local/state, so for those operators removing it changes the path and not the
> warning - and it names the OUTERMOST containing workdir, since moving just
> outside the innermost one lands in the next one out.

### configuredWorkDirs

> configuredWorkDirs is every directory this config names as a workdir, absolute,
> cleaned and de-duplicated.
>
> An empty value is resolved rather than skipped, matching config.absWorkDir and
> config.SessionWorkDirs: an unset workdir means the cwd everywhere else in the
> tool, and skipping it here would make the containment check silently answer
> about fewer directories than the loop uses.


## `internal/cli/doctor_stranded.go`

### checkStranded

> checkStranded reports commits that exist only on this machine: reachable from
> a local branch or a worktree HEAD (including detached ones), and from no
> remote-tracking ref.
>
> The failure this exists for is silent and irreversible, not loud and
> expensive: on 2026-08-28 three commits were found that existed in exactly one
> place on earth — local `main` in one checkout, on no remote branch and no
> other local branch. They had been committed on a permanent branch the
> workflow forbids (so they were unpushable) and in a worktree under
> /private/tmp, which macOS sweeps; when the tree went, the only surviving
> pointer was a local ref nobody looks at. The causes vary — a swept temp dir,
> an abandoned worktree, a stale checkout, an iteration that died mid-flight —
> and they all have one signature: commits reachable from a local ref and from
> no remote ref.
>
> This check is deliberately inert: it reports and recommends, and never
> deletes, moves, pushes or rebases. A clean worktree is not evidence of
> abandonment, and a live holder may be mid-task, so acting on someone else's
> tree is dangerous — a report that surfaces the work and does nothing is safe
> precisely because it does nothing.

### strandedRepos

> strandedRepos lists the repositories the stranded check reports on: every
> checkout the config resolves to for each project — the declared repos
> (CLA-437), and the checkouts under the project's workdir.
>
> The workdir scan is scoped to the project's own repos when a slug is
> configured: a multi-repo parent holds checkouts that belong to other
> projects, and warning about every one of them is how a WARN line becomes
> something an operator skims. The project's repos are taken to be the ones
> named after its slug — the same deliberate under-reporting direction as
> detectToolchains: a missed report is a one-off, a wall of irrelevant
> warnings buries the one that mattered.

### delivery.Repos returns working-tree dirs deduplicated by repository, s

> delivery.Repos returns working-tree dirs deduplicated by repository, so
> linked worktrees never double-report. The declared checkouts can collide
> with the scan the other way round — a declared path that is a WORKTREE of
> a repo the scan also found: two paths, one repository. Dedupe the
> combined list by the shared git directory, the same identity
> candidateRepos uses, so a repository is checked once no matter how many
> of its trees the config names.

### strandedLine

> strandedLine is one report line for a repo: a finding naming a tip that
> reaches stranded commits, or an error line naming a git read that failed.
> err lines are prefixed with the repo path by the caller even in single-repo
> mode, so the repo the remedy points at ("run the failing git command by hand
> in the repo named above") is actually named; finding lines stay bare, the
> ref or worktree path being the handle a human rescues by.

### strandedFindings

> strandedFindings returns the lines to report for one repository — a finding
> per tip that reaches a commit no remote-tracking ref reaches, and an error
> line per git read that failed — and whether any line is a REAL finding
> (commits established to exist only on this machine).
>
> A finding names the ref (a branch) or the worktree path (a detached HEAD —
> the only handle a detached commit has), how many commits are stranded, and
> the newest one's subject and date: the three facts a human needs to tell live
> work from debris at a glance.

### parseStrandedWorktrees

> parseStrandedWorktrees reads `git worktree list --porcelain`: blank-line-
> separated blocks, each led by `worktree <path>` with `HEAD <sha>` and, when a
> branch is checked out, `branch refs/heads/<name>`. A `detached` block has no
> branch line and is exactly the tip no branch names. Bare blocks (no working
> tree) are dropped: their HEAD is a branch, so the branch enumeration already
> covers it.


## `internal/cli/doctor_toolchain.go`

### checkToolchains

> checkToolchains cross-references the build tools the backlog's repos need
> against what the permission policy actually grants.
>
> A headless claude session FAILS CLOSED: anything the policy does not name is
> refused outright, with no prompt reaching the operator. So a toolchain the
> repos need but the allowlist never mentions does not stop a run — it produces
> work that CANNOT BE VERIFIED, which is worse. One overnight run wrote a whole
> Go subcommand, pushed it, and re-discovered the same wall on three separate
> iterations because nothing checked this up front.
>
> Every finding here is a WARN, never a FAIL: doctor reads the settings files it
> knows about, and a grant can also arrive by a rule form it does not parse or a
> flag on the harness invocation. A false FAIL would block a run that works.

### runsHarness

> runsHarness reports whether the named harness will actually START A SESSION in
> this config. SpawnedHarnesses, not PhaseHarnesses: a run-wide `harness` that
> every phase overrides is declared and never spawned, and a check reasoning
> about what the run DOES must not count it.

### costBlindHarnesses

> costBlindHarnesses names every harness this run spawns that cannot report what
> a session cost, in the order SpawnedHarnesses lists them. Empty means a cost
> ceiling can see the whole run.
>
> SPAWNED, not declared: a never-spawned cost-blind harness would make the run's
> live `max_cost_usd` report as INERT and send the operator to fix a harness no
> session ever runs on.

### detectToolchains

> detectToolchains looks in each session workdir and, when that workdir is a
> multi-repo parent, in the checkouts under it that belong to the project.
>
> Scope is the whole difficulty here. A session rooted in a multi-repo parent
> COULD be sent into any repo below it, but only the project's own repos are
> plausibly in its backlog — and warning about every unrelated checkout on the
> machine is how a WARN line becomes something an operator skims. The project's
> repos are taken to be the ones named after its slug (`clankerbar` covers
> `clankerbar/` and `clankerbar-cli/`), which under-reports for a project whose
> repos are named differently. That direction is deliberate: a missed grant
> surfaces as one refused command, while a wall of irrelevant warnings buries the
> one that mattered.

### policySettingsPaths

> policySettingsPaths lists every settings file Claude will MERGE for these
> sessions, in no particular order — auditing only the drain policy would report
> a tool as ungranted when another layer already allows it.
>
> The session's cwd matters as much as the config dir: `<workdir>/.claude/settings.json`
> and its `.local` sibling are the project layer, and are where an operator is
> most likely to have put `Bash(go test:*)` in the first place.

### grantedCommands

> grantedCommands returns, per Bash command head, whether the policy grants it
> and whether it denies it OUTRIGHT.
>
> "Outright" is the load-bearing word. Deny wins over allow in Claude's policy,
> but only for what the deny rule actually covers: a narrow `Bash(go run:*)` deny
> alongside a `Bash(go test:*)` allow is a normal, careful setup, and reporting
> that as "go is denied, tasks in those repos cannot be verified" would be flatly
> wrong. So only a BARE head deny (`Bash(go)` / `Bash(go:*)`) counts as decisive;
> a verb-qualified deny is recorded separately and merely narrows.

