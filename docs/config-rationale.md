# Config Rationale

Configuration: defaults, layering, validation, prompts, budgets and endpoint/trust rules.

This file holds the design and process reasoning retired from the code comments in
`internal/config` when those comments were cut to the one-to-three-line rule (CLA-809). The
comments beside the code keep the invariant, trap or constraint an editor needs at
that site; the fuller rationale - why a default is the number it is, how a
failure was measured, which alternative was rejected - lives here. Task refs and
dates are provenance for the reasoning, not a changelog.

Each section is one source file. Within it, a heading names the declaration the
comments belonged to, or quotes the first line when they were a free-standing
section note.


## `internal/config/budget.go`

### PerHarness

> PerHarness are optional per-harness spend ceilings, keyed by harness name,
> each counted over ONLY that harness's own sessions (CLA-367).
>
> The dials above are one ceiling over every session a run spends, which is
> the right shape while a run drives one harness and the wrong one the moment
> it drives two. `max_tokens` is calibrated against a subscription plan — 75M
> is a sane week of Claude and roughly $2 on a DeepSeek-class backend, which
> would end a drain after a task or two. `max_cost_usd` is the meaningful dial
> for a metered backend and a meaningless one for a session billed to a
> subscription, which reports a price nobody is charged (CLA-289). Neither
> dial can hold a number that means the same thing on both sides, so a
> mixed-harness run gets one block per harness, each measured in the unit that
> harness understands.
>
> Any block that trips stops the WHOLE run. These are circuit breakers, not
> per-harness quotas to be juggled: a run that carried on with one harness
> switched off would be a shape nobody configured, and the phase that needed
> it would fail every iteration.
>
> The dials above keep working exactly as they always have, over the run's
> whole spend, and a config setting none of these behaves byte for byte as
> before. Set both and both apply — the global ceiling over everything, the
> per-harness one over its own sessions, whichever is reached first.
>
> Wall clock is deliberately absent: it measures the run, not a harness, and a
> run has one clock however many harnesses it spends it on. So is
> max_session_tokens, which is a runaway detector on a single session rather
> than an accumulating ceiling.

### ExceededBy

> ExceededBy names the dimension of this block that tripped, or "" if none has.
>
> The harness name leads the string for the reason Budget.ExceededBy names the
> dimension at all: in a run carrying two ceilings, "cost $2.05 ≥ $2.00" does not
> say which side stopped it.

### CountsSpendFor

> CountsSpendFor reports whether a session run on this harness is under a spend
> ceiling — its own block's, or the run-wide one.
>
> This is the question CLA-262's side effects turn on (see CountsSpend): a
> session whose spend cannot be measured breaks a promise only where a promise
> was made. In a mixed-harness run the promise follows the harness whose breaker
> is set, so an unreadable opencode session stops a run carrying an opencode
> block while an unreadable claude session does not — unless a global dial covers
> them both, which is what every pre-CLA-367 config has.

### sessionTokenCeilingMultiplier

> sessionTokenCeilingMultiplier derives the ceiling from budget.max_tokens
> when the operator set one. 2x is defensible because the runaway this
> exists to stop ran 3.8x its run's ceiling: a detector at 2x catches that
> class of runaway while sitting far above what a large task can plausibly
> cost (~66M measured, CLA-309). It is a DETECTOR, deliberately not a
> tighter budget.

### SessionTokenCeilingFor

> SessionTokenCeilingFor resolves the per-session runaway ceiling for a session
> on this harness: the operator's own dial, else 2x the run's max_tokens, else
> 2x that harness's own per_harness max_tokens, else the floor.
>
> The per-harness rung exists because CLA-367 tells an operator to move claude's
> token ceiling out of the global dial and into its own block — and without it,
> doing exactly that would silently LOOSEN the runaway detector the global dial
> was deriving: per_harness.claude.max_tokens=20M would give a 150M floor rather
> than the 40M the old shape gave, 7.5x the run's own ceiling. A detector that
> gets weaker when you follow the documented migration is the CLA-343 bug
> reappearing by another door.
>
> The global dial still wins over the per-harness one where both are set: it
> bounds the whole run, so it is the tighter promise about any one session.

### SessionTokenCeiling

> SessionTokenCeiling resolves the per-session runaway ceiling: the operator's
> own dial, else 2x the run's max_tokens, else the floor. There is deliberately
> no "disabled": the whole point of CLA-343 is that nothing was able to stop the
> 285.9M session, so a ceiling that can be left unset by accident is the bug.
>
> This is the harness-blind form, kept for callers with no harness in hand; a
> caller that knows which harness the session runs on asks
> SessionTokenCeilingFor, which can also see a per-harness token ceiling.

### CountsSpend

> CountsSpend reports whether this budget bounds SPEND — tokens or dollars — as
> opposed to only the wall clock.
>
> The distinction matters when a session's spend cannot be measured at all (a
> stream the supervisor could not read whole, CLA-262): a run under a spend
> ceiling has been promised something the driver can no longer deliver, while a
> run under a wall-clock ceiling alone still has its ceiling intact, because the
> clock does not depend on anything the child said.

### ExceededBy

> ExceededBy names the dimension that tripped, or "" if none has.
>
> Which dial stopped a run is the first thing an operator asks, and reporting
> all three figures side by side answers it wrongly: a run under a wall-clock
> ceiling alone still prints a token count and a dollar figure, which read as the
> cause. Naming the dimension — and the ceiling it crossed — is the difference
> between "why did it stop at $148" and "it ran for 10h23m against an 8h cap".

### Remaining

> Remaining is how much of the wall-clock ceiling is left after elapsed, and
> whether a ceiling is set at all.
>
> Callers must pass the SAME elapsed the breaker is given (ExceededBy), so a
> decision taken mid-drain cannot disagree with the breaker's own verdict
> between drains. Deriving a wall-clock deadline is what let them disagree:
> Deadline keeps start's monotonic reading while ExceededBy counts monotonic
> elapsed, and a suspended machine advances the one and freezes the other.


## `internal/config/config.go`

### defaultPrompt

> defaultPrompt asks each session for exactly ONE task.
>
> The wording is not casual: the served protocol
> (clankerbar.com/skills/clankerbar.md) defines "work the next backlog item" as
> "means exactly one - run the loop once, finish that task, and stop", against
> "work the backlog", which it defines as drain the whole ready queue. This
> string is read by an agent that has read that document, so a paraphrase is a
> behaviour change. It is pinned by TestDefaultPromptAsksForOneTask, which is
> there because the previous default was the DRAIN phrase and nothing failed.

### DefaultMaxTurns

> DefaultMaxTurns bounds a session that sets no phase cap and no top-level cap.
>
> Calibrated against the two measured anchors from the CLA-343 audit: the
> largest legitimate session on record ran 370 turns (CLA-309, 66.2M tokens),
> and the runaway this exists to stop ran 1093 turns (285.9M tokens). 400 sits
> between them — above the largest legitimate session with room to spare (8%
> headroom is deliberate, not generous, because the cost of a false positive is
> a scruffy salvage commit and a re-queued task), and at less than half the
> runaway. It is a RUNAWAY DETECTOR, not a budget: the salvage (CLA-314)
> commits and pushes whatever a cut-off session left, so a hit costs a scruffy
> commit, not work.

### DefaultMaxZeroSpendAttempts

> DefaultMaxZeroSpendAttempts bounds consecutive attempts within one drain that
> ended without the harness reporting ANY usage (see Config.MaxZeroSpendAttempts).
>
> 3 is deliberately small, and small is the point: a spend ceiling cannot see an
> attempt that reported nothing, so every one of these is a paid session the
> breaker is blind to, and the loop's other backstop - `max_retries: 0`, never
> give up - is exactly the setting that turns three of them into three thousand.
>
> What actually reaches this bound is narrower than it sounds, and worth naming
> so nobody tunes it against the wrong failure: an attempt has to be classified
> TRANSIENT (anything else ends the phase on the first one) and die before its
> harness accounts for anything - a stream that never gets past the handshake, a
> connection reset early in the session, an overloaded API turning it away. A
> real blip that reaches the model is followed by a session that reports, which
> resets the count, so the ladder an operator tuned `retry_cap` for is untouched;
> and a usage-limit pause is counted neither way (see loop.drainPhase). Raise it
> if a harness of yours legitimately dies silently more often than that.

### DefaultMaxRepeatedToolFailures

> DefaultMaxRepeatedToolFailures is how many CONSECUTIVE calls of one tool may
> return the same error before the harness kills the session (see
> Config.MaxRepeatedToolFailures, CLA-739).
>
> 5 because the measured loop is nothing like the ordinary shape. The incident
> sessions made 65 to 316 identical refused calls each; a working session that
> trips over the same refusal retries it once or twice and moves on. And the
> largest legitimate run of identical failures in that incident's own logs was
> 4: a session refused `external_directory` on four shell calls in a row, which
> then recovered and carried on. So 5 sits just above what a session that is
> still working does, and every call past it re-sends the whole context.

### VerifyPhaseName

> VerifyPhaseName is the fix-verification round (CLA-638): it re-reviews the
> fix delta, adjudicates the prior round's open MUSTs by their plane finding
> ids, and carries the terminal step only when no MUST remains open. It is a
> review-shaped phase — it writes a sealed durable review record like the
> review gate — and it must be the LAST phase of the sequence, because the
> driver repeats the fix→verify pair after it until a round finds no open
> MUST or the round cap parks the task.

### ReviewRoundCap

> ReviewRoundCap is the hard round cap per review cycle (B8): a human verdict
> starts a new cycle, a takeover or reclaim does not, and a void round does not
> count. The plane derives the count from its non-void records and serves it as
> `reviews.counted`; the driver refuses to spawn another fix after the third
> round. It is a constant rather than a config knob on purpose: "never one more
> round" is the stopping rule, not a tuning dial, and a config value of 0 would
> be a sequence with no bound at all.

### Role

> Role is the role THIS instance serves (studio-chat.md §2, decision
> 96ac5e8d): empty/"worker" drains the backlog, "planner" runs the
> conversation loop instead. It is a MACHINE fact — which roles this
> machine serves — so it stays in the local file and rides the presence
> beacon; the plane never decides it.
>
> `omitempty` is load-bearing for the same reason as ProjectRoot: the
> config's marshalled form is its identity, and an absent key must mean
> worker without changing the identity of every existing daemon.

### Models

> Models is the operator's own tier map: bucket name -> harness-specific model
> alias, e.g. {"strong": "opus", "standard": "sonnet", "cheap": "haiku"}. A
> phase names a BUCKET (Phase.Tier), never an alias.
>
> The indirection is the whole point. clankerbar drives three harnesses and
> cannot know which models an operator has, nor which is cheaper — that would
> need a price table, which goes stale silently and in the direction that
> costs money. So it never learns what models exist; it only learns that the
> operator bucketed them, and ships a policy over the buckets.
>
> Everything here is OPTIONAL and every path falls back rather than refusing:
> no map, an unnamed bucket, or a bucket mapped to a blank string all resolve
> to Model, and Model empty resolves to the harness default. A mistyped tier
> therefore costs a line in the iteration log, not a stopped run.

### Harnesses

> Harnesses carries the per-harness invocation fields for a run whose phases
> are not all on one harness (CLA-366): harness name -> HarnessConfig. Empty
> on every single-harness config, which is every config written before this
> existed.
>
> Keyed by harness NAME, deliberately, so it composes with the other
> per-harness maps rather than nesting inside one of them — the budget's own
> per-harness breakers are keyed the same way, and neither has to know about
> the other.
>
> A block for the TOP-LEVEL harness is optional and its gaps are filled from
> the run-wide twins (Model, Models, ConfigDir, MCPConfigPath, SettingsPath),
> because those fields have always described that harness. A block for any
> OTHER harness is required and is filled from nothing — see HarnessConfig
> for why inheriting a harness dialect across harnesses is worse than an
> empty field.

### Prompt

> Prompt is what each fresh session is asked to do. Default: work ONE task.
> It is not a per-task prompt - the backlog is the source of tasks - but it
> does decide how much a session takes on, and the served protocol reads the
> wording precisely: "work the next backlog item" means exactly one task,
> while "work the backlog" means drain the whole ready queue in that session.
>
> One task is the default for two reasons, and only the second is unconditional.
>
> Context: each session starts fresh, so per-session cost stays bounded however
> long the queue is, where a draining session CAN accumulate every task it
> touches into one context. "Can", not "does" - a repo whose agent guide tells
> the session to dispatch each task to a subagent keeps the orchestrator thin,
> so this argument is contingent on the target repo's own guidance.
>
> Control latency: this one always holds. The console pause, HALT,
> max_iterations and the budget breaker are all evaluated at the top of
> loop.Run's iteration, and none of them can interrupt a session that is
> already running (STOP is additionally honoured inside waitOrStop, but only
> while the driver is between or backing off from sessions, never mid-session).
> So the iteration IS the operator's control granularity: one task per
> iteration means a pause takes effect after the current task instead of after
> the whole queue.
>
> The trade being bought: a session's fixed startup cost - re-reading the
> served protocol and the repo's agent guide - is now paid per task rather
> than amortised across a drain.

### Phases

> Phases splits ONE task across several sessions, so its context resets at a
> checkpoint the model cannot reach itself. See Phase for why this exists.
>
> OPT-IN, and deliberately so. Left empty, a task runs exactly as it always
> has: one session, asked Prompt. Two or more phases mean a task that reaches
> its checkpoint and stops there is only HALF finished until the next session
> runs, so switching it on before the sequence has been watched end to end
> would turn a driver bug into tasks that silently never reach review. An
> operator opts in per config, and the shipped briefs are one line each:
>
> 	"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}]
>
> The three-phase shape is the shipped one: `review` with a phase after it is
> the REVIEW-ONLY gate (it records findings and changes nothing), and `fix`
> is the last phase and owns the PR plus in_review. A FINAL `review` keeps the
> historical review-and-fix brief, so an existing two-phase config still
> finishes its tasks; the mode is chosen by position, not by a second name.
>
> Naming a phase and leaving its prompt empty takes the built-in brief for
> that name; set `prompt` to override it, and `max_turns` to cap it.
>
> Phases and Prompt are mutually exclusive — Validate refuses both, rather
> than silently preferring one and leaving an operator to wonder which of
> two prompts their sessions are actually being handed.

### Conflict

> Conflict is the supervisor's conflict-only session step (CLA-759): the
> harness and model for the short session it starts when CLA-758's
> deterministic close-out stands down on a CONFLICTING approved task. It is
> NOT a phase — it runs outside any task's sequence, for one task the
> supervisor picked, at most one at a time fleet-wide — so it is its own
> block rather than another entry in `phases`.
>
> ABSENT/partial = the fleet's IMPLEMENT step's harness and model, which is
> the documented default: resolving a conflict is implementation work that
> wants the model already trusted with the task's code. See ConflictStep.
>
> A pointer so an unset block is omitted from Identity(), exactly as
> ReviewPanel's `omitempty` keeps an untouched config's identity unchanged.

### ReviewPanel

> ReviewPanel is the ordered list of independent reviewers a built-in review
> phase expands into (CLA-639). There is NO default and NO preset: the CLI
> cannot know which harnesses, providers or models an operator has, so it
> never names one (decision a0b1373b).
>
> ABSENT = one reviewer, exactly as before this feature: the config's own
> review phase runs once, on the harness/model the config already gives it,
> with the historical brief, the historical `review-1` role and the legacy
> single-review fix path — byte for byte.
>
> SET = those members, and only those: any number, the same model allowed
> more than once (each entry is an independent run). A member's `name` is
> only its log label; the reviewer role the durable record is keyed on is
> positional (`review-1`..`review-N`, counted over the review-shaped phases),
> so naming a member never changes which record it writes. The panel applies
> to a BUILT-IN review phase (no prompt of its own) that is not the last
> phase — the read-only gate. A final review phase (the historical
> review-and-fix brief) never expands, and a review phase with its own prompt
> owns its own contract; Validate refuses the combination rather than letting
> the panel silently not apply.
>
> An explicitly EMPTY array (`"review_panel": []`) is a half-edited config
> rather than an opt-out, and Validate refuses it — the opt-out is to remove
> the key. Read through Config.EffectivePanel.
>
> The panel hangs off the review PHASE, not the sequence, because that is
> what it multiplies: `phases` still reads [implement, review, fix], the
> config still has one review, and the driver is what fans it out.
>
> INTERIM placement (B10, A7): the panel is local config because phases are,
> until CLA-598 moves the sequence onto the plane and the state-machine
> reshape (CLA-454) carries panel membership as plane phase state. Nothing
> here should be read as the durable shape — the durable review RECORD is
> the plane's (CLA-636); this is only which reviewers run.

### ReviewPanelConcurrency

> ReviewPanelConcurrency caps how many of a review panel's members run AT
> ONCE (CLA-697). The members are independent by design, so the default is
> all of them: 0 (or an absent key) means "the whole panel", and a value
> larger than the panel means the same. 1 is the historical behaviour — the
> members run one after another in the task's own worktree — and is the
> fallback for an operator who wants the old sequence, or whose machine or
> provider cannot take N sessions at once.
>
> Each concurrent member gets its own DETACHED worktree at the reviewed tip
> (created and removed by the driver), because members may run builds or
> probes: one shared checkout would let two reviewers write the same file.
> A member's detached worktree is discarded when it ends — there is nothing
> to salvage from a review-only session, and the fix phase owns the real
> worktree.
>
> Read through Config.EffectivePanelConcurrency. It only applies when
> `review_panel` expands; a single reviewer never fans out.

### reviewPanelSet

> reviewPanelSet records that `review_panel` was an EXPLICIT key — written
> by the config file at Load, or carried by a stored run-config document at
> ApplyRunConfig — so Validate can tell two shapes apart that the slice
> alone cannot: an operator who wrote `"review_panel": []` (a panel of none
> — refused, because it is almost certainly a half-edited config) and a
> config that never mentioned the key (nil, the single reviewer). An overlay
> REPLACES the pipeline section, so it rewrites this flag from the
> document's own key presence rather than inheriting the file's.

### ProjectRoot

> ProjectRoot marks every workdir in this file as a FLEET PROJECT ROOT
> (CLA-678): `<machine root>/<project slug>`, holding that project's clones
> (<project root>/<repo name>) and its worktrees directory
> (<project root>/worktrees). The supervisor's workdir derivation sets it
> on the config it materializes; set it by hand to adopt the layout without
> the supervisor.
>
> It is what decides a session's permission SCOPE. With it, a session
> starting in one of the project's checkouts reaches its whole project —
> every repo under the project root and every worktree in it — and nothing
> outside; without it, the scope stays the session's own checkout, exactly
> as before. So a config whose workdir is a developer's multi-repo parent
> (`~/dev`, say) must NOT set it: the flag is an assertion about the
> layout, and granting a whole directory tree on a guess is the wrong
> direction to be wrong in.
>
> `omitempty` is load-bearing: Config.Identity() hashes the marshalled
> form (see ReviewPanel for the same care), so the field must NOT alter the
> identity of a config that does not adopt the layout — an unset flag is
> the legacy shape, and a daemon's beacon must not report a config change
> just because this field exists. Set it, and the identity does change,
> correctly: the config now means something different.

### CloneURLBase

> CloneURLBase is the URL PREFIX the supervisor clones a project's missing
> repos from: <base><owner>/<name>.git. Empty means the built-in
> DefaultCloneURLBase (`https://github.com/`), so a config that never
> mentions the key clones exactly as it always did.
>
> It exists because a derived fleet clone's origin is otherwise always
> HTTPS, which makes a session's branch pushes depend on a GitHub token
> riding in its environment (the `gh auth token` scoping on the clone).
> An SSH prefix — `git@github-personal:` for an ssh host alias, or
> `git@github.com:` — makes the clone authenticate with the key alone and
> skips the token scoping entirely. Any other mirror prefix works too;
> it is a prefix, not a GitHub-specific setting.
>
> The value is a PREFIX, so it has to end where the owner begins: a URL
> base ends with `/` (`https://github.com/`, `file:///srv/mirrors/`) and
> an scp-style ssh base ends with `:` (`git@github-personal:`). Anything
> else is refused by Validate rather than concatenated into a broken URL.
>
> `omitempty` is load-bearing for the same reason as ProjectRoot: an unset
> key must not change a daemon's identity, and a set one correctly does.

### InstanceName

> InstanceName names this daemon on the console's Fleet page (CLA-466):
> presence beacons are keyed per (project, instance name), so this is how two
> daemons driving the same project stay distinguishable. Empty resolves to
> the machine's hostname plus the config file's basename (see
> ResolveInstanceName) - unique per running daemon, so co-located daemons
> with distinct config files no longer collapse into one Fleet row (CLA-501).
>
> The plane caps the name at 100 characters — Validate refuses longer here,
> so a too-long name fails loudly at startup instead of silently dropping
> every beacon. The resolved default is truncated to the same cap.

### MaxIterations

> MaxIterations stops the loop after N respawns. 0 = no iteration ceiling:
> the loop runs until a STOP/HALT marker or a signal stops it (or a budget
> ceiling is reached); an empty queue idle-polls rather than exiting — the
> same falsehood-corrected shape CLA-290 applied to doctor's no-ceiling
> detail, kept in lockstep because this comment is where the old lie
> survived longest.

### MaxTurns

> MaxTurns is the run-wide turn cap any phase inherits when it sets none of
> its own (see Phase.MaxTurns for the resolution chain). It exists because
> the cap used to be reachable ONLY through per-phase config, so an unphased
> run — the default — had no cap at all: one measured session ran 1093 turns
> / 285.9M tokens with nothing able to interrupt it (CLA-343). 0 = defer
> upward to the built-in default; "uncapped" is not a value this config
> accepts.

### MaxSessionWallClock

> MaxSessionWallClock is the run-wide per-SESSION wall-clock cap any phase
> inherits when it sets none of its own (see Phase.MaxWallClock). It is the
> turn cap's stand-in for a harness whose CLI takes no turn flag: under
> opencode, Invocation.MaxTurns reaches nothing, so without this a session
> that works past its brief has no backstop at all and the CLA-314 salvage —
> which assumes a phase CAN be cut off — never gets its chance.
>
> 0 = OFF, and that is the default: see Phase.MaxWallClock for why this dial
> alone among the backstops ships without a built-in number.
>
> Measured per SESSION, never across the run: Budget.MaxWallClock is the
> run-wide elapsed ceiling and counts the hours a run spends WAITING OUT a
> usage limit, which is exactly why it cannot double as a runaway detector.
> A session's own clock starts when its process does.

### PauseWindows

> PauseWindows is the operator's recurring local-time schedule of hours in
> which the loop starts no new work — checked between iterations, like the
> console pause, AND at every phase boundary. A session already running is
> never interrupted; the pause holds the NEXT phase/session, keeps the
> sequence's claim alive, and resumes the same run there when the window
> ends. See docs/pause-windows.md. Nil/empty = no schedule. `omitempty` is
> load-bearing for the same reason as ProjectRoot: an unset key must not
> change a daemon's config identity.

### RouteSchedules

> RouteSchedules are NAMED pricing schedules a route can be attached to
> (CLA-947), keyed by schedule name. One definition attached to N routes
> means DeepSeek's peak hours — which move as a set — are one edit, not one
> per route.
>
> They are deliberately named rather than inline per route: per-route
> literals drift, and a route's window is a fact about its provider's
> billing, which several routes share.
>
> `omitempty` is load-bearing for the same reason as PauseWindows: a config
> that sets no route schedule must keep its existing identity.

### RouteWindows

> RouteWindows attaches a route ID (the exact model string the driver
> iterates) to a RouteSchedules entry. A route inside its schedule's window
> is unavailable for a NEW session, exactly like a route carrying a usage
> mark — but it is a filter on route SELECTION, never a gate on the driver:
> a route with no entry is always available, so a newly added model cannot
> inherit a schedule by accident.
>
> Keyed by route ID rather than provider or model because the same model
> reached through a flat-priced provider has no peak tier.
>
> `omitempty` is load-bearing for identity, as above.

### MaxZeroSpendAttempts

> MaxZeroSpendAttempts bounds consecutive attempts within one drain that
> ended without the harness reporting any usage at all. 0 = the built-in
> default (DefaultMaxZeroSpendAttempts).
>
> It is the ceiling's blind spot made bounded. A spend ceiling can only stop
> spend it is told about, and an attempt that dies before its harness reports
> usage tells it nothing - so under `max_retries: 0` and a token- or
> cost-only budget, the retry ladder is: attempt, zero spend, back off at
> retry_cap, attempt, forever, with only max_wall_clock able to end it
> (CLA-288). An attempt that DOES report resets the count, including one that
> honestly reports zero, so this never counts a session that merely did
> nothing.

### MaxRepeatedToolFailures

> MaxRepeatedToolFailures ends a session whose SAME tool returns the SAME
> error on this many consecutive tool calls (CLA-739). The harness kills the
> session mid-stream, and the driver classifies it as dead with a cause
> naming the tool, the count and the error. Unset = the built-in default
> (DefaultMaxRepeatedToolFailures); 0 turns the check off; negative is
> refused.
>
> A POINTER, unlike max_zero_spend_attempts, because 0 here is a real
> setting (off) and not "use the default", so unset has to be a third state.
> And `omitempty`, because the config's marshalled form is its identity (see
> EchoSessions): adding the knob must not change an existing daemon's.
>
> Enforced by the harnesses that observe tool results as they stream -
> claude and opencode. Inert under codex.

### SessionNice

> SessionNice runs every spawned harness session's process tree at a LOWER
> scheduling priority: 0 (the default) leaves behaviour unchanged, and a
> positive value (1-19) is added to every process the session forks — test
> runners, builds, dev servers. It exists for a machine where the drain fleet
> shares cores with something that must not wait, such as a self-hosted CI
> runner: the kernel serves CI first while they contend, and still gives a
> session the whole machine when CI is idle (CLA-899).
>
> Applied at the harness spawn, by nice(1) on the child, so the whole tree
> inherits it and the daemon's own priority is untouched. `omitempty` is
> load-bearing for the same identity reason as the dials above; a positive
> value on a platform with no nice(1) is REFUSED at validation rather than
> ignored, because an operator who set it believes CI is protected.

### EchoSessions

> EchoSessions mirrors each session's output stream to the daemon's own
> stderr while it runs, as well as to its iteration log. Default FALSE
> (CLA-701): the stream is the whole harness transcript — opencode emits
> one JSON event per line, many carrying tool output inline — so echoing it
> buries the daemon's own lines (spawn, exit, classification, why the run
> ended) under thousands of rows of scrollback, and the iteration log keeps
> it in full anyway. Turn it on when watching one session live is worth
> more than the terminal being readable: `"echo_sessions": true`.
>
> `omitempty` is load-bearing for the same reason as ProjectRoot: the
> config's marshalled form is its identity, and the default must not change
> a daemon's identity merely because this field now exists.

### SettingsPath

> SettingsPath points the harness at an extra settings file (Claude Code's
> --settings) carrying the headless permission policy — the allow/deny rules
> that gate what an unattended run may call, since there is no human to prompt
> and no interactive auto-mode classifier. It MERGES with (does not replace)
> the config-dir's own settings, and deny rules win — so this file's job is to
> grant the few tools the run needs and to deny the exfil vectors the ambient
> allowlist leaves open. Claude-specific; other harnesses ignore it. ~ expands.

### Repos

> Repos maps the repos a project's tasks may name to their local checkouts,
> so a session starts in the TASK's repo rather than in the multi-repo parent
> (agent-rule-scoping pieces 2 and 3). Keyed by repo identity — either the
> full "owner/name" form the task carries or just its bare name — with the
> checkout path as the value (~ and relative paths both expand; a relative
> path resolves against the workdir). Resolution order, fallbacks and the
> loud repo_not_found failure are ResolveCheckout's, which is the only thing
> that should interpret this map.
>
> Declaring repos also widens each session's permission policy to cover every
> declared checkout regardless of cwd, so a two-repo project does not wall
> sessions started in one repo off the other. The grants themselves stay in
> local config — this map holds paths, never credentials.
>
> In single-project mode set it here; in multi-project mode set it per
> project (see Project.Repos), whose non-empty map replaces the top-level
> one for that project.

### PrimaryRepo

> PrimaryRepo names the repo a session starts in when its task carries none:
> the fallback that keeps a fresh phase out of the multi-repo parent until
> the plane returns a repo for every task. It names a `repos` key, or any
> identity resolvable by the same steps; with exactly one repo declared and
> no primary named, that one is primary implicitly. Empty = no fallback, so
> a task without a repo fails the iteration with repo_not_found rather than
> start somewhere unconsidered — unless NO repos are configured at all, in
> which case this whole feature is off and the legacy workdir behaviour runs.

### AllowUncheckedPR

> AllowUncheckedPR opts this project out of CLA-310's empty-check-rollup
> refusal: a delivery whose PR carries NO checks is logged as a WARNING
> instead of refused. It exists for repos with NO CI at all, where refusing
> every delivery would wedge the driver shut; both clankerbar repos have
> CI, so for them the default (refuse) costs nothing.
>
> The mergeability half of the gate is NEVER relaxed by it: a CONFLICTING
> PR is refused either way, because a conflicted PR is exactly the state
> that produces an empty rollup, and silence-reads-as-pass is the bug the
> gate exists to kill. The safe state is the default; the loose state is
> this visible, operator-owned line, which `doctor` reports so it is seen
> before it fires. In single-project mode set it here; in multi-project
> mode set it per project (see Project.AllowUncheckedPR), which overrides
> the top-level value for that project only.

### HealthURL

> HealthURL is the deployment health endpoint (`/health`) doctor's deploy_lag
> check reads to learn WHICH commit is running out there - CLA-322. The plane
> stamps version.commit at BUILD time, so "is commit X live?" is one request
> and a string compare; this URL names where to ask. It is an operator-set
> value with no derived default, because a deployment can live anywhere
> (staging, production, a self-hosted plane) and deriving it from backlog_url
> would answer about whichever environment the guess happened to hit. Empty =
> not monitored: the check says so once, quietly, and moves on.
>
> The endpoint is read WITHOUT credentials - /health is public - so unlike
> backlog_url it is held only to "parses as an absolute URL", not to the
> bearer-token TLS floor.

### IntegrationBranch

> IntegrationBranch is the branch deployments are built from and promoted
> along - the branch whose tip a healthy deployed build should be an ancestor
> of. Doctor's deploy_lag check compares /health's version.commit against its
> REMOTE tip. Default: staging (the convention both of this project's repos
> use); override where your project integrates elsewhere. It deliberately does
> NOT come from delivery.Claim.IntegrationBranch: that is a per-session
> declaration of where one commit landed, while this is standing per-project
> configuration about how releases flow.
>
> It is NOT what the phase briefs call the PR base (CLA-608): a repo can
> deploy from one branch while its PRs target another (lecstor/makespdf
> deploys staging but merges task branches into integration), and a repo in
> a driven project can have no integration branch at all (the makesPDF
> plugin repos carry only main). See PRBase for the override that names a
> PR base explicitly; without one, each repo's PR base is resolved against
> the origin remote at phase-spawn time.

### PRBase

> PRBase maps a repo identity ("owner/name", or its bare name) to the branch
> that repo's PRs TARGET - the branch a task branch forks from and merges
> back into, substituted into the phase briefs as {{prBase}} (CLA-608).
>
> It exists because the briefs used to hard-code "(staging)" for every repo
> of every project: the makesPDF plugins have no staging branch (their PRs
> all target main), and lecstor/makespdf and lecstor/clankerbar merge into
> integration - so a session was told to open its PR against a branch its
> repo does not have, and the setting raised itself as a decision on every
> task. An entry here wins outright, no probe: name the branch when the
> convention is not derivable. Without an entry the driver resolves per
> repo at phase-spawn time: the project's integration_branch when it
> exists on the origin remote, else origin's default branch (see
> loop.Driver.prBaseFor).
>
> This is deliberately NOT integration_branch renamed: that one keeps
> meaning the branch whose tip doctor's deploy_lag check judges, so a repo
> that deploys staging and merges integration needs both to stay truthful.

### AllowLocalMCPServers

> AllowLocalMCPServers names the MCP server entries an operator means to run
> from a DISCOVERED `<workdir>/.mcp.json` — the file Validate adopts by
> default when mcp_config_path is empty. Any local-command entry whose name
> is on this list is accepted from a discovered file; every other command
> entry in one is refused (CLA-266: such an entry starts a process at session
> init, before any permission rule applies, and a workdir default is not
> where that decision belongs). It reaches nothing else: opencode-schema
> policy keys (`permission`, `plugin`, `agent`) in a discovered file are
> refused regardless of this list — naming servers never approves policy.
>
> The safe state is the default (refuse); the loose state is this visible,
> operator-owned list, which `doctor` reports. A file the operator NAMES via
> mcp_config_path needs no entry here — naming it IS the statement, and its
> local servers are disclosed by doctor's WARN as they always were.
>
> In single-project mode set it here; in multi-project mode set it per
> project (see Project.AllowLocalMCPServers) to replace the top-level list
> for that project only.

### Projects

> Projects declares the backlogs a single loop instance drives — one entry per
> clankerbar project (CLA-142: one account key, many queues). Empty = the
> original single-project mode, driven by the top-level fields, exactly as
> before. When set, the driver polls each project's
> `/api/projects/<slug>/backlog-summary` and spawns each drain session in that
> project's workdir, round-robin over whichever queues have claimable work.
> The account key in CLANKERBAR_API_KEY covers every project the operator is a
> member of; per-project keys are never needed.

### Env

> Env is extra environment for the spawned harness process. Each entry's
> value is either a literal string or an object {"fromCommand": "..."} whose
> stdout becomes the value, resolved FRESH AT EVERY SPAWN so a rotated token
> reaches the next session without a daemon restart (CLA-462). The child
> already inherits the loop's own environment, so this is for the unattended
> case: with GH_TOKEN declared here — per project if you drive several — the
> cb-run wrapper stops being load-bearing and launching the binary directly
> is safe.
>
> The map overlays at FOUR levels in most-specific-wins order (the same
> precedence ResolveMCPConfig applies): this top-level map first, then
> `harnesses.<name>.env`, then `projects[].env`, then that project's
> `env_per_harness.<name>`. A value of the form "@path" is replaced by the
> contents of that file (trimmed; a leading ~ is expanded) — keep a secret in
> a 0600 file and point at it here rather than inlining it. That 0600 is
> ENFORCED, not advice: a file any other local account can read refuses the
> session spawn, as it always refused Validate.
>
> A fromCommand whose execution fails, times out (10s), or produces empty
> output REFUSES the session spawn with a log line naming the variable —
> fail closed, because a session missing its declared env is the incident,
> not a degraded mode. `doctor` verifies every declared command resolves
> before a run starts.

### EnvAllowlist

> EnvAllowlist, when non-empty, is the COMPLETE set of inherited environment
> variable NAMES a spawned session receives (CLA-714). Variables not named
> here are dropped from the harness process, so the session cannot read the
> tokens that happen to sit in the shell that started the supervisor.
>
> Unset is the default and keeps today's behaviour exactly: the harness
> inherits the daemon's whole environment. An explicitly EMPTY list is
> refused by Validate as a half-edit, so the only way to say "everything" is
> to omit the key. The list is about INHERITANCE only — the `env` maps above
> are still added on top of it at every level, which is how GH_TOKEN and
> CLAUDE_CODE_OAUTH_TOKEN arrive. A var may therefore be in the allowlist,
> in an `env` block, or in both; a `fromCommand` may read a variable that is
> NOT in the allowlist, because it runs in the daemon, before the filter.
>
> It composes at the same four levels as `env` (top level, harness, project,
> project-per-harness) but as a WHOLE LIST, not per key: the most specific
> level that declares one wins entirely. Restate a broader list where you
> narrow it; an intersection would make a level's own entry silently
> ineffective. `doctor` FAILS when the list in force omits a variable the
> fleet needs (HOME, PATH, CLANKERBAR_API_KEY, and CLANKERBAR_WORKDIR_ROOT
> when the workdir derivation is on), because a session missing one of those
> fails later and elsewhere.
>
> `omitempty` is load-bearing for the same reason as ProjectRoot: an unset
> key must not change a daemon's identity, and a set one correctly does.

### harnessFromFlag

> harnessFromFlag / modelFromFlag record that --harness / --model overrode
> the file. Both flags say "the run has ONE harness and this is it", which a
> mixed-harness config contradicts: --harness would re-point which harness
> SessionFor treats as top-level, so the other one would start inheriting the
> run-wide claude fields it must never see, and --model would silently apply
> to one phase's sessions and not the rest. Validate refuses rather than
> resolving either; see there.

### MCPConfigPaths

> MCPConfigPaths is this project's MCP config PER HARNESS (harness name ->
> path), for a run whose phases are not all on one harness (CLA-366). Empty
> on every single-harness config.
>
> It exists because the two axes multiply. MCPConfigPath above selects the
> PROJECT (its `/mcp/<slug>` URL is what the poll and the sessions agree on),
> while the file's SCHEMA belongs to a harness — opencode refuses to start on
> Claude's `mcpServers`. One file cannot be both for two harnesses, so a
> multi-project mixed-harness run needs one per pair, and Validate refuses the
> combination rather than letting every project's opencode phase quietly share
> whichever single file `harnesses.<name>.mcp_config_path` named — which would
> poll one queue and work another, the split-brain the slug check below exists
> to prevent.
>
> Each entry is held to the same origin and slug checks as MCPConfigPath.


## `internal/config/conflict.go`

### The supervisor's conflict-only session step (CLA-759)

> The supervisor's conflict-only session step (CLA-759).
>
> CLA-758's deterministic close-out integrates a CLEAN approved task with no
> agent session. A CONFLICTING one still waited for an agent close-out: the next
> clanker to finish the task it was on ran a FULL implement-phase session, with
> the whole drain protocol in its brief, to resolve a merge conflict.
>
> This is the narrow replacement: one short session that claims the task, merges
> the PR's base into its branch, resolves ONLY the conflicting hunks, runs the
> repo-wide gate and pushes. The supervisor starts it (and the supervisor's own
> package carries that code and its tests); this file owns the STEP's config and
> the brief, because both are operator-facing text the config package already
> owns for every other brief.
>
> The step is deliberately its own top-level block rather than another entry in
> `phases`: a phase is a step in ONE task's sequence, while this runs outside any
> sequence, for one task the supervisor picked, at most one at a time. It still
> reads like the rest of the pipeline — A step with a harness and a model — and
> defaults to the fleet's IMPLEMENT step, because resolving a conflict is
> implementation work that wants the model already trusted with the task's code.

### ConflictStep

> ConflictStep is the config block controlling the supervisor's conflict-only
> session.
>
> Resolution order for every field is "this block, else the implement step": an
> unset block runs the conflict session on exactly the harness and model that
> implemented the branch, which is the fleet's documented default and needs no
> repetition in the config file. A set field pins only itself, so an operator can
> name a cheaper model for conflict resolution without repeating the harness.

### ConflictStep

> ConflictStep resolves the effective conflict step: the block's own fields where
> it names them, and the implement step's harness/model routes everywhere else.
>
> Total, never an error: an unset or partly set block is the documented default,
> not a refusal. Validate is where an unknown harness is refused, using the same
> registry check every other harness-naming field gets.

### implementStep

> implementStep answers the harness and the WHOLE model route list of the
> config's implement step: the first configured phase named "implement", or the
> run-wide harness/model for an unphased run (where EffectivePhases synthesizes a
> single implement phase).
>
> The routes are resolved through the phase's own tier, so a tiered review panel
> beside an untiered implement phase cannot shift the conflict session's models.
> The whole list is carried because the conflict session runs the same fallback
> ladder a phase does (CLA-890): resolving only the first route left a free-tier
> quota death with nowhere to go.

### ConflictResolution

> ConflictResolution reads the LAST resolution marker in outcome. It returns the
> kind, the first explanatory line after that marker (the note for a behavioural
> resolution, the reason for a failure) and whether a marker was found at all.
> The brief tells a session to copy the stored outcome verbatim and APPEND its
> block, so the newest marker is the last one. Total and forgiving: the outcome
> is written by a model, so a marker among prose is found line by line rather
> than required to be the whole outcome.

### ConflictResolutionSince

> ConflictResolutionSince reads the marker THIS session appended, given the
> outcome the claim response served BEFORE the session (prior) and the outcome
> served after it (current). An unchanged outcome means the session wrote
> nothing, and therefore left no marker — the accumulated text's older markers
> belong to earlier attempts and must not classify this one (CLA-889). Otherwise
> the session's own text is the suffix it appended to the stored outcome, and the
> LAST marker in it wins.
>
> Total and forgiving like ConflictResolution: a session that replaced the
> stored outcome instead of appending to it is classified on what it wrote.

### ConflictBrief

> ConflictBrief renders the short brief a conflict-only session runs. It is NOT
> the implement brief: the session must not claim anything (the supervisor
> already did), must not work another task, and must not review or merge.
>
> The instructions are the task's own contract (CLA-759):
>
>   - merge, NEVER rebase: the branch is already pushed, and rewriting published
>     history invalidates the fleet's tip checks;
>   - resolve ONLY the conflicting hunks: no unrelated edits, no formatter sweep,
>     no dependency bump hiding inside a conflict resolution;
>   - run the repo-wide gate and push;
>   - then hand back in exactly one of three states: mechanical (the task stays
>     approved for the deterministic close-out), behavioural/unsure (the code
>     changed behaviour, so it goes back to review) or failed (the claim is
>     released and the task stays approved for the agent close-out).
>
> The outcome markers are load-bearing rather than decorative: the supervisor
> reads them after the session ends to tell a mechanical hand-back from a failed
> one, because both leave the task in the same plane status.


## `internal/config/discovered.go`

### This file holds CLA-266's rule

> This file holds CLA-266's rule: a DISCOVERED MCP config may not silently
> decide what the next unattended session executes.
>
> `mcp_config_path` defaults to `<workdir>/.mcp.json` (discoverMCPConfig). That
> default points into a checkout — which the sessions themselves can write,
> since they run with edit permission in it, and which may have been cloned
> from anywhere — and the file is then handed to the harness WHOLE:
>
>   - under claude, as `--mcp-config --strict-mcp-config`, making it the
>     session's entire MCP surface;
>   - under opencode, never: a .mcp.json is gated out of opencode's hand-off
>     (mcpConfigHandedTo), though a file named otherwise would be passed as
>     `OPENCODE_CONFIG`, the session's ENTIRE configuration. The rules below
>     still hold because the poll reads the discovered file for every harness.
>
> Three refusals follow for a file nobody named:
>
>  1. An MCP entry carrying a `command` starts a process at MCP init — before
>     any tool-permission rule has an opinion. That is the reasoning origin.go
>     already states for a local server handed the API key; it applies whether
>     or not the process gets the key, because it inherits the session's whole
>     environment (CLANKERBAR_API_KEY among it) and runs as the operator.
>
>  2. Under opencode the same file may set top-level keys that decide what a
>     session IS rather than what it talks to: `permission` would override the
>     fail-closed policy the adapter pins (the CLA-260 threat with the safety
>     rail removed), `plugin` loads startup code, `agent` replaces agent and
>     per-agent permission definitions. A checkout silently deciding any of
>     those has no honest reading.
>
>  3. An UNREADABLE discovered file is refused outright, for the same reason
>     every other reader of this file fails closed: it is handed onward either
>     way, so "cannot be checked" cannot mean "accepted".
>
> The escape hatch is naming things, in two grades:
>
>   - `allow_local_mcp_servers` lists the ENTRY NAMES an operator means to run
>     from a discovered file — the CLA-310 shape: the safe state is the default,
>     the loose state is a visible, operator-owned config line doctor reports.
>     It admits those names and NOTHING ELSE, and never reaches rule 2.
>   - naming the FILE itself (`mcp_config_path`, per harness or per project)
>     adopts it wholesale — the operator's vetting statement, the same line
>     this package drew when clankerbar.json stopped being discovered from the
>     working directory (refuseImplicitWorkDirConfig).
>
> Both checks read BOTH schema blocks (`mcpServers` and `mcp`) regardless of
> which harness will run, for the reason mcpFile records: a file that shows one
> block to a gate and another to the harness is exactly the decoy CLA-257's own
> review caught.

### checkDiscoveredMCPConfig

> checkDiscoveredMCPConfig refuses a workdir-discovered MCP config that decides
> what the next unattended session executes: any local-command server entry not
> named in `allow` (rule 1), or any opencode-schema policy key (rule 2, which
> no allowlist relaxes).
>
> label names the config field in errors ("mcp_config_path",
> "projects[0].mcp_config_path"), matching checkMCPConfigOrigins, so the
> operator is told which line to add next to the place they see every other
> MCP-config refusal. allow is the entry-name allowlist resolved for the scope
> this path serves (AllowLocalMCPServersFor).
>
> It is called only where Validate has just FILLED the path by discovery, so
> the provenance is known at the call site and nothing has to be remembered
> between there and here.

### The mechanism behind this one, measured in 1.18.19 (CLA-441)

> The mechanism behind this one, measured in 1.18.19 (CLA-441). The
> obvious reading is that it cannot bite - the driver's policy is
> applied after every config layer (on 2.x in OPENCODE_CONFIG_CONTENT),
> so surely it overwrites the file's block. It does not. That merge is recursive and PER KEY, `fromConfig`
> flattens the result in INSERTION order rather than sorted, and
> `evaluate` is findLast over that order. Our document is marshalled
> from a Go map, so it arrives sorted with the `*` catch-all first,
> which is the whole basis of the fail-closed posture. A file that
> declares `permission.read` gives `read` ITS position in the merged
> object - ahead of the `*` deny, which the driver adds in the last layer - so
> the catch-all matches last and every read in the session is denied.
> Not a loosening, a total lockout, which is the CLA-441 wall itself.


## `internal/config/done_bounds.go`

### The plane's durable-record bounds, mirroring REVIEWBOUNDS in clankerba

> The plane's durable-record bounds, mirroring REVIEW_BOUNDS in clankerbar's
> apps/web/src/repositories/task-reviews.ts. The plane refuses an over-bound
> record whole rather than clipping it, so the runner refuses it first.
> Only the session-authored fields are mirrored; the runner-filled ones
> (reviewerRole, harness, model, branch, reviewedTip, scope, raisedByModel) are
> not, because an over-long one is a config fault the plane reports, not
> something a session can fix.


## `internal/config/done_report.go`

### ParseDone

> ParseDone finds and decodes the done report in a session's final message.
>
> The scan mirrors loop.parseHandoff deliberately (the two markers are read out
> of the same message shape): the LAST marker line wins because the block is
> defined as ending the message, the marker must be UNINDENTED, and it must not
> sit inside a ``` fence - both are the natural shapes of a session quoting the
> mechanism rather than emitting a report.
>
> found is separate from err so a marker with an unreadable body is logged as
> what it is (a report the runner could not use) rather than as "no marker".
> Unknown fields are ignored: the briefs and the runner may drift by a field
> without a session's whole report becoming unreadable.

### ValidateReview

> ValidateReview holds a review-shaped done report to the record the runner
> writes: the verdict and its findings agree, the reviewed tip is a commit, no
> finding string is over the plane's bound, and a verify report's findingUpdates
> are shippable. tip is the checkpoint tip the runner verified, "" when it had
> none to verify against.
>
> The errors name the offending field or finding index because they go to the
> log, where "invalid report" is worth nothing to whoever reads it.


## `internal/config/endpoints.go`

### BacklogEndpoint

> BacklogEndpoint returns the full, project-scoped MCP URL the driver's own
> backlog poll should hit — the same `/mcp/<project>` endpoint the harness uses.
>
> BacklogURL defaults to a bare base (`https://clankerbar.com`), which the plane
> rejects without the project slug in the path. The slug lives in the harness
> .mcp.json (MCPConfigPath), so we take it from there — but ONLY the slug. The
> origin always comes from CredentialOrigin, because this URL carries the API key
> (CLA-257): a file inside the workdir may say which project, never which host.
> An explicit backlog_url that already names a `/mcp/<project>` path wins outright.
>
> The raw field is read, not the harness-gated resolution (ResolveMCPConfig):
> since CLA-318 the file stays every run's slug source even when the running
> harness would not be handed it.
>
> Returns "" when no usable project-scoped endpoint can be resolved (a bare base
> and no slug). That is deliberate: New("") yields a not-wired poller, so the loop
> falls into blind drain — which still makes progress — rather than retrying
> forever against a slug-less base the plane can only reject.

### A pre-CLA-99 file names a bare /mcp with no slug to lift out

> A pre-CLA-99 file names a bare `/mcp` with no slug to lift out. Take its
> path verbatim, but only once its origin is proven to BE the trusted one —
> which Validate has already refused the file for otherwise. Dropping to ""
> here instead would silently disable the driver's one write (the CLA-242
> release-on-interrupt) for those setups: a security fix quietly deleting an
> unrelated feature.

### ProjectEndpoint

> ProjectEndpoint returns a project's MCP endpoint: `<origin>/mcp/<slug>`, the
> slug path-escaped. The origin is CredentialOrigin — `backlog_url`, the
> operator's own config, or the built-in default — never a host read off a
> checkout's .mcp.json (CLA-257).
>
> The slug need not be a project this config declares: propose-config --slug and
> upload --project may both name one it does not, so the write lands where the
> caller said rather than necessarily where this config's sessions are pointed.
> The plane membership-gates /mcp/<slug>, so an unreachable slug is refused
> there, never mapped onto a project here.

### BacklogSummaryURL

> BacklogSummaryURL returns the URL of the driver's cheap backlog read: the
> plane's `GET .../backlog-summary` surface, which returns {version, counts,
> claimable, openQuestions, loopPaused} in one authenticated call (counts to gate
> on, plus the console-driven pause the driver honours).
>
> Two forms exist (CLA-141), and which one we return decides which key kinds work:
>
>   - `/api/projects/<slug>/backlog-summary` — project named in the PATH, so the
>     operator's ACCOUNT key works (membership-gated, exactly like /mcp/<slug>);
>     a project key works too, for its own slug. Returned whenever a slug can be
>     derived from the resolved MCP endpoint (an .mcp.json naming /mcp/<slug>).
>   - `/api/backlog-summary` — the legacy slug-less route, where only a
>     project-scoped key can select the project. The fallback when no slug is
>     derivable, so pre-CLA-141 setups keep working unchanged.
>
> The origin is CredentialOrigin — `backlog_url`, the operator's own config, or
> the default base — and nothing else. Before CLA-257 it fell back to the origin
> named by the workdir's .mcp.json whenever backlog_url was left at its default,
> which is the normal setup, so a committed file in a cloned repo could redirect
> this credentialed GET to any host over plain http. A self-hosted plane is still
> reachable; it just has to be named in `backlog_url`. Only the SLUG still comes
> from .mcp.json, and it only chooses a path on an origin we already trust.
>
> Returns "" when no origin can be resolved (New("") then yields a not-wired,
> blind poller).

### FleetReportURL

> FleetReportURL returns the URL of the daemon's fleet presence beacon target
> (CLA-466): the plane's `POST .../fleet/report` surface, which takes presence
> plus any finished iteration records in one authenticated call.
>
> Unlike the summary route there is NO legacy slug-less form — the report
> endpoint exists only as `/api/projects/<slug>/fleet/report` (CLA-465). So a
> setup with no derivable slug has no fleet URL at all: reporting is off for
> that run (doctor says so), and the loop behaves exactly as it did before
> fleets existed. Same origin rule as every credentialed call: CredentialOrigin,
> never a path read off the workdir's .mcp.json.

### ConversationPendingURL

> ConversationPendingURL is the planner loop's 2-second wake poll for the
> single-project config (studio-chat.md §4): the project named by the config's
> MCP wiring. It is process-level HTTP, not an MCP call, so the poll never
> touches a model.
>
> Empty when no slug can be derived: the endpoint has no legacy slug-less form,
> exactly like FleetReportURL, and an underivable slug means this config cannot
> serve chat. The origin is CredentialOrigin — the same trusted origin every
> other credentialed call uses.

### ResolveInstanceName

> ResolveInstanceName composes the Fleet-page instance identity a daemon
> beacons under (CLA-466): an explicit instance_name wins verbatim; otherwise
> the default is the machine's hostname plus the config file's basename minus
> its .json suffix ("Jasons-MBP/clanker1"), because the config PATH is what
> actually distinguishes co-located daemons (CLA-501). The earlier
> bare-hostname fallback keyed every same-host daemon to one presence row,
> each beacon overwriting the last.
>
> The config-identity hash was considered and rejected as the discriminator:
> it fingerprints file CONTENT, and the live co-located configs hash identically.
>
> The composed default is truncated to MaxInstanceNameLen so it cannot exceed
> the plane's cap. Two paths identical within that prefix would still collide;
> that is pathological, and visible to doctor's sibling scan, which resolves
> identities through this same function.

### Identity

> Identity fingerprints the effective config in force — what the fleet presence
> beacon reports as `configIdentity` (CLA-466), so the console can tell two
> daemons apart when they disagree about how to run and can see a RELOAD land.
>
> It is a SHA-256 over the JSON encoding of the config AS VALIDATED (flag
> overrides applied), recomputed on demand rather than cached: a RELOAD swaps
> d.cfg mid-run, and the next beacon must carry the new identity. Only exported
> fields encode; no secret is in the config file by contract (the API key lives
> in CLANKERBAR_API_KEY). Map fields encode with sorted keys, so the fingerprint
> is stable across processes.

### mcpURLFromConfig

> mcpURLFromConfig reads a harness MCP config and returns the clankerbar MCP
> server URL, or "" if the file is absent or names no such server.
>
> Callers use this for the PATH — a project slug — never for an origin (see
> origin.go). The old "else the first http server's URL" fallback is gone with
> it: it made an unrelated MCP entry (a docs server, a browser driver) speak for
> clankerbar, and picked which one by map iteration order. What is left is the
> entry named `clankerbar`, else whichever entry is handed CLANKERBAR_API_KEY —
> the two ways a file can actually mean "this is the clankerbar server".
>
> A read/parse failure yields "", which is safe HERE and only here: Validate has
> already refused such a file outright (checkMCPConfigOrigins fails closed), so
> this is never reached with one.

### retiredHarnessHint

> retiredHarnessHint names what replaced a harness that used to be registered,
> so a config written for it fails with the fix in the message rather than a
> bare "unknown harness". `opencode2` drove the 2.x preview; since CLA-675 the
> `opencode` harness drives 2.x itself and the preview adapter is gone. It is
> not kept as an alias: one name per adapter keeps the binary doctor looks up,
> the harnesses block, budgets and logs all agreeing.


## `internal/config/env.go`

### This file makes session env DAEMON-OWNED (CLA-462)

> This file makes session env DAEMON-OWNED (CLA-462). Before it, load-bearing
> session environment could only be declared at one level and resolved once at
> Validate, so an operator who launched the binary without the cb-run wrapper
> spawned sessions with no GH_TOKEN — and every such session died later, at
> `git push` or `gh`, with an error that named neither the cause nor the fix.
> The root cause was ownership: the LAUNCHER decided whether sessions could
> push. Restart mechanisms cannot fix that class of failure, because re-exec
> preserves whatever environment was broken.
>
> The shape that replaces it: `env` maps at FOUR levels — top level, per
> harness, per project, and per project-per-harness — overlaid most-specific-
> wins in the same order as the existing MCP wiring (ResolveMCPConfig), where a
> value is either a literal string (including the "@path" file-reference form)
> or {"fromCommand": "..."} run FRESH AT EVERY SPAWN so tokens rotate without a
> restart. A failing command REFUSES the spawn rather than degrading: a session
> without its declared env is the incident, not a mode to run in.

### RunEnvCommand

> RunEnvCommand executes one declared fromCommand and returns its trimmed
> stdout. Package-level so tests can stub it (a counter script proves
> freshness; a failing stub proves the refusal); the default runs the command
> under `sh -c` with the command timeout. It comes from the operator's own
> config file — the same trust root as everything else in the config — never
> from a workdir file, whose credential-origin rule (origin.go) is untouched.

### validateEnvDecls

> validateEnvDecls holds one `env` block to its schema: every NAME must be a
> valid environment-variable name, and every declared command must be
> non-empty. Refused at Validate rather than discovered at spawn, because both
> failures are static facts of the config file.
>
> The name rule is also how "non-string names" are refused. encoding/json
> delivers map keys as Go strings no matter what the file said, so a bare
> number in the file fails JSON parsing outright and a quoted number arrives
> here as the string "123" — which is not a name any child process can read,
> and is refused by the same rule that catches "A-B" and "".

### SessionEnv

> SessionEnv composes ONE session's extra environment: the four-level overlay
> (top level, then the harness block, then the project, then the project's
> per-harness block — ResolveMCPConfig's precedence order, least specific
> first, each key's most specific declaration winning) with every fromCommand
> run FRESH, right now, so a rotated token reaches the next session without a
> daemon restart.
>
> The error names the VARIABLE whose resolution failed, and callers refuse the
> spawn on it — fail closed, never a silent partial env. Literals need no
> execution but their "@path" files are re-read per spawn too, so a chmodded
> or deleted secret file surfaces the same way instead of surviving until the
> next daemon restart.
>
> Sorted output keeps the child's environment deterministic across otherwise
> identical spawns.

### SessionEnvAllowlist

> SessionEnvAllowlist returns the inherited-environment allowlist in force for
> one session (CLA-714): nil/empty means "inherit the daemon's whole
> environment", which is the unchanged default.
>
> The four levels compose as a WHOLE LIST, not per key: top level, harness
> block, project block, project's per-harness block, each REPLACING the
> previous answer when it declares one. That is deliberately unlike the `env`
> maps, whose per-key overlay merges: an allowlist is a complete answer to
> "what may this session see", and unioning levels would make a narrower level
> unable to remove a variable a broader one admitted. An unmatched slug (the
> single-project case, which reaches here with "") gets the top-level and
> harness layers only, exactly as envOverlay does.

### validateEnvAllowlist

> validateEnvAllowlist checks one declared allowlist. A list that is present
> but EMPTY is refused rather than read as "inherit everything": `[]` is the
> shape of a half-edited config, and silently giving it the most permissive
> meaning is the wrong direction to be wrong in. Omit the key for full
> inheritance. Duplicates are refused too — a set with one member spelled twice
> tells the operator something about the list that is not true of its effect.

### resolveEnvValue

> resolveEnvValue turns one declared value into the string the child sees.
> Literals go through the same "@path" handling resolveEnv has always applied;
> command-derived values run now and must SUCCEED with non-empty output — an
> empty success is a token source that has stopped issuing tokens, and handing
> the child an empty GH_TOKEN would move the failure from spawn time (loud,
> named) into git/gh deep inside a session (silent, misattributed).

### resolveLiteral

> resolveLiteral resolves a literal value, expanding the "@path" form against
> an owner-only file exactly as resolveEnv did. A file that resolves to empty
> is REFUSED, for the same reason a command's empty output is: an emptied token
> file hands the child an empty GH_TOKEN and moves the failure from spawn time
> (loud, named) into git/gh deep inside a session (silent, misattributed). An
> inline "" literal is different — it is static and visible in the operator's
> own config, and a deliberately set-but-empty variable is a real pattern — so
> it passes through untouched. The underlying error — the insecure-mode
> sentinel included — stays wrapped, so callers can still classify it.

### VerifyEnvFilePath

> VerifyEnvFilePath checks that an "@path" secret file is readable, meets its
> owner-only rule, and is not EMPTY — the same refusal spawn-time resolution
> applies (an emptied token file is the silent-misattribution failure this map
> exists to end) — without returning its contents. It is the check Validate
> used to run on every @path value; it lives here so `doctor` can keep making
> that preflight before a run leans on a file resolution now deferred to spawn
> time.


## `internal/config/filemode_other.go`

### permissionBitsAreMeaningful

> Windows and friends. Go synthesises 0666 (or 0444 for a read-only file) for an
> ordinary file here, so the permission bits describe the platform rather than the
> file: enforcing them would refuse every config and every @path secret, making
> the tool unusable rather than safer. The releases are darwin/linux
> (.goreleaser.yaml), so this is the source-build path, and it says plainly that
> the mode-based guarantees do not hold on it.


## `internal/config/jsonc.go`

### JSONC - JSON with comments and trailing commas - is what opencode's ow

> JSONC - JSON with comments and trailing commas - is what opencode's own config
> files are written in, and `opencode.jsonc` is the conventional name for the
> global one. Go's encoding/json refuses both extensions, so a strict parse of a
> perfectly ordinary opencode config fails, and the ambient-config check
> (opencodeambient.go) would have reported "nothing to see here" for exactly the
> file that caused CLA-441.
>
> This tolerance is confined to files opencode DISCOVERS on its own. The files
> the driver NAMES - mcp_config_path and its per-harness and per-project
> variants - are still parsed strictly by readMCPFile, and deliberately: those
> feed security gates that fail closed, and widening what they accept would
> widen what an untrusted workdir file can say without being refused. Two
> parsers is the price of not moving that line.
>
> The stripper itself lives in internal/jsonc because the harness adapter's MCP
> preflight reads the same discovered files (CLA-703) and cannot import this
> package - config imports harness, not the other way round.


## `internal/config/load.go`

### Load

> Load reads config from path (or a discovered default), layered over defaults.
> An explicit path that cannot be read is an error; a missing default file is not.
>
> DISCOVERY IS HOME-ONLY (CLA-260). An explicit --config is honoured wherever it
> points, including into the working directory; what is gone is the implicit
> candidate. See refuseImplicitWorkDirConfig.

### probe

> CLA-639: whether the file named `review_panel` with a JSON ARRAY. A
> present-but-empty array is the operator's own panel of none, which Validate
> refuses; absent (nil after unmarshal), or an explicit JSON `null` (what a
> marshalled config with no panel used to write), is the single reviewer,
> exactly as before the panel existed. Only an array counts as set: `null`
> carries no panel to honour.

### refuseImplicitWorkDirConfig

> refuseImplicitWorkDirConfig refuses to run when a `clankerbar.json` is sitting
> in the process working directory and no --config was given.
>
> It used to be the FIRST discovery candidate, stat'd as a relative path against
> whatever cwd the process happened to have, ahead of the operator's own
> ~/.config file, with no ownership or provenance check. Everything in it is
> load-bearing for an unattended run: `prompt` is the entire instruction the
> fresh session gets, `settings_path` is the headless allow/deny policy,
> `env` is arbitrary environment for the child (including secrets read off
> disk), and `backlog_url` is the one origin the account-scoped API key may be
> sent to (CLA-257 - a fix that rests on this file being the operator's).
>
> The working directory is exactly where that trust does not hold. It is a
> checkout, which may have been cloned from anywhere, and the sessions this
> daemon spawns run there WITH EDIT PERMISSION - so a session can write the file
> that owns the NEXT run's prompt, policy and environment. Config is read once at
> startup, so the damage lands on tomorrow's cron, not on the run that wrote it.
>
> REFUSED, NOT IGNORED. Ignoring it would silently fall back to the home config
> or to bare defaults, which for an operator who genuinely relied on cwd
> discovery means an unattended loop running a different prompt against a
> different backlog and saying nothing - the same class of silent-wrong-config
> this closes. A refusal costs the honest operator one flag they are already
> shown (`-c ./clankerbar.json`), and turns the hostile case into a stopped
> daemon a human looks at rather than a captured one nobody does.

### Permission bits that disqualify a file this loop is about to TRUST

> Permission bits that disqualify a file this loop is about to TRUST.
>
>   - groupOtherWrite: someone other than the owner can REWRITE it. Fatal for a
>     config file, whose contents choose what an unattended session is told to do
>     and what it is allowed to call.
>   - groupOtherAccess: someone other than the owner can READ it (write implied).
>     Fatal for a secret - the whole point of the `@path` indirection.

### readOwnerOnly

> readOwnerOnly reads a file the loop is about to trust, refusing it when anyone
> but the owner (or root) could have decided its contents.
>
> The file's own mode is taken from the OPEN FILE HANDLE, not from a separate
> os.Stat of the path, so there is no window between the mode that was checked
> and the bytes that were read: a file swapped after the check is a different
> inode, and this reads the one it vetted. Symlinks are followed (the target's
> mode is what matters), which is deliberate - an operator pointing at
> ~/.secrets/token through a symlink is normal.

### vetTrustedFile

> vetTrustedFile is the three questions a mode check has to answer to mean what
> it says. Only the first is about the file's own bits:
>
>  1. Can group or other CHANGE (or, for a secret, READ) it?
>  2. Is it OWNED by someone else? A 0600 file is unreadable by a peer, so under a
>     normal uid this is nearly self-enforcing - but a root daemon reading a
>     user-owned config is exactly the case where mode alone says "fine" and the
>     file is under someone else's control.
>  3. Can group or other REPLACE it by writing its directory? A 0600 token in a
>     0777 directory can be unlinked and recreated by any local account, which
>     defeats (1) entirely. This checks the immediate parent only, not the whole
>     ancestor chain - a world-writable /home is its own, larger problem, and
>     OpenSSH's StrictModes draws the line in the same place. The sticky bit is
>     the documented exception (/tmp), where only an entry's owner may remove it.

### refuseInsecureMode

> refuseInsecureMode vets a file the loop hands ONWARD rather than reads, so the
> same rule covers a path whose contents are another program's business.
>
> Only an insecure-mode verdict is returned: absence and unreadability belong to
> whichever check already reports them (for settings_path, `doctor`'s permissions
> check, which says what a missing policy file means far better than a config
> error could).

### underWorkDir

> underWorkDir resolves a RELATIVE path the way the spawned harness will: against
> the session's working directory, not against the daemon's.
>
> The two are routinely different, and every one of these paths is handed to the
> child verbatim while `cmd.Dir` is the workdir (harness/claude.go), so a
> relative value used to be read by US against one directory and by the CHILD
> against another. That is not a cosmetic mismatch: `mcp_config_path: ".mcp.json"`
> with a workdir elsewhere made checkMCPConfigOrigins vet a file that did not
> exist (absent is the one benign case, so the gate passed) while the session
> loaded the checkout's file with its own origins and its own `Authorization`
> headers - the CLA-257 property defeated by a relative path, with `doctor` green
> throughout. Resolving here means the file that is VETTED is provably the file
> that is USED.
>
> An empty workdir means the child inherits our cwd, so relative already means
> the same thing to both and is left alone.

### RedactURLCredentials

> RedactURLCredentials removes any URL userinfo - the `user:password@` part of
> `scheme://user:password@host/...` - from raw, so a clone URL base carrying
> embedded mirror credentials can be logged or quoted in an error without
> writing the credential down. The host and path are kept: they are what makes
> a log line or a failure diagnosable.
>
> An scp-style ssh prefix (`git@github-personal:`) is not a URL and its user is
> a plain login name, not a credential, so it is returned unchanged. A value
> with no userinfo is returned byte for byte.

### ValidateCloneURLBase

> ValidateCloneURLBase checks that base can be concatenated with an
> `owner/name.git` identity to make ONE clone URL. The value is a prefix, so
> the only structural mistake that is both common and silent is a missing
> separator - `https://github.com` would otherwise produce
> `https://github.comowner/name.git` - and that is refused here.
>
> Accepted shapes:
>
> 	https://github.com/        (any URL-style base, ending in /)
> 	file:///srv/mirrors/       (a local mirror)
> 	/srv/mirrors/              (a bare path prefix)
> 	git@github-personal:       (an scp-style ssh host alias)
>
> An empty value is the default and therefore valid. Errors are recorded
> with the key name by the caller, so they describe only what is wrong with
> the value.


## `internal/config/maps_embed.go`

### mapsembed.go

> maps_embed.go — maps_session.embed_hunks (CLA-871).
>
> The embedded hunk is UNTRUSTED repository content inside the prompt, so it is
> never pasted loose: it sits in a fenced block whose fence is longer than any
> backtick run the hunk contains, and a hunk line equal to the done marker is
> escaped. Either way a hunk cannot close its own fence or start a done report.

### EmbedMapsSessionHunks

> EmbedMapsSessionHunks returns items with each M item followed by its hunk in
> a fenced diff block. items[:len(hunks)] are the M items, in map order.
>
> The budgets cap the hunk TEXT: fences and marker lines are fixed prompt
> overhead, not counted. A hunk over MaxBytesPerItem is cut at a line boundary
> and its item names the exact git command for the rest. Once the next hunk
> would take the total past MaxBytesTotal, embedding stops for that item and
> every item after it — those keep their plain text — and the returned index is
> where it stopped, so the caller can say so once in that chunk. A return of -1
> means embedding is off or every hunk was embedded.

### MapsSessionHunkBudgetNote

> MapsSessionHunkBudgetNote is the one-time note placed where the total budget
> stopped embedding. The caller inserts it at the first unembedded item, so the
> items above it in the chunk still carry their diffs.
>
> The stop condition is that the NEXT hunk alone would pass the total, not that
> every byte was used, so the note says which budget was hit rather than
> claiming it is spent.


## `internal/config/mcphanded.go`

### MCPConfigNone

> MCPConfigNone is the mcp_config_path value that means "no MCP config": set it
> at any level (run-wide, per-harness, per-project) to opt out explicitly. It
> exists because an empty value cannot opt out - empty re-runs the harness-blind
> <workdir>/.mcp.json discovery, whose Claude schema opencode cannot read
> (CLA-318). "none" survives Validate untouched, skips the origin
> checks (there is no file to check), and resolves to nothing at invocation time
> for every harness. A single-project run that sets it loses the slug its poll
> would otherwise derive from .mcp.json, so name the project in backlog_url
> (/mcp/<slug>) to keep the poll project-scoped.

### mcpConfigHandedTo

> mcpConfigHandedTo narrows a resolved MCP config path to what one harness may
> actually be handed (CLA-318).
>
> The auto-discovered file is always named .mcp.json - Claude Code's project MCP
> config, whose schema opencode cannot read and whose wiring codex never
> sees. That one discovery feeds two consumers with conflicting needs: the
> driver's poll reads the slug out of the file whatever harness runs, while a
> SESSION should receive only a file in ITS schema. So Validate keeps filling
> the field for everyone, and this gate decides at hand-off whether the file
> reaches the session:
>
>   - codex (MCPConfigUnused): nothing is ever handed - the adapter drops the
>     path anyway, so handing it nothing costs nothing.
>   - opencode (MCPConfigNative): a file named .mcp.json is Claude-shaped by
>     convention and is refused here rather than passed as OPENCODE_CONFIG;
>     any other path passes verbatim, because the operator named that file
>     themselves and doctor still reports on its shape.
>   - claude and copilot (MCPConfigClaudeJSON): everything passes, exactly as
>     before - copilot reads the same `mcpServers` object.
>
> An unknown harness returns the path unchanged: Validate refuses those long
> before this runs, and refusing to guess beats inventing policy here.

### WithheldMCPConfig

> WithheldMCPConfig returns the file an OPERATOR named for this harness (and
> project, when p is non-nil) that mcpConfigHandedTo withholds from its sessions,
> or "" when there is nothing to report. It follows ResolveMCPConfig's precedence
> so doctor judges the file the loop would pick.
>
> A file Validate found in the workdir is not reported: the operator never chose
> it, and the gate dropping it is the intended behaviour. "none" is not a file.


## `internal/config/modelroutes.go`

### ModelRoutes

> ModelRoutes is an ordered list of model route aliases for ONE model field
> (CLA-782): the free route first, then the paid route of the same model, and so
> on. A single route is the historical shape and stays byte-identical on the
> wire, which is what keeps every existing config's Identity() where it was.
>
> The fallback exists because a provider's free tier can rate-limit or withdraw
> a route at any time, and the same model is available paid. The adapter runs
> the first route, and when it hits a usage limit mid-session it resumes the
> SAME session on the next route; the driver remembers which routes were limited
> so the rest of the run stays on the route that worked until the free one's
> reset.
>
> This is deliberately NOT a second kind of model resolution: ModelForPhase and
> ModelForTier keep returning the FIRST route, so every existing caller, log line
> and brief placeholder is unchanged, and a config that names one route (or a
> plain string) behaves exactly as it did before this type existed.

### UnmarshalJSON

> UnmarshalJSON accepts either the historical plain string or an ordered JSON
> array of strings. A JSON `null` or an empty string, like an absent key, is no
> routes at all. A
> non-string element is refused with a message naming the field's shape rather
> than the decoder's, because a mistyped route list must be visible in the
> config load that refused it.

### An explicit "model"

> An explicit `"model": ""` is the historical "no model", so it decodes to no
> routes rather than one blank route. A one-element slice is non-empty to
> `omitempty` (Phase.Model) and to len()-based checks (HarnessConfig.Empty),
> so keeping it would emit a key the string field omitted, moving
> Config.Identity(), and make a block holding only `"model": ""` read as
> configured. Only the exact empty string: a whitespace alias always
> marshalled as itself, and still does.

### MarshalJSON

> MarshalJSON emits a plain string for zero or one route and only uses the array
> form when there is a choice to make.
>
> The zero case is load-bearing for Config.Identity(): these fields marshal
> without `omitempty` (the historical tags), so an unset field used to write
> `"model":""` and must keep writing exactly that. A config that names one route
> likewise hashes the same bytes as the identical string config, so moving a
> config to the new shape is not by itself an identity change.


## `internal/config/models.go`

### ModelForTier

> ModelForTier resolves one phase's tier to the model alias its session runs on.
>
> It is TOTAL — there is no input for which it fails — because the fallback chain
> is the design and not an error path: an unset tier, a tier the operator's map
> does not name, and a tier mapped to a blank string all resolve to the run-wide
> Model, and a blank Model resolves to "" (the harness's own default). That is
> what keeps a config written before tiers existed behaving exactly as it did.
>
> Everything is trimmed on the way through, which is the load-bearing half: a
> tier or a `"model": " "` that is blank-but-not-empty must resolve to the same
> nothing an absent one does, or a whitespace alias reaches the child's model
> flag and the session dies on an alias no provider has.
>
> `ok` is false only when a tier was NAMED and resolved to nothing — the typo
> case. The caller says so in the iteration log rather than refusing: a mistyped
> bucket should cost a log line and a run on the default model, not a stopped
> unattended run at 3am. It is deliberately NOT reported for an unset tier,
> which is the ordinary case and not a mistake.

### SessionFor

> SessionFor resolves one harness's invocation fields.
>
> The run-wide fields fill the gaps for the TOP-LEVEL harness only. For any
> other harness the block stands alone: every field in it is a dialect of that
> harness (see HarnessConfig), so inheriting the top level's would hand a claude
> config dir and a Claude-shaped `.mcp.json` to opencode, which does not ignore
> the difference — it refuses to start. An unconfigured harness therefore
> resolves to empty fields, and each adapter falls back to its own ambient
> defaults, which is a session that may lack tools rather than one that dies on
> somebody else's config. Validate refuses the unconfigured case up front; this
> stays total so nothing downstream has a second error path.

### ModelForPhase

> ModelForPhase resolves one phase's model: its own pinned alias (Phase.Model,
> which is how a review-panel member names the model it runs on), else its tier
> through the tier map of the harness THAT phase runs on, else that harness's
> default model.
>
> Same total fallback chain as the single-harness case it replaces — unset tier,
> unknown bucket, and a bucket mapped to blank all resolve to that harness's
> default model, and an empty default means the harness's own — with the
> per-harness lookup being the whole difference. A bucket name is portable and
> the alias in it is not, so "strong" on a claude phase and "strong" on an
> opencode phase are two different aliases under one policy word.
>
> `ok` is false only when a tier was NAMED and resolved to nothing, exactly as
> before: the caller logs it and runs on the default rather than stopping an
> unattended run over a typo. A phase on a harness with no tier map at all
> therefore reports the typo case for any tier it names, which is honest — the
> bucket really does resolve to nothing there.

### ModelRoutesForPhase

> ModelRoutesForPhase is ModelForPhase with the whole ordered route list: the
> phase's own pinned routes, else its tier's routes through the tier map of the
> harness that phase runs on, else that harness's default routes. The first
> entry is exactly what ModelForPhase returns, so a single-route phase (every
> config written before CLA-782) resolves identically through both.
>
> The sibling exists because two callers need the whole list: the driver builds
> the adapter's fallback chain from it, and doctor's credential check must see
> every route the run may ask for, not only the first. `ok` keeps ModelForPhase's
> meaning: false only when a tier was NAMED and resolved to nothing.

### ModelIDsFor

> ModelIDsFor returns every model id the sessions on harnessName will actually
> run with, deduplicated and sorted: each effective phase that resolves to that
> harness (so the review panel and the unphased single phase are both included),
> with its model resolved through the same chain the driver uses - Phase.Model,
> else Phase.Tier through that harness's tier map, else that harness's default.
>
> It answers "which models is this config going to ask for", which is a question
> about the RUN rather than about any one field, and there is exactly one caller
> that needs it: `doctor`'s opencode credential check has to know which
> providers those models name before it can say whether a stored credential
> covers them (CLA-687). Reading `model` alone would miss the tier and panel
> paths, which is precisely where a config pins a provider the top-level field
> never mentions.
>
> An EMPTY id is skipped rather than reported: it means the harness's own
> default model, which this config does not name and cannot check. A caller that
> gets nothing back knows only that no session here names a model.

### ResolveMCPConfig

> ResolveMCPConfig picks the MCP config file one session gets, given the harness
> it runs on and the project scope it runs in.
>
> The file carries TWO facts at once — which project (its `/mcp/<slug>` URL) and
> which schema (the harness's) — so the rule has to satisfy both, and it lives
> here because two callers must agree on it: the driver building an invocation
> and `doctor` reporting on what that invocation will be. A preflight that
> resolved this differently from the loop would certify a file no session ever
> reads, which is the exact failure MCPConfigUse was added to end.
>
> projectMCP / projectPerHarness are the project's own fields (empty and nil for
> a single-project run). Precedence: the project's file FOR THIS HARNESS, else
> the project's file when this is the top-level harness — it is that harness's
> schema and no other's — else the harness block's own path.
>
> The winner is then narrowed by mcpConfigHandedTo (CLA-318): a file this
> harness cannot be handed (a .mcp.json under opencode, anything under codex,
> or "none") resolves to "" rather than falling through to a lower-precedence
> file. Falling through would hand a project's sessions the harness block's
> file, which in a multi-project run names some other project. The poll's slug
> reads the raw field instead (see BacklogEndpoint).

### panelMemberHarnesses

> panelMemberHarnesses is the harness each panel member resolves to when the
> panel actually expands, in member order. A member with no harness of its own
> inherits the review phase's (see reviewPhaseHarness). Empty when no panel is
> set or no phase would expand — an inert panel spawns nothing.
>
> PhaseHarnesses and SpawnedHarnesses use it to add the members to the harnesses
> a config DECLARES and SPAWNS while keeping their own iteration over c.Phases
> (so an absent panel answers exactly as it did before CLA-639).

### PhaseHarnesses

> PhaseHarnesses lists every harness this config DECLARES — the run-wide one and
> every phase's — deduplicated, with the run-wide one first and the rest in
> sequence order.
>
> Callers are the ones that must reason about ALL of them rather than the
> top-level name alone: `doctor` checking binaries, config dirs and permission
> policy, and Validate checking that each is registered and configured. Reporting
> on `harness` alone is how a mixed-harness config earns a green preflight and
> then dies on its second phase.
>
> **Declared, not spawned** — and the difference matters, so pick deliberately.
> A declared harness must be USABLE whether or not today's phases reach it, which
> is why every caller above wants the run-wide one included. For the other
> question — what will this run actually do — use SpawnedHarnesses below, which
> omits a run-wide harness that every phase overrides. Asking this one there
> produces confidently false statements about a harness no session runs on.

### SpawnedHarnesses

> SpawnedHarnesses lists every harness this config will actually START A SESSION
> on: the run-wide one for an unphased run, and otherwise exactly the harnesses
> its phases resolve to — plus the conflict-only session's own harness (CLA-759)
> when the `conflict` block names a different one, because the supervisor starts
> a real session there on a conflicted approved task. Deduplicated, in sequence order.
>
> It differs from PhaseHarnesses in one shape, and the difference is the whole
> reason it exists: when EVERY phase names its own harness, the run-wide
> `harness` field is DECLARED but never spawned - `Validate` does not refuse
> that, and the only two spawn sites in the driver (the phase's Invoke and its
> supervised-wait Probe) are both phase-driven. PhaseHarnesses seeds c.Harness
> unconditionally, which is right for the questions IT answers - is every
> declared harness registered and configured, is its binary on the PATH - since
> a declared harness must be usable whether or not today's phases reach it.
>
> It is wrong for any question of the form "what is true of the sessions this run
> will actually run", which is every inert-dial judgement in `doctor`'s budget
> check. Asked of PhaseHarnesses, those get three separate false statements out
> of a config whose phases all override: a live cost ceiling called INERT because
> a never-spawned codex is cost-blind; an unreachable per_harness block for that
> codex counted as a live ceiling; and an inert `max_session_wall_clock` passed
> over in silence because a never-spawned opencode would have honoured it.
>
> So: ask PhaseHarnesses what is CONFIGURED, and SpawnedHarnesses what will RUN.


## `internal/config/opencodeambient.go`

### This file answers one question

> This file answers one question: which opencode config files will a session
> merge that the DRIVER never named, and does one of them point the `clankerbar`
> MCP server at a different project than the sessions loading it are meant to
> work?
>
> opencode 2.x discovers config on its own, in merge order (read off `opencode
> debug config` against 2.0.32): the global config dir, then OPENCODE_CONFIG
> (the file this driver names), then the project-level `opencode.json` and
> `.opencode/opencode.json` it finds from the INSTANCE DIRECTORY, then
> OPENCODE_CONFIG_CONTENT. The global dir and the project files are not ours,
> and the project files merge AFTER the file we point at - which is how, on
> 1.x, sessions spawned for one project spent a fortnight claiming, working and
> handing back another project's tasks: 14 CLA-* leases held by sessions
> labelled [ezyapp], and no ezyapp task ever worked (CLA-441).
>
> harness/opencode.go pins the driver's own config through
> OPENCODE_CONFIG_CONTENT, which opencode merges after every layer this driver
> or a checkout can write, so none of these files can redirect a spawned
> session any more. This check is what stops the fix from being invisible: a
> file that still names the wrong project is a live trap for every interactive
> session and for any future path that loses the content pin, and it is one
> line of output to say so.
>
> It WARNS rather than refusing, deliberately - see OpencodeAmbientConflicts.

### The file names opencode discovers by itself

> The file names opencode discovers by itself. `opencode.json` is the name
> 2.0.32 was observed loading in both the global dir and a project; the `.jsonc`
> spelling and the global `config.json` are carried over from 1.x, where a live
> run logged loading all three, and are scanned because a stale file there is
> cheap to report and an unread one costs nothing. `config.json` is a GLOBAL
> name only: a project-level `config.json` is an ordinary file thousands of
> repos carry for something else entirely.

### opencodeGlobalConfigDirs

> opencodeGlobalConfigDirs is the directory opencode 2.x reads global config
> from: `$XDG_CONFIG_HOME/opencode`, or `~/.config/opencode` when that is unset
> - unless OPENCODE_CONFIG_DIR is set, which REPLACES it (verified against
> 2.0.32). The adapter exports `config_dir` as OPENCODE_CONFIG_DIR, so a run
> with a config_dir reads that directory and not the default one. 2.x does NOT
> read ~/.opencode, ~/.claude, ~/.agents or ~/.config/opencode2 as config
> (verified; 1.x read ~/.opencode).

### OpencodeAmbientConflicts

> OpencodeAmbientConflicts lists every discovered opencode config whose
> clankerbar server names the wrong project. Empty when the run spawns no
> opencode session, when no such file exists, or when every one of them agrees
> with the project it would serve.
>
> A WARNING, not a refusal, which is a departure from how this package treats
> every other slug disagreement (those are hard errors: see the projects[i] slug
> check and the harnesses.<name> one). Three reasons, and the first is the one
> that decides it:
>
>   - The file can no longer do the damage. OPENCODE_CONFIG_CONTENT is merged
>     after every layer listed above, so a spawned session's `mcp.clankerbar` is
>     the driver's whatever these files say. Refusing to start over a disarmed
>     trap trades a silent wrong-project drain for a loud no-drain-at-all.
>   - The file is frequently not the operator's to change. A project-level
>     `opencode.json` is a CHECKED-IN file of somebody's repo - this CLI's own
>     repo carries one - so a run that declares two repos would be refused
>     startup over a file that is correct for its own repo and merely foreign to
>     this run.
>   - It still needs saying, because it is not harmless: every INTERACTIVE
>     opencode session in that tree gets the wrong backlog, which is exactly how
>     this was found.
>
> Nothing here reads a file the run would not otherwise hand to a session, and a
> file it cannot parse is passed over rather than guessed at: this is a
> diagnostic, and a diagnostic that fails closed would be a startup outage on a
> malformed file that opencode itself tolerates.

### opencodeAmbientScopes

> opencodeAmbientScopes lists where to look. The global config dir is checked
> against EVERY slug the run drains - it is loaded by all of them, so it is only
> wrong if it agrees with none. Each session directory is checked against the
> one project whose sessions start there.
>
> The session directories are the project's workdir AND every checkout it
> declares, because CLA-437 spawns each session in the task's own repo: a
> checked-in `opencode.json` in any declared repo is discovered from there, which
> is the second of the two live mechanisms in CLA-441 and the one no amount of
> tidying the global file would have found.

### opencodeConfigClankerbarSlug

> opencodeConfigClankerbarSlug reads one discovered opencode config and returns
> the project slug its `clankerbar` MCP server names, or "" when the file is
> absent, unreadable, unparseable, carries no such server, has it DISABLED, or
> gives it a URL that is not an /mcp/<slug> endpoint.
>
> Only the server literally NAMED `clankerbar` counts. That is narrower than
> mcpURLFromConfig, which also accepts "whichever entry is handed the API key",
> and the narrowness is the point: opencode merges these files KEY BY KEY, so
> only an entry sharing the driver's own key can displace it. A differently
> named block pointed at another project - `clankerbar-interactive`, the shape
> the operator's own global config now uses - displaces nothing and must not be
> reported as if it did.
>
> `"enabled": false` is respected for the same reason: opencode does not start a
> disabled server, so it cannot redirect anything. This is deliberately NOT done
> in readMCPServers, whose callers are security gates - there, an entry the file
> merely CLAIMS is disabled is still an entry the operator should see disclosed,
> and a later merge layer can flip the flag back.

### sessionShapingKeys

> sessionShapingKeys names what a discovered opencode config carries that
> decides what a session IS rather than what it talks to.
> checkDiscoveredMCPConfig REFUSES these, but only on the file
> `mcp_config_path` resolved to - a file opencode discovers by itself never
> reaches that gate, and it is merged into every session all the same.
>
> `permission` is on the list, though on 2.x a top-level block in a lower
> layer was observed to neither loosen nor lock out the policy the adapter pins
> (CLA-675 review, 2.0.32). It still shapes every INTERACTIVE session in that
> tree, which is what this report is for. (On 1.x it was worse: the merge kept
> the file's key order ahead of the policy's `*` catch-all and could deny every
> read, CLA-441.)
>
> An agent-level permission block - 1.x `agent.<name>.permission`, 2.x
> `agents.<name>.permissions` - and `default_agent` are the 2.x mechanism that
> DOES bite: verified on 2.0.32, an agent's own block overrides the top-level
> policy for that agent, and default_agent routes a session to an agent of the
> file's choosing. The adapter pins the policy on every built-in agent and
> passes --agent build, which the conformance suite shows defeats both, so this
> is reported rather than refused: refusing would stop the fleet over a file the
> pin already neutralises, often a checked-in file of somebody else's repo.
>
> `plugin` is code opencode loads and runs at session start. An `mcp` entry
> with a `command` is the same threat wearing a different key, and
> startsProcess is the predicate the CLA-266 gate already applies to it, so it
> is reported here rather than left as the conspicuous omission from a list
> about what a session runs.
>
> `agent` is reported only for an entry carrying an AUTHORITY key - see
> authorityAgents. The unfiltered version fired on the operator's own global
> config, whose whole agent block picks a cheap model for generating titles,
> and a WARN like that is how an operator learns to skim the ones that matter.

### authorityAgents

> authorityAgents names the agents in a discovered config's `agent` block that
> carry a key deciding what that agent MAY DO - its permissions, its tool set,
> its mode, whether it is disabled, or its prompt. An agent entry that only
> picks a model or a temperature changes cost and style, not authority, and is
> not worth an operator's attention on every doctor run.
>
> An `agent` block this cannot parse counts as authority-carrying: the report is
> the conservative side, and a block shaped in a way this does not model is
> precisely the one nobody has looked at.


## `internal/config/origin.go`

### This file holds one rule

> This file holds one rule: WHERE the operator's API key is allowed to go.
>
> CLANKERBAR_API_KEY is an ACCOUNT-scoped bearer token — it covers every project
> the operator is a member of, not just the one being drained. Before CLA-257 its
> destination was derived from `<workdir>/.mcp.json`: a file that lives inside the
> checkout the loop is pointed at, that repos legitimately commit (this one does),
> and that named any scheme and any host it liked. Cloning a repo and pointing
> `workdir` at it — the documented usage — was enough to ship the key to a third
> party in cleartext, and `doctor` reported PASS for any host that answered 200.
>
> So the credential origin is now taken ONLY from the operator's own config
> (`backlog_url`, default `https://clankerbar.com`), and a discovered `.mcp.json`
> contributes at most a project SLUG. A self-hosted plane is still supported; it
> just has to be stated in the operator's config file rather than inferred from a
> file inside a checkout.

### mcpFile

> mcpFile is the union of the two schemas a harness MCP config can be in, because
> the same `MCPConfigPath` field serves every harness (ResolveMCPConfig decides
> which file a session is actually handed):
>
>   - `mcpServers` — Claude Code's `.mcp.json`, passed as `--mcp-config`.
>   - `mcp` — opencode's config, exported as `OPENCODE_CONFIG` (harness/opencode.go).
>
> Decoding both from one file is deliberate. Reading only the shape the current
> harness uses would let a workdir file carry a benign `mcpServers` block for
> `doctor` to report on and a hostile `mcp` block for the session to actually
> use — a decoy that reads green.

### Permission

> The opencode-schema keys that decide what a session IS rather than what it
> talks to. Modelled because OPENCODE_CONFIG makes the file the session's
> ENTIRE config (harness/opencode.go), so a workdir-discovered file carrying
> any of these does not merely add servers — it overrides what the driver
> pins (CLA-266):
>
>   permission — replaces the fail-closed permission policy the adapter
>                exports; the CLA-260 threat with the safety rail removed;
>   plugin     — code opencode loads and runs at startup;
>   agent      — replaces agent definitions, including their modes and
>                per-agent permissions.
>
> Left RAW because "present at all" is the only fact this needs: every value
> shape counts (object, array, bool, string), and only an absent key or a
> JSON null means unset — the same reading Go's own optional types give them.

### UnmarshalJSON

> UnmarshalJSON decodes the union mcpFile shape and additionally flattens the
> opencode 2.x `mcp.servers` dialect into MCP, so every consumer (the
> credential-origin gates, the local-process disclosure, the ambient-config
> audit, the slug derivation) sees an opencode 2.x config's servers exactly as
> a 1.x config's. Both dialects are accepted by opencode 2.0.32, and the
> adapter rewrites 1.x entries into `mcp.servers` before a session sees them
> (docs/opencode-build.md).
>
> The two dialects cannot collide on a real file: opencode 1.x writes `mcp` as
> a map of server-name -> entry, opencode 2.x writes it as an object with a
> `servers` sub-map. The one ambiguous document — a v1 file whose only server
> is literally named "servers" — is resolved by the VALUE: the v2 container's
> `servers` value is an object of entries, which decodes into mcpEntry as all
> zero (its fields are entry fields, url/command/…, and none match), where a
> v1 entry object carries at least one of those fields. An all-zero "servers"
> entry is therefore read as the v2 container and flattened.

### Enabled

> Enabled is opencode's per-server switch. A POINTER because absent means
> enabled and `false` means disabled, and a plain bool cannot tell those
> apart - which would read every entry of every Claude-shaped file, where
> the key does not exist at all, as switched off.
>
> Read by ONE caller: the ambient-config check, where a disabled server
> genuinely cannot redirect anything because opencode does not start it. It
> is deliberately NOT consulted by readMCPServers, whose callers are the
> credential-origin and local-process gates - see
> opencodeConfigClankerbarSlug for why a security gate must keep looking at
> an entry the file merely CLAIMS is off.

### readMCPFile

> readMCPFile parses a harness MCP config into the whole mcpFile model: both
> server blocks and the opencode-schema keys beside them.
>
> It FAILS CLOSED like readMCPServers: this feeds security gates, and the same
> file is handed onward to the harness, so "I could not read it" must not read
> as "there is nothing in it to object to". An absent file is the one benign
> case — there is then nothing to hand over either.

### Only an entry that would actually START something carries a command

> Only an entry that would actually START something carries a command.
> Raw "null", and args-without-command, used to be reported as a
> process (doctor named an entry whose command read "null --serve"),
> which made this disclosure disagree with startsProcess — the same
> predicate CLA-266's gate applies to the very same entry. Disclosure
> and gate must answer "does this start a process" identically: a WARN
> listing entries that never run trains the operator to skim it.

### LocalMCPServers

> LocalMCPServers lists every entry in the resolved MCP config(s) that spawns a
> local process rather than talking to an http server.
>
> Why this is worth surfacing (CLA-260): `mcp_config_path` defaults to
> `<workdir>/.mcp.json`, the file is handed to the harness whole
> (`--mcp-config`, or as opencode's entire `OPENCODE_CONFIG`), and an entry
> carrying a `command` is started at MCP init - BEFORE any tool-permission rule
> has an opinion, which is the same reasoning checkMCPConfigOrigins already
> applies to a local server handed the API key. checkMCPConfigOrigins leaves
> entries that neither are named `clankerbar` nor reference the key alone, and
> that is still the right call for an ORIGIN check, but it means a checkout can
> declare a process the next unattended session will run.
>
> What happens to them depends on how the file was found (CLA-266). A file the
> operator NAMED is disclosed only — local MCP servers are a normal, wanted
> thing to put behind an explicit config line, and this listing is what doctor
> shows. A DISCOVERED one (Validate defaulting mcp_config_path to
> `<workdir>/.mcp.json`) is refused outright by checkDiscoveredMCPConfig before
> any session can read it, so from a validated config onward everything this
> returns lives in a file the operator pointed at.

### MCPClankerbar

> MCPClankerbar reports the usable clankerbar server the file at path declares:
> an entry named `clankerbar`, or one referencing `CLANKERBAR_API_KEY`, that is
> not explicitly `"enabled": false`. It returns that entry's URL ("" for a
> local-command entry, which has none) and whether such an entry exists at all.
>
> This is the "always has clankerbar set up" probe (CLA-448): it feeds both
> Validate's refusal of a named MCP config that is present and silent about
> clankerbar, and doctor's sessionCheck verdict for the opencode harness. It is
> deliberately STRICTER than checkMCPConfigOrigins, which only constrains where
> the key may go — a file with no clankerbar entry at all has nothing to check
> there and passes, exactly the tool-less-session config this predicate exists
> to refuse.

### checkMCPConfigNamesClankerbar

> checkMCPConfigOrigins refuses a harness MCP config that would send the API key
> somewhere this config does not trust.
>
> The driver's own requests are already safe — it builds every credentialed URL
> from CredentialOrigin — but the same file is handed to the harness
> (`--mcp-config`, or `OPENCODE_CONFIG`), whose session carries
> CLANKERBAR_API_KEY in its environment and will happily substitute it into
> whatever `Authorization` header the file declares. Policing only our own client
> would fix the smaller half of the leak.
>
> Only entries that can carry the key are constrained: the one named `clankerbar`
> (the documented wiring) and any whose headers or env reference the variable. An
> unrelated MCP server in the same file — a docs server, a browser driver — is
> none of our business and is left alone.
>
> A refusal, not a silent drop: dropping the file would spawn a session with no
> clankerbar tools at all, which burns an iteration and reads as "the backlog was
> empty". The operator gets a named host and a remedy instead.
> checkMCPConfigNamesClankerbar refuses a resolved MCP config file that is
> PRESENT but silent about clankerbar (CLA-448): a file a daemon hands to every
> session, whose sessions would therefore start with no clankerbar tools at
> all — the shape that burned three clankers on CLA-351 and CLA-377 before
> parking them. A missing file keeps today's behavior (doctor's no-.mcp.json
> WARN covers it), and an empty path is the harness's fallback to its own
> config dir, which this check must not second-guess.


## `internal/config/pausewindows.go`

### PauseWindows

> PauseWindows is the operator's recurring "start no new work" schedule: local
> wall-clock windows in which the loop spawns no new session. It is checked
> between iterations exactly like the console pause, and at every phase boundary
> (CLA-906): a running session is never interrupted, but the next phase, a
> handoff respawn or a drain's first session waits out the window with the claim
> held and its lease renewed.
>
> It exists for pricing and quiet hours - "do not spend on peak rates" - which
> is why the windows are wall-clock times in an operator-named zone rather than
> a UTC offset that would drift across a DST change.

### Validate

> Validate reports the first problem, naming the window index and field.
>
> A nil schedule, and one with no windows, are valid: the scheduler is optional,
> and an absent one must never be read as "paused".


## `internal/config/phases.go`

### The review panel (CLA-639, design §5.4 / A4 / B10)

> The review panel (CLA-639, design §5.4 / A4 / B10). When a config SETS
> `review_panel`, each built-in review phase expands into ONE review phase PER
> MEMBER, run against the same reviewed tip, each writing its own sealed durable
> record under its own reviewer role (`review-1`..`review-N`, by member
> position). The members run concurrently by default, in private detached
> worktrees; see ReviewPanelConcurrency. The round closes only once every member has recorded or been
> recorded as failed — a sealed round is what keeps a later member from reading
> an earlier member's findings, and a usage-limited or dead member therefore does
> not stop the round.
>
> OPT-IN, and shipped with NO model-naming default and NO preset: the CLI cannot
> know which harnesses, providers or models an operator has, so it never names
> one (decision a0b1373b, superseding the trial panel of c8e7509a). A config
> that sets no panel runs its one review phase as it always has — the historical
> brief, the historical `review-1` role, the legacy fix path, byte for byte. See
> Config.ReviewPanel.
>
> INTERIM, deliberately (B10): task-state-machine build phase 4 removes phase
> logic from the CLI and routes findings as an `agent_review -> implementing`
> plane transition, and panel membership becomes PLANE phase state (that is the
> CLA-454 reshape). Until then the panel is local config and the members run
> CONCURRENTLY by default (CLA-697), each in its own detached worktree at the
> reviewed tip; `review_panel_concurrency: 1` restores the sequential sequence

### PanelMemberLabel

> PanelMemberLabel is the LOG LABEL of the member at position i: the name the
> operator gave it, or the deterministic `review-<i+1>` derived from its
> position. It names the member in daemon logs and iteration-log filenames; it
> is NOT the reviewer role the durable record is keyed on. Roles are positional
> (review-1..review-N), derived from the effective sequence by the driver, so
> naming a member cannot change which record it writes or which record the seam
> gate matches (CLA-639).

### Phase

> Phase is one session in a task's sequence. A task runs as an ordered list of
> them, each a separate harness process, so context resets between them.
>
> The point is a context reset the MODEL cannot perform for itself. A session's
> context grows monotonically and is re-read on every turn: one measured task
> (CLA-309, 2026-08-11) spent 66.2M tokens over 370 turns, and its last four
> deciles were 53% of that purely because each of those turns re-read ~250k. The
> served protocol already tells a session to shed at a safe checkpoint, and says
> in the same breath that the rule is a no-op if the harness cannot discard
> context. Inside a `claude -p` run there is no tool and no slash command a model
> can use to discard its own context, so for the agent that rule is dead letter.
>
> What was evaluated and NOT chosen, since the driver does have options here
> (claude 2.1.226): `--autocompact <auto|tokens>` compacts at a THRESHOLD, and
> `--continue` / `--resume` / `--fork-session` restore a session rather than
> shedding one. Compaction lands wherever the threshold falls, which is the
> specific thing the protocol warns about — a summarised context can garble the
> bar the work is judged against, and the session will not know. A phase boundary
> lands at a point where everything load-bearing is already durable elsewhere
> (code pushed, bar and decisions on the plane), so the next phase re-reads them
> from source instead of trusting a summary. Autocompact is also Claude-only,
> where a process boundary is a thing every harness has. Worth revisiting: the
> two compose, and autocompact inside a long phase costs nothing to try.
>
> Why phases and not a better-worded rule: "your job this session is
> implementation only, then stop" is a SCOPE instruction, and scope is the kind
> an agent follows reliably. The driver decides the boundary; the model only has
> to respect its brief. MaxTurns is the backstop for when it does not, and the
> existing salvage (CLA-314) commits and pushes whatever a cut-off phase left.
>
> The cost being paid: a session's fixed startup — re-reading the served
> protocol and the repo's agent guide — is paid per PHASE rather than per task.
> That is real, and it is why phases are opt-in and why splitting thin is a bad
> trade: the saving falls off per extra cut while the ramp does not.

### Prompt

> Prompt is what this phase's session is asked to do. Empty means "use the
> built-in prompt for Name". Two placeholders are substituted by the driver
> from the claim the previous phase left held: {{taskId}} and {{runId}}.
> They are the whole reason a later phase can RESUME a run instead of
> claiming a new task, and they are only available once a phase has held a
> claim — so they are meaningless in the first phase.

### MaxTurns

> MaxTurns caps the session's turns (harness permitting) so the boundary
> lands even if the model works past its brief. 0 = defer upward: the
> top-level Config.MaxTurns, then the built-in default (see
> Config.EffectivePhases). It is never "uncapped" — CLA-343 measured the
> unphased run that reading produced (1093 turns / 285.9M tokens, nothing
> able to stop it), so the cap chain always resolves to a number.

### MaxWallClock

> MaxWallClock caps this phase's SESSION by elapsed time (harness
> permitting), as the backstop for a harness whose CLI takes no turn flag —
> opencode is that harness today, where MaxTurns never reaches the process
> and a phase boundary rests on the prompt alone. 0 = defer upward to
> Config.MaxSessionWallClock; if that is unset too the cap is OFF.
>
> Unlike MaxTurns it has no built-in default, deliberately: turns are a
> harness-independent unit of work, while a wall-clock number that would be a
> runaway detector for one model/provider is a routine session for another,
> and clankerbar cannot know which the operator has. A default here would cut
> honest sessions off in the middle rather than catch a runaway.

### Tier

> Tier names a bucket in Config.Models — which of the operator's models this
> phase's session runs on. Empty = the run-wide Model, and that empty = the
> harness default, so an untouched config is untouched behaviour.
>
> It is a bucket NAME, never a model alias: putting "opus" here does not pin
> opus, it looks for a bucket called "opus". Keeping the two apart is what
> lets a tier policy be RECOMMENDED — see the README, where a phase producing
> a durable artifact is worth the strong bucket — while the models themselves
> stay the operator's, since clankerbar cannot know which ones they have or
> what they cost.
>
> Nothing is tiered by DEFAULT. defaults() sets no phases at all and the
> built-in briefs carry prompts only, so an untouched config runs every phase
> on the run's model. The policy is advice in prose, never a shipped
> assignment.

### Model

> Model pins this phase's session to a model alias directly, skipping the
> tier map. It is an operator-facing key (operator decision 3eddb7a5) on
> both the local file and the plane's stored document (CLA-598, mirrored by
> the plane in CLA-722), and it is what lets a REVIEW PANEL read as the
> harness/model pairs the operator wrote — see ReviewPanelMember, whose
> expansion sets it too. Empty = fall through to Tier, then the harness's
> default: an untouched config's resolution is unchanged.
>
> Unlike Tier it is an ALIAS, never a bucket name — the panel entries and the
> documented config examples name `opencode-go/gpt-6-luna` and friends, not a
> policy word. The two are deliberately not merged: keeping them apart is
> what makes a typo visible as a model the provider refuses, rather than a
> bucket that silently resolves to the harness default. A CONFIGURED phase
> sets one of them, not both (Validate refuses both, so which one wins is
> never ambiguous); a panel MEMBER may set both, and expansion lets the
> alias win, exactly as before.
>
> It carries an ordered ROUTE LIST since CLA-782: a plain string is the
> historical shape, and an array names the free route first and its paid
> fallback after it. The session starts on the first route and the opencode
> adapter may resume it on the next one when a provider usage limit denies
> the first; ModelForPhase still reports the first route, so a single-route
> (or string) phase resolves exactly as it did before.
>
> `omitempty` is load-bearing: Config.Identity() hashes the marshalled
> config, so an unset model must not add a key and move every identity.

### Harness

> Harness selects which coding-agent CLI runs this phase. Empty = the
> run-wide Config.Harness, so an untouched config runs every phase on one
> harness exactly as before.
>
> It exists because the phases a task splits into are not equally demanding.
> The implement phase can run on a cheap provider-agnostic backend; the
> review phase runs the adversarial gate the repo's workflow mandates, which
> wants the harness carrying the subagent machinery. Nothing in the phase seam
> resists this: each phase is already a FRESH session, seeded from the
> observed claim through the {{taskId}}/{{runId}} placeholders and
> Invocation.ResumeClaim, so there is no session state to carry across a
> harness boundary — only a claim, and the claim lives on the plane.
>
> What a mixed sequence DOES change is that the harness-shaped fields stop
> being run-wide: a claude `config_dir` handed to opencode, or a claude model
> alias handed to opencode's `--model`, is a session that dies at startup.
> That is what Config.Harnesses is for, and why Validate refuses a phase
> naming a harness with no block of its own.

### ReviewMethod

> ReviewMethod names how this phase produces its review findings. Empty and
> "single-pass" are the shipped single-pass review, unchanged. "maps-session"
> replaces the one session's prompt with the maps+notes turn sequence (see
> MapsSessionOptions): the driver builds the maps in the task worktree, sends
> them as chunks, and the findings turn writes the same done report a
> single-pass review would. Only a review phase may set it (Validate refuses
> otherwise), and only a harness that can continue one session across turns
> can run it (opencode today).

### ConfiguredIndex

> ConfiguredIndex is the index, in Config.Phases, of the phase the operator
> actually wrote for this one. It differs from the effective position only for
> an expanded review-panel member: every member reports the single `review`
> phase it came from. The driver reads a phase's CONFIGURED prompt through it —
> a handoff continuation must match the brief the operator asked for, not the
> member's resolved copy of it.

### ReviewPanelMember

> ReviewPanelMember is one reviewer in a review panel (CLA-639): a
> driver-spawned review phase of its own, on its own harness and model, run
> against the same reviewed tip as its siblings.
>
> A member is NOT a session: the driver still runs one review phase per member,
> concurrently up to Config.ReviewPanelConcurrency, and each writes its own
> sealed durable record under its own reviewer role (see Config.ReviewPanel).
> Two entries naming the same harness/model are two INDEPENDENT REVIEWS of the
> same revision — never a mistake to be deduplicated.

### Name

> Name is the member's LOG LABEL only. Empty = the driver derives it from
> the member's position: `review-1` .. `review-N`. It names the member in
> daemon logs and iteration-log filenames; it is NOT the reviewer role the
> durable record is keyed on. The role is always positional (`review-N`,
> counted over the review-shaped phases of the sequence), so naming a member
> cannot change which record it writes or which record the seam gate matches,
> and a resumed or retried round maps a member to the same record either way.

### Optional

> Optional marks a TRIAL seat: a reviewer the operator added to evaluate,
> not one the panel relies on (CLA-784). An optional member runs and records
> exactly like any other, but its failure is that member's own result and
> nothing more: whatever its session ends in — a transient blip, a usage
> limit, a stop, a death, a tool-loop kill, a setup error — it does not end
> the sequence, does not hand the panel's shared claim back, and does not
> trigger the run's limit wait. The required members' records still close the
> round and spawn the fix; a round where ONLY optional members recorded is no
> review at all, and no fix runs.
>
> Default false, and `omitempty` is load-bearing: Config.Identity() hashes the
> marshalled form, so a panel that does not write the key must hash exactly
> what it hashed before the field existed (a trial seat is a deliberate
> change to the panel, and the identity moving with it is correct).
>
> Validate refuses a panel whose members are ALL optional: with no required
> seat there is no review a round could ever be said to have produced.

### MapsSession

> MapsSession overrides the phase's maps-session options for this member.
> Nil inherits the phase's block, so a member that only tunes the options
> keeps the inherited method and profile. A member that NAMES a method takes
> its own options with it, and nil then means the benchmark defaults — a seat
> opting out of maps-session must not silently keep the phase's knobs for a
> method that has no use for them.

### HarnessConfig

> HarnessConfig is one harness's own invocation fields, for a run whose phases
> are not all on the same harness (CLA-366).
>
> Every field here has a run-wide twin on Config, and the twins are NOT shared
> across harnesses: they describe the top-level harness alone. That is the whole
> reason this type exists rather than a deeper fallback chain. The fields are
> harness dialects wearing generic names — `config_dir` is CLAUDE_CONFIG_DIR or
> OPENCODE_CONFIG_DIR or CODEX_HOME, `mcp_config_path` is a Claude-shaped
> `.mcp.json` for one adapter and an opencode-schema config for another (see
> harness.MCPConfigUse, which records that opencode REFUSES TO START on the
> Claude-shaped file), `model` is an alias only one provider has. So "inherit
> whatever the top level said" is not a helpful default but a guaranteed startup
> failure with the operator's own config as the cause. Better an empty field,
> which the adapter falls back on and `doctor` can talk about.

### Models

> Models is this harness's tier map (bucket name -> model alias), the
> per-harness twin of Config.Models. A phase's Tier resolves against the map
> belonging to the harness that phase runs on, because a bucket NAME is
> portable across harnesses and the alias inside it is not — which is what
> lets one `"tier": "strong"` mean opus here and something else there. Each
> bucket's value also accepts the ordered route-list form (CLA-782).

### Env

> Env is this harness's own extra session environment, overlaid ON TOP of the
> top-level env map (per-key winner) — see Config.Env for what the map holds.
> Unlike the fields above it does NOT inherit the top-level value only for the
> run-wide harness: environment is not dialect-shaped, so every layer of the
> overlay applies to every harness, with SessionEnv stacking them in
> most-specific-wins order.

### phaseNameRe

> phaseNameRe is what a phase name may contain, because it becomes part of an
> iteration log's filename. It is the PLANE's own pattern verbatim
> (/^[a-z0-9]+(-[a-z0-9]+)*$/, RUN_CONFIG built-ins aside): a proposal is
> ratified by that validator, so a name this file accepts but the plane refuses
> (a double hyphen, a leading or trailing one) would be a config `propose-config`
> could never lift.

### The placeholders a phase prompt may carry, substituted by the driver f

> The placeholders a phase prompt may carry, substituted by the driver from the
> claim the previous phase left held. {{branch}} names the recorded, verified
> hand-off branch (CLA-457): the implement phase's checkpoint is gated on it
> being real and on the origin remote, so the review brief can name it rather
> than asserting an unverified "an earlier session already implemented".
>
> {{prBase}} names the branch the TASK'S REPO's PRs target (CLA-608), resolved
> per repo at phase-spawn time (config.PRBaseFor, else loop.Driver.prBaseFor).
> Unlike the other three it is not a per-run fact and rides EVERY built-in
> phase, the implement phase included: the implement brief's resumed-work rule
> needs a branch to name, and that rule runs in phase 1, before any claim
> exists for the other placeholders to come from. It replaced literal
> "(staging)" text in these briefs — the literal was wrong for every repo
> whose PRs do not target staging, and the resolution above is what knows.

### PhaseBasePlaceholder

> PhaseBasePlaceholder names the commit a brief's `git diff` starts from:
> the merge-base of origin/<prBase> and the worktree tip, as the runner
> computed it during worktree prep, or origin/<prBase> when no SHA was
> available. Never a bare local <prBase>: the local base branch can be far
> behind origin (the MAK-362 review's was 124 commits), and a diff from it
> carries unrelated files. Filled in every phase, like {{prBase}} — it needs
> no claim.

### ReviewerRolePlaceholder

> ReviewerRolePlaceholder and ReviewedTipPlaceholder are the durable review
> record's identity (CLA-637). The plane assigns the round and the cycle,
> so neither is a placeholder: the driver reads them back off the record it
> matches. The driver resolves the role from the phase and the reviewed tip
> from the checkpoint it VERIFIED on the origin remote, so the session
> records what the seam gate will match rather than inventing it. The role
> is unique per review phase (`review-1`, ...) so a panel member each
> appends its own record; the tip is the origin commit the reviewer must
> check out and review.

### ReviewRecordHeading

> ReviewRecordHeading is the marker of the LEGACY block a review-only phase
> still records in the task's outcome for one release (CLA-637), so older
> readers keep seeing a review where they expect one. The durable
> per-reviewer record, written through record_review, is the interface the
> driver's review→fix seam gate reads now; this block is carried for
> compatibility, and the fix phase keeps it verbatim in its own outcome.
>
> It deliberately carries no round number. The round lives in the durable
> record, assigned by the plane; a hard-coded "round 1" would be false on any
> later run of the same task.

### reviewPhaseHarness

> reviewPhaseHarness is the harness a panel member with none of its own
> inherits: the REVIEW phase's, which is itself the run-wide harness when that
> phase names none — exactly the chain EffectivePhases' panelMember copies into
> each member. The FIRST built-in review phase is the one that expands, so it is
> the one whose harness a member inherits; nothing here assumes a review phase
> exists (the caller checks).

### EffectivePanel

> EffectivePanel is the review panel this config actually runs: the operator's
> own list, and nothing else — an absent key (nil) means the historical single
> review, not a panel of one (decision a0b1373b). There is no trial panel and no
> preset: the CLI cannot know which harnesses, providers or models an operator
> has, so it never names one. A FILE that writes `"review_panel": []` is refused
> by Validate rather than silently meaning either thing (reviewPanelSet).

### EffectivePanelConcurrency

> EffectivePanelConcurrency is how many members of a panel of n review AT ONCE
> (CLA-697): the operator's `review_panel_concurrency` cap, or the whole panel
> when the cap is absent, zero or larger than the panel. It never returns less
> than 1, and a panel of fewer than two members always reports 1 — there is
> nothing to fan out, and callers can use `1` as "the historical sequential
> path" without a second check.
>
> A negative cap is a config error refused by Validate; this method is the
> read path and stays total so a hand-built Config cannot make the driver
> spawn a negative number of sessions.

### panelMember

> panelMember is the review phase one panel member runs as: the configured
> review phase's knobs (its turn and wall-clock caps, its tier, its prompt)
> with the member's own identity, harness and model on top. Copying the phase
> rather than synthesizing a fresh one is what lets an operator cap the whole
> panel in one place — `{"name": "review", "max_turns": 80, "review_panel":
> [...]}` caps every member — while a member that names its own harness or
> model still wins where it does.

### Model and Tier are the same choice at two levels of indirection, and a

> Model and Tier are the same choice at two levels of indirection, and a
> member states one of them: either is the more specific statement than
> whatever the review phase carries, so setting one clears the other. An
> empty member field leaves the review phase's own value in place (itself
> empty on a config that resolved neither), which is the fall-through to
> the harness default.

### The method is the same kind of per-member statement as the model

> The method is the same kind of per-member statement as the model: the
> member's own wins, an empty one inherits the phase's (copied above), and a
> member that sets only the options keeps the inherited method so a panel can
> tune one seat's chunking without restating `maps-session`. Naming a METHOD
> takes the member's own options with it — nil means the benchmark defaults —
> so a seat that opts OUT of maps-session cannot keep inheriting the phase's
> knobs into a method that has no use for them.

### EffectivePhases

> EffectivePhases is the sequence a task runs as: the configured phases with
> their built-in prompts resolved and the review panel expanded, or — when none
> are configured — a single phase carrying Prompt.
>
> The unphased case is expressed as one phase on purpose, so the driver has
> exactly ONE shape to run and the old behaviour is not a second code path that
> can rot untested. "No phases" and "one phase" are the same thing downstream.
>
> Each phase's MaxTurns is resolved here too: the phase's own cap, else the
> top-level cap, else the built-in default. That resolution is what makes an
> unphased run bounded — before CLA-343 it fell through to the zero value, and
> one session ran 1093 turns / 285.9M tokens with no cap anywhere in the chain.

### The review phase's MODE is positional

> The review phase's MODE is positional: a review with a phase after
> it is the read-only gate, and a final review keeps the historical
> review-and-fix brief so a two-phase config still finishes. See
> reviewOnlyBrief and the builtinPhasePrompts comment.
>
> The fix phase's TERMINAL is positional for the same reason: a fix
> followed by a verification round must not open the PR (CLA-638,
> B12), because the verifier carries that terminal step only after it
> finds no open MUST.

### CLA-639

> CLA-639: a built-in review phase expands into one review phase PER
> PANEL MEMBER, so every consumer of the effective sequence — the
> driver's spawn loop, its seam, doctor — sees the reviewers that
> actually run rather than a single review the driver would have to
> remember to multiply. The panel is what the operator SET: an absent
> `review_panel` expands nothing (the single reviewer, as before), a
> phase carrying its own prompt is not expanded either (a custom review
> prompt owns its own contract), and a FINAL review keeps the historical
> combined brief — the position check is part of the expansion itself,
> not only of Validate, so EffectivePhases can never multiply a final
> review even when a caller skips validation. Validate refuses a
> configured panel in either of those shapes rather than letting it
> silently not apply.

### A verify phase that names neither a harness nor a tier nor a model wou

> A verify phase that names neither a harness nor a tier nor a model would
> otherwise run on the REVIEWER'S model — the one that raised the findings —
> which A5 (throughput, not only dollars) says is the wrong default:
> verification rounds are scoped and cheap, and a Claude subscription limit is
> shared with the operator's interactive use. So it inherits the FIX phase's
> harness and its model/tier pair instead (the shipped shape has fix on a
> cheap non-Claude harness), and an explicit harness/model/tier on the verify
> phase always wins. Configurable, never silent magic: the resolved values
> are what the spawn line stamps.
>
> Keyed on the RESOLVED built-in brief, not the name: an operator's own
> prompt under the name "verify" owns its own contract (the same convention
> every other variant follows — verifyBrief is what the driver treats as a
> verifier), so it must not silently inherit the fix phase's knobs.


## `internal/config/prbase.go`

### ResolvePRBase

> ResolvePRBase resolves the branch one repo's PRs target: the operator's
> per-repo pr_base override, else the project's integration branch when origin
> has it, else origin's default branch, else the integration branch.
>
> It never fails. A probe that cannot run degrades to the integration branch
> rather than blocking the work that needs a merge target, and every caller gets
> the same answer for the same repo: a brief that names one branch and a
> close-out that judges the PR against another is the disagreement this prevents.


## `internal/config/projects.go`

### DefaultIntegrationBranch

> DefaultIntegrationBranch is the branch whose tip doctor's deploy_lag check
> (CLA-322) judges the deployed build against when neither the project nor the
> top-level config names one. Both of this project's repos integrate through
> `staging`, so that is the convention the default encodes; an operator whose
> project integrates elsewhere sets integration_branch explicitly rather than
> getting an answer about a branch they do not have.

### PRBaseFor

> PRBaseFor resolves the operator's per-repo PR-base override for one project
> and one task repo: a matching projects[] entry's own NON-EMPTY map replaces
> the top-level one (the same replace-not-merge shape ReposFor uses), and the
> map is then consulted by the FULL identity ("owner/name") first, then by the
> bare name — the common spelling, and the same fallback ResolveCheckout
> applies to checkouts (CLA-608).
>
> Empty means "nothing overridden": the caller resolves the branch against
> the origin remote instead. It does NOT mean "no branch" — a validated map
> entry always names one.

### validateHealthURL

> validateHealthURL holds a health_url to "an absolute http(s) URL with a
> host". It is deliberately weaker than backlog_url's TLS floor: /health is a
> public endpoint read without credentials, so there is no bearer token to
> keep off the wire, and a plain-http internal plane is a legitimate thing to
> point at. What it refuses is a value that could never be fetched at all - a
> relative reference, or a scheme no HTTP client speaks - which would
> otherwise surface as an opaque "unsupported protocol scheme" error from
> doctor instead of as the misfiled config line it is.

### validateIntegrationBranch

> validateIntegrationBranch holds integration_branch to a bare ref-name shape.
> The value is passed to `git fetch` and `git ls-remote` by doctor's deploy_lag
> check (CLA-322); a value carrying refspec syntax (":") would make the fetch
> argument a src:dst spec and MUTATE LOCAL REFS during a preflight, a leading
> "-" reads as option syntax, and whitespace or anything outside a branch
> name's alphabet is rejected by git downstream anyway. Empty is legal: it
> means the default branch.

### validatePRBase

> validatePRBase holds every pr_base entry to a bare branch name, one level
> deeper than validateIntegrationBranch's own field: each VALUE reaches the
> phase briefs and (through them) `git`/`gh pr create` arguments, so it carries
> the same refspec-injection hazards a bad integration_branch does, plus the
> extra one of being substituted into prose. An empty value is refused rather
> than passed through: validateIntegrationBranch reads empty as "the default
> branch", but a map entry saying nothing is a half-written override whose
> silence would read as "resolved per repo" when the operator plainly meant to
> name a branch — omit the entry instead. Keys are held to non-blank, sorted so
> the reported entry does not change between runs on unchanged input.

### AllowLocalMCPServersFor

> AllowLocalMCPServersFor resolves the CLA-266 discovered-file allowlist for
> one project: a matching projects[] entry's own NON-EMPTY list replaces the
> top-level one, and everything else — an unmatched slug (how a single-project
> run reaches here with no projects at all), or an entry that set none — falls
> back to the top-level list. There is deliberately no way to say "none here"
> while the top level allows names: an empty allowlist is indistinguishable
> from not having configured one, and pretending otherwise would let a project
> line look tighter than it runs. Total, so no caller has a second error path.


## `internal/config/prompts.go`

### builtinPhasePrompts

> builtinPhasePrompts are the shipped briefs, selected by phase name.
>
> The split is implement, review, fix. The review phase's brief is chosen by
> POSITION rather than a second name (see EffectivePhases): a review with a
> phase after it is the read-only gate (reviewOnlyBrief), and a FINAL review
> keeps this historical review-and-fix brief so an existing two-phase config
> still reaches in_review. The fix phase applies the findings on a cheaper
> harness and owns the PR/in_review terminal step.
>
> An earlier version of this comment argued a fix phase was wrong because it
> "would hold neither the implementation nor the review context". That is still
> true, and it was never an argument about COST. The review record on the task
> (reviewOnlyRecordShape) is what replaces the shared context: the fix phase
> re-reads it from the plane, the way any resumed phase re-reads the bar.

### ReviewShaped

> ReviewShaped reports whether a RESOLVED phase prompt is one of the review-only
> briefs whose deliverable is the durable review record (CLA-637). It is the
> driver's single "requires a review record" predicate: the seam gate must apply
> to every review-shaped brief, not only the exact built-in review-only prompt
> it was originally keyed to, so every variant — and, as later phases land,
> every verifier and panel brief — is added HERE rather than in a second
> condition that can drift.
>
> The resolved prompt, not the configured one: EffectivePhases swaps in
> reviewOnlyBrief by position, and invocationFor later swaps the branchless and
> unverified variants. A handoff continuation replaces the prompt wholesale, so
> the caller reads this from EffectivePhases before any of that.


## `internal/config/prompts_briefs.go`

### noCodeReviewBrief

> noCodeReviewBrief is the review brief for a phase-1 exit evidenced by the
> PLANE'S RECORD (CLA-497) instead of a branch: the task left `ready`
> (in_review / done / parked / blocked - its own session settled it), or it
> carries a declared no-code delivery. There is no branch because the task's
> correct delivery is no code, so the branch-shaped builtin review brief would
> misdirect its successor - it asserts a branch "the driver verified to exist"
> that does not exist, and its "empty branch field is a FAILED hand-off to
> report" instruction contradicts the checkpoint the driver just recorded. The
> no-code review runs the adversarial gate over the plane's record of what
> phase 1 did, and its terminal step is the handover alone - no commit, no PR.

### unverifiedReviewBrief

> unverifiedReviewBrief is the review brief for a checkpoint the driver could
> NOT verify against the origin remote (CLA-576): the implement phase recorded
> branch {{branch}} on the plane, but the driver's branch check did not
> complete — most often the remote could not be read (network, a GitHub
> outage, a credential problem), though a check that never reached the remote
> (no local branch to compare, an ambiguous repo, no git on PATH) lands in the
> same delivery.Unknown — so whether the branch was ever pushed — and whether
> phase 1 did anything at all — is unknown. The
> branch-shaped builtin brief would tell the successor "a branch the driver
> verified to exist on the origin remote with its tip reachable, so phase 1
> really did implement, commit and push", which is exactly the claim this
> checkpoint cannot support. The successor is told the truth instead, and told
> to verify the hand-off itself before reviewing anything: the review phase is
> the backstop the 2026-09-24 decision accepted for the narrow loosening of
> CLA-457 (a session that recorded a branch it never pushed, during an outage,
> still reaches a review phase).

### reviewGateScope

> reviewGateScope defines what "run the adversarial review gate" means for a
> review-only phase, and what it does NOT mean (CLA-696).
>
> On the first review-panel run (MAK-266, 2026-10-19) the implement phase had
> already run typecheck, build, the full web suite (2935 tests), the
> layout-engine suite and veraPDF at its tip, and the brief was silent on
> whether the gate included running them again - so each of the four members
> decided for itself: one re-ran all of it on effectively the same tree, two
> ran focused subsets, and one read code. That is up to N redundant full-gate
> runs per round (wall time, machine load, a context full of test output), and
> it makes the members incomparable for the analytics (CLA-640) because some
> spent the session reading and some watching builds.
>
> The gates belong to the phases that own them: implement self-verifies at its
> tip, and the fix and verify phases run the gates again over what they change.
> A reviewer's value is the reading, so a command is scoped to a PROBE of a
> defect it already suspects - evidence for a finding, never a second gate.
>
> The last sentence is the panel's: members share one worktree, so a probe that
> writes a report, coverage file or snapshot hands the next member (and the fix
> phase) a dirty tree it must not commit.
>
> It names no evidence kind of its own. The MUST bar's seven kinds are what the
> record is read and analysed with, so a probe's output is cited under the kind
> that fits it - `gate` for a command that fails, `failure` for behaviour
> reproduced by hand - not under a new one this brief alone would know.

### reviewOnlyBriefOpening

> reviewOnlyBrief is the review phase's brief when a LATER phase will apply its
> findings (the shipped [implement, review, fix] sequence). It is the split's
> whole point: the expensive model reads and judges, and changes nothing.
>
> A final `review` phase keeps the historical review-AND-fix brief
> (builtinPhasePrompts[ReviewPhaseName]) so a two-phase config written before
> this split keeps reaching in_review instead of silently stopping at a
> checkpoint with no successor. The position, not a second name, selects the
> mode; EffectivePhases resolves it, and the driver's variant selection matches
> on the resolved prompt text.
>
> "Review only" is enforced as a scope, not a capability: the phase is told what
> it may not do, and the fix phase is told to treat any fix it did not make as
> its own job by ID. A session that edits anyway is a failure the fix phase can
> still absorb - it re-reads the branch - which is why the prohibition is worded
> as the default rather than as an impossible act.
>
> The brief is composed of a CORE plus a record shape and an ending, so the
> verified panel member's variant (CLA-639 M5) can reuse the core verbatim and
> swap only the two halves that differ: the panel member writes the durable
> record and nothing else.
> The core is assembled from the parts that do and do not depend on the
> hand-off's verification state: reviewOnlyBriefOpening names the recorded
> branch, and the claim that follows is the one sentence an unverifiable
> checkpoint cannot support. The verified briefs append reviewHandoffVerified;
> the panel member's unverified variant appends reviewHandoffUnverified and its
> self-verification step. The remainder is identical in every variant.

### panelMemberReviewOnlyBrief

> panelMemberReviewOnlyBrief is a PANEL MEMBER's brief (CLA-639 M5): the same
> core, with the durable-record-only shape and an ending that forbids the
> outcome write. It is selected by the driver at spawn time — the member's
> captured prompt is the review-only brief, exactly as for the single reviewer
> — and only for a checkpoint that carries a branch: a branchless checkpoint
> cannot run a panel at all (no live holder accepts a durable record), so its
> single reviewer keeps the shipped no-code brief.

### panelMemberUnverifiedReviewOnlyBrief

> panelMemberUnverifiedReviewOnlyBrief is panelMemberReviewOnlyBrief for a
> checkpoint the driver could not verify against the origin remote (CLA-576,
> CLA-702). It cannot reuse the verified core: that core asserts the driver
> verified the branch, which is exactly what an unreadable checkpoint cannot
> support, while its record shape says the driver could NOT read the tip. It
> keeps the panel's record-only contract and ending unchanged.

### noCodeReviewOnlyTerminalStep

> noCodeReviewOnlyTerminalStep ends a branchless review-only phase. There is no
> branch to keep a lease on and the task may already be settled by phase 1, so
> the terminal step records the review and leaves the task's own status alone;
> the fix phase decides whether a finding requires re-opening it. The record
> here is the LEGACY block alone (operator decision on question 03a12578,
> option 1): the durable table takes writes only from the task's live holder —
> a settled task has none, and a still-held no-code task's write would seal a
> round this variant never closes — so attempting the append would be a
> refused call at best and a wedged run at worst, and the seam does not need it.

### fixCore

> fixBrief is the phase that APPLIES a review's findings. It is deliberately not
> named "review": reviewTerminalStep is injected by phase name, and a fix phase
> that reused the name would be told it was review-only (or review-and-fix)
> rather than the one actor that changes code.
>
> Why a separate phase at all, when the review used to fix its own findings: the
> review model is the expensive one, and the fixing - apply, re-verify, commit,
> push, open the PR - is the bulk of a review session's tool calls and context.
> The reviewer keeps the judgement; the cheap harness gets the typing. The cost
> is one more session startup and a hand-off the fixer must read; the review
> record on the task is what makes that hand-off cheap and auditable.
> fixCore is the part both fix variants share: read the released record, check
> the reviewed tip, apply every finding by its plane id, re-verify scoped. The
> variants differ ONLY in their terminal step, because whether the fix phase is
> the last one depends on whether a verification round follows it (CLA-638).

### fixBrief

> fixBrief is the phase that APPLIES a review's findings when it is the LAST
> phase of the sequence. It is deliberately not named "review":
> reviewTerminalStep is injected by phase name, and a fix phase that reused the
> name would be told it was review-only (or review-and-fix) rather than the one
> actor that changes code.
>
> Why a separate phase at all, when the review used to fix its own findings: the
> review model is the expensive one, and the fixing - apply, re-verify, commit,
> push, open the PR - is the bulk of a review session's tool calls and context.
> The reviewer keeps the judgement; the cheap harness gets the typing. The cost
> is one more session startup and a hand-off the fixer must read; the review
> record on the task is what makes that hand-off cheap and auditable.

### fixVerifyBrief

> fixVerifyBrief is the fix phase when a VERIFICATION round follows it (CLA-638,
> B12). The verification round re-reviews the delta and adjudicates the prior
> MUSTs, and IT carries the PR/in_review terminal step; a fix session that
> opened the PR here would ship fixes no verifier has looked at. The rest of the
> contract is identical, because it reads the same record and applies the same
> findings.

### noCodeFixBrief

> noCodeFixBrief is fixBrief for a branchless checkpoint: the review judged the
> plane's record, and the fix is an amendment to that record, never invented
> code. The branchless review writes no durable record (question 03a12578,
> option 1), so this brief reads the legacy `## Review` block ALONE and applies
> its findings by amending the task's record - there is no `reviews.records`
> entry and no plane-assigned id to update. Its terminal step is the no-code
> handover alone.

### verifyStoppingRule

> verifyStoppingRule is the stopping rule's core: what counts as a MUST that is
> still open (CLA-638, D7/A6/B7/B12/B13). It is shared by the code and no-code
> variants so the bar cannot drift between them; the ID-based machinery that
> enforces it — the round cap and the non-convergence breaker — is
> verifyDriverBounds, because the branchless variant has no plane finding ids
> to count or park on.

### noCodeVerifyTerminalStep

> noCodeVerifyTerminalStep ends a BRANCHLESS verification round: the no-code
> counterpart of verifyTerminalStep. This shape writes no durable record
> (question 03a12578, option 1 — the branchless review writes none either), so
> there is no round to release and no closeRound to make: the verifier appends
> its OWN `## Review` block, which IS the record the driver's checkpoint reads,
> and the disposition is an operator park (open MUST — the id-based loop the
> driver runs cannot exist without plane finding ids) or the status move
> (clean — the verifier carries the hand-over, and the fix phase has already
> written the **Tests** section the in_review guard reads).

### verifyBrief

> verifyBrief is the fix-verification round (CLA-638): review the fix delta plus
> its regression surface, adjudicate every prior open MUST by its plane id, and
> gate on no open MUST. It is review-shaped — it writes a sealed durable review
> record and the driver closes the round at the seam — so it uses the same
> record machinery as the review gate, with `scope: fix-delta` and the prior
> round's reviewed tip as the delta base.
>
> The driver prepends the prior open MUST ids (and their findings) to this
> brief: the list is plane state, not config, and the driver reads it from the
> released records before spawning. The wording below assumes that block is
> present and tells the session what to do if it is not.


## `internal/config/prompts_handoff.go`

### HandoffMarker

> HandoffMarker is the exact line a session puts in its FINAL message to hand
> the rest of its job to a fresh successor session (CLA-352). Everything after
> the marker line is the successor's prompt; the driver respawns on it with the
> same run-continuity rules as a phase resume.
>
> The syntax had to be improbable in ordinary prose and easy to pin in a test,
> and it must not resemble tool-call framing markup: the web repo's
> docs/text-integrity.md records agents destroying their own output merely by
> EMITTING such tags, so an angle-bracket or tag-shaped marker would put the
> defect this feature rides on into the feature itself. A plain punctuated
> uppercase line does neither.

### HandoffPromptMax

> HandoffPromptMax bounds the successor prompt a handoff block may carry, in
> bytes. A session-authored respawn prompt is self-directed prompt injection,
> and the size cap is one of the non-negotiable guards on it: an over-cap
> prompt is refused (logged, normal path) rather than truncated, because a
> truncated brief is a successor confidently working half an instruction. "A
> few KB" is the spec; a genuine continuation prompt — decisions made, state
> verified, exact next steps — fits in far less.

### HandoffPreamble

> HandoffPreamble is driver-authored framing prepended to a handoff respawn's
> prompt (CLA-352) — the successor never gets the emitting session's own
> prompt bare. A phase resume (reviewPhaseName, above) works only because its
> brief spells out "do not claim, call heartbeat first": a session-authored
> prompt cannot be trusted to carry that instruction forward on its own, and
> without it a successor calls next_task/claim_task on a fresh task, or never
> re-enters its run — the single heartbeat is the resume/settlement check,
> while the run's lease itself is renewed by the driver from Go while the
> session runs. Placed FIRST, not
> appended, so the driver's own constraints bound the prompt that follows
> rather than being one more thing a self-authored prompt could bury or
> contradict — the same reasoning behind refusing to truncate an over-cap
> prompt rather than editing it.

### HandoffContinuation

> HandoffContinuation is appended, by the driver, to a handoff respawn's prompt
> (CLA-353) so a phase's own contract survives a session-authored hand-off.
> HandoffPreamble carries the "resume, don't claim" contract forward on every
> handoff; the ORIGINAL phase brief's own instructions do not — a handoff
> respawn's prompt is HandoffPreamble plus the predecessor's self-authored
> nextPrompt alone, so anything the built-in brief said is otherwise gone the
> moment a session hands off instead of finishing itself.
>
> Two things ride, and the first rides for BOTH built-in phases. The rerun
> bound (CLA-391) rides because a handoff must not be how unbounded rerunning
> arrives: the review successor inheriting a half-fixed finding list is the
> loopiest session there is, but the implement successor mid-verification is
> the same shape — fresh context, no memory of the runs already paid for, and
> every reason to start paying again — so bounding only the review respawn
> would leave the asymmetry as the hole. The review phase ADDITIONALLY carries
> reviewTerminalStep — the PR-then-hand-off sequence CLA-353 exists to make
> land — so a handoff mid-review must not be a way to lose it. The implement
> phase carries the bound alone: it has no terminal step, because it stops
> rather than hands its task to in_review.

### The fix phase owns the branch-shaped terminal step, exactly as a final

> The fix phase owns the branch-shaped terminal step, exactly as a final
> review did before the split. A handoff mid-fix must not be a way to
> lose the commit/push/PR/in_review sequence, NOR the record contract:
> fixTerminalRecord is what makes the successor keep the review block
> verbatim and add the ### Fixes list. Dropping it here loses the only
> durable record of the round, which is the exact loss the split exists
> to prevent.

### HandoffContinuationFor

> HandoffContinuationFor is HandoffContinuation for a review phase whose mode is
> POSITION-dependent (see EffectivePhases): a review-only phase has no terminal
> step to carry, only the record-then-hold ending, and appending the shared
> reviewTerminalStep would tell its successor to commit, push, open a PR and
> hand the task to in_review — the exact actions the review-only mode forbids.
>
> It carries reviewOnlyRecordShape as well as the ending: the ending says
> "in the shape above", and a respawned successor never saw the shape. The
> driver's review→fix gate matches the DURABLE record, so a successor that
> invented its own shape (or skipped the append) would fail the seam.
>
> The flag is only meaningful for ReviewPhaseName; every other phase ignores it.

### FixVerifyHandoffContinuation

> FixVerifyHandoffContinuation is the fix phase's continuation when a
> verification round follows (CLA-638): the successor must still answer every
> finding, commit and push, and then STOP holding the task. Carrying the
> legacy reviewTerminalStep here would tell it to open the PR and move the task
> to in_review — the terminal actions the verifier owns, and precisely what
> B12 says the fix phase must not do — and carrying fixTerminalRecord would do
> the same thing in prose (CLA-686 D1): the record requirement here is
> fixVerifyRecord, which keeps the finding updates and drops the hand-over.

### NoCodeFixVerifyHandoffContinuation

> NoCodeFixVerifyHandoffContinuation is FixVerifyHandoffContinuation for a
> BRANCHLESS fix: the amendment is to the plane's record, so there is nothing
> to commit or push, but the terminal step still belongs to the verifier.
> NoCodeFixHandoffContinuation would carry the legacy no-code hand-over and tell
> the successor to move the task to in_review before the verification round has
> seen the amendment — the same B12 leak the branch-shaped variant exists to
> close.

### driverRenewsLease

> driverRenewsLease tells a session-claimed phase that its lease is renewed by
> the driver, not by the session (CLA-917). Every shipped session-claimed phase
> runs under internal/loop's leaseRenewer, which beats from Go every third of
> the 30-minute lease for as long as the session runs, so the served
> protocol's "heartbeat every ~10 min" would make the session a redundant
> second holder. The runner-claimed briefs carry their own prohibition. The
> supervisor's conflict-only step has NO driver-side renewer (it runs outside
> the loop package), so ConflictBrief keeps its own heartbeat cadence.

### rerunGuidance

> rerunGuidance rides on every built-in phase brief (CLA-391). Repeated
> verification was the dominant measured waste of the 2026-08-19/20 drain: one
> review phase ran the same Playwright spec fourteen times differing only in
> the grep filter; another ran twelve identical `go test -race ./...` on a
> two-file diff; a third managed seven full-suite runs, eight typechecks and
> ten lints inside one phase. Cost is turns times context, and every rerun is
> another turn over everything the session holds, so the brief bounds
> CONSECUTIVE reruns of the same command and demands a stated reason past the
> bound.
>
> The phase-2 adversarial review found three ways the first wording still let
> the fourteen-run loop through, and the wording closes each:
>
>   - A reason phrased as "name what changed since the last run" was satisfied
>     by the selector change ITSELF - "narrowed the grep" is a change, and it
>     does make the output differ, so fourteen compliant runs each carried a
>     truthful-looking reason line. The reason must name a change to what is
>     UNDER TEST, or say why the last result may not reproduce; a narrower
>     selector is neither.
>   - "Consecutive" was undefined, so any interleaved call (read, grep, diff -
>     the shape real sessions actually produce) reset the count every turn and
>     the ceiling never engaged. Only a change to what the command tests
>     resets it now; other tool calls do not.
>   - The arithmetic past a reason was unstated: a reason buying a fresh pair
>     makes fourteen runs four cheap lines, while an absolute ceiling collides
>     with the review brief's own fix-and-reverify demand. A reason buys ONE
>     run, and the count restarts only when the thing under test changed -
>     which is exactly what happens each time a fix lands, so honest
>     re-verification never reaches the bound at all.
>
> Two readings the task called out stay pinned in the wording: the bound is
> per command, not on verification overall (typecheck, then tests, then lint
> is three commands run once each); and a narrowed rerun is still a rerun
> (same suite, different selector or filter - the fourteen-run case differed
> ONLY by its filter). The sibling failure, consecutive turns idling on a
> background job and re-polling its output, is the waiting problem the served
> skill already carries a reference for - so the brief POINTS at it rather
> than restating rules that live there and would drift here. That pointer is
> SCOPED as well as pointed: waiting.md's Rule 1 tells a claim-holder to let
> go, and read unscoped it directs a mid-phase session to release - the exact
> failure the briefs' own endings forbid - so the session takes the polling
> discipline and keeps hold of the task.

### implementResumedBranchRule

> implementResumedBranchRule is the implement brief's rule for resuming an EXISTING branch or
> worktree (CLA-378). EZY-199 was a recovered stale claim whose implement phase followed the
> rest of this brief faithfully - it verified the found tip with a build, the unit suite and
> 60 e2e executions, then pushed - and never synced against staging, which had superseded the
> branch's whole fix a day earlier. The review phase then paid $11.70 for the archaeology,
> merge and salvage that an up-front check costing cents would have made unnecessary.
>
> Three parts, each load-bearing. SYNC first, as a merge and never a rebase: the branch may
> already be pushed, and the daemon's phase-boundary check compares the pushed origin tip to
> the local tip, which a rebase invalidates. RE-VALIDATE against the MERGED tip before doing
> or verifying anything: a clean merge proves nothing - it can silently revert a newer fix -
> so the check is the task's own bar, not conflict markers. On SUPERSESSION, record the
> decision and PARK the task with an outcome citing it - a bare stop leaves the recorded
> branch reading as checkpointed WIP, which spends a review phase rediscovering the stale
> tip - or salvage only what still adds value; never re-verify and push stale work.
>
> Harness-neutral on purpose: one wording serves every harness's implement phase.
>
> CLA-608: the branch to merge in is the repo's PR BASE ({{prBase}}), resolved
> per repo by the driver, not necessarily the project's integration_branch. The
> resumed branch forked from its PR base and will merge back into it, so the
> superseding commits to sync against land there; for a repo whose PRs target
> main (the makesPDF plugins) the integration_branch would name a branch that
> does not exist, and for lecstor/makespdf it would sync against the deploy
> branch while the PR merges elsewhere.

### implementCloseOutRule

> implementCloseOutRule is the implement brief's disposition for the offer the
> plane boosts ahead of fresh work (CLA-540): an approved-awaiting-merge task —
> implementation and review already done, only the close-out remaining — which
> next_task serves FIRST and which the phase-1 brief must not refuse by default.
> A phased implement session is told to claim, work, commit, push and stop at
> the checkpoint; a close-out has no commit, no branch and no worktree, and its
> required act is a status move the brief's letter forbids — so without an
> exception the session asks the operator about an offer that is exactly its
> job (observed twice on 2026-08-28: CLA-533 and CLA-526, both green and
> mergeable, both raised as blocking questions).
>
> Everything the plane already says on the offer stays there: the merge target,
> the red-CI repair rule and the decline door all ride on the offer's mergeHint,
> and the CLA-341 one-representation rule keeps them in that one place. The
> clause also names ONE offer, never the queue — several close-outs pending
> means this session serves the one it was offered and ends, not all of them on
> one accumulating context, which is the failure the unphased prompt's
> drain-vocabulary ban exists to stop.
>
> The ending deliberately does NOT reuse the checkpoint's "Then STOP and end the
> session" sentence: TestBuiltinImplementBriefPinsTheResumedBranchRule locates
> the terminal stop by that exact phrase, and this clause sits before the
> resumed-work rule, so reusing it would move the pin's anchor.
>
> The fork also preempts the resumed-work rule below: a close-out claim carries
> a recorded branch, so that rule's "existing branch" trigger would otherwise
> match an offer it must never apply to - merging the integration branch into an
> approved PR branch would rewrite the diff the approval and the green CI stand
> on. "For found work, not this offer" says the boundary in the brief itself.

### implementCommitEarlyRule

> implementCommitEarlyRule is the implement brief's checkpoint floor (CLA-544):
> get a first commit down EARLY, before the design is fully settled - and push
> it and RECORD the branch on the task as soon as that first commit is down -
> then commit and push as you go rather than once at the end. CLA-529 and
> CLA-531 were each handed back empty three times on 2026-08-28: the four
> heaviest sessions contained not one Edit, Write or Create tool call - each
> read and planned for its whole life, a wall-clock kill ended it with a clean
> worktree, and nothing was salvageable.
>
> The mechanism is the driver's checkpoint line (loop.go): a wall-clock-killed
> session earns a checkpoint only with HasWIP, and HasWIP means a branch
> recorded on the TASK - the session's own record_progress(branch), or the salvage
> recording one. The salvage refuses a clean worktree on purpose (salvage.go:
> "The one thing that must NOT happen here is recording a branch"), so a commit
> nobody pushed and recorded is as invisible to the plane as no commit at all.
> The clause therefore names all three acts - commit, push, record - because
> the record is the checkpoint, and it must land as soon as the first commit is
> pushed, not only at the terminal step.
>
> Two constraints shaped the wording. The trigger is an EVENT, never a
> duration: a session cannot read its own wall clock (the constraint that
> shaped handoffGuidance), so "commit within fifteen minutes" is not
> implementable; a skeleton, a failing test, a stub with the signatures are
> events it can recognise. And the clause is a FLOOR, not a ceiling: checkpoint
> pushes are WIP, not deliveries - the terminal step's bar (self-verify, then
> commit, push, record the branch) is unchanged, and a checkpoint push must not
> be passed off as the self-verified terminal delivery.


## `internal/config/prompts_maps.go`

### promptsmaps.go

> prompts_maps.go — the maps-session review method's turn prompts (CLA-864,
> CLA-869). This is not a brief of its own: the phase's review brief stays the
> first turn's substance (runner context, read-only rules, the bar, the done
> report), and the section below is APPENDED to it. The later turns are
> deliberately lean — they rely on the first turn's transcript, which the same
> session still holds.

### MapsSessionFirstTurn

> MapsSessionFirstTurn is the section appended to the phase's review brief for
> the first notes turn: what the maps are, how to read an M and an F item, the
> checklist, and chunk 1 of the items. hunksEmbedded says each M item carries
> its hunk, so the instructions send the model to the tools only for context.
>
> base is the commit the per-file diff starts from (the runner's merge-base),
> filled in literally: this text is appended after the driver's placeholder
> substitution, so a placeholder here would reach the session unsubstituted.


## `internal/config/prompts_records.go`

### worktreeReclaimStep

> worktreeReclaimStep is the reclaim every FINAL phase step carries: the session
> that takes the work off the machine removes the tree it worked in, because the
> close-out that merges may not touch a tree it did not create (CLA-534). It is
> one constant so the shipped review and verify steps and the runner's final fix
> and verify steps cannot drift apart.
>
> Order is the property: it follows the PR's confirmation, because until then
> the tree may hold the only copy of the work, and it precedes the phase's final
> call, which stays the emphatic ending (CLA-384).
>
> The upstream reset is load-bearing, not tidiness. A fresh task worktree is cut
> with `git worktree add -b <branch> <dir> origin/<prBase>`, so its branch
> tracks the PR BASE, and `git branch -d` deletes only a branch fully merged in
> its UPSTREAM: without the reset the delete refuses a pushed, unmerged branch.
> No `--force` or `-D` anywhere - a refusal is the guard.

### reviewTerminalStep

> reviewTerminalStep is the review phase's LAST instruction, kept as a constant so
> its position can be pinned by a test and not only its wording.
>
> Position is the property that failed (CLA-384). The old brief said "Then push,
> and hand the task to in_review" - the instruction was PRESENT, as a trailing
> clause on a long sentence about scoping the follow-up re-verification. Three of
> four review phases in one evening did the work and ended without it. The
> implement brief, whose equivalent step is emphatic, names its call and its
> arguments, and comes last, did not fail once over the same evening. So this is
> written in that shape and pinned to that place.
>
> CLA-353: the terminal sequence is push, then open a PR, then the hand-off - in
> that order, and now all three live here rather than "push" sitting one sentence
> earlier. An overnight forensic drain found `gh pr create` appearing ZERO times
> across four review-phase transcripts, and two tasks committed straight onto
> `staging` because a session's cwd was already sitting there and nothing told it
> that was wrong - so a task closed at in_review with no PR, and the close-out
> flow's "merge its PR into staging" had nothing to merge. CLA-384 made
> the hand-off the phase's emphatic final call, which if the PR step were bolted on
> earlier would make it LESS likely to survive, not more - a step mentioned before
> an emphatic final call loses to it. So the PR step is folded into this same
> terminal block, before the hand-off sentence, not left in the paragraph above.
>
> CLA-608: the target was the literal "(staging)" here and in the never-commit
> rule below. Neither read the config nor the task's repo, so every task in a
> repo with no staging branch (the makesPDF plugins; lecstor/makespdf, whose
> PRs target integration) was told to open its PR against a branch that does
> not exist. The target is now {{prBase}}, resolved per repo by the driver.
>
> CLA-536: this phase's session is the last one to work in the tree, so the
> reclaim rides the shared worktreeReclaimStep - after the PR, before the
> hand-off that must stay the emphatic final call.

### reviewOnlyTerminalStep

> reviewOnlyTerminalStep ends the review-only phase. It is the counterpart of
> reviewTerminalStep for a phase that must NOT fix, commit, push, open a PR or
> hand the task to in_review: the review is a checkpoint, and the next (fix)
> phase resumes the same run from the branch the implement phase recorded.
>
> The review's FINDINGS are the deliverable, and they are recorded on the task
> with record_progress(outcome: ...) — the plane's one owner of the statusless
> outcome write. That keeps the record durable across the seam, readable by the
> fix phase's get_task, and attributed to the run that wrote it. A file in the
> worktree was rejected: it is invisible to the plane, dies with the worktree,
> and cannot be read from another machine.

### panelMemberTerminalStep

> panelMemberTerminalStep ends a PANEL MEMBER's phase (CLA-639 M5). It is
> reviewOnlyTerminalStep with the legacy outcome write REMOVED, not moved: the
> member's only task write is the durable record, and the member after it must
> not be able to read this member's findings through the task's outcome. A
> session told to write both would have its outcome write refused by the plane
> (the round is sealed the moment the record lands, CLA-636 B3) and could
> easily read that refusal as the phase failing.

### reviewRecordOutcomeShape

> reviewRecordOutcomeShape is the LEGACY half of the review record: the
> `## Review` block appended to the task's outcome. It is kept for one release
> (CLA-637), so older planes and readers still find a review where they expect
> one, while the durable per-reviewer record below is what the driver's seam
> gate reads.
>
> It is a SHAPE, not a grade: the fix phase reads it back by finding ID, and
> the driver still parses it (reviewFindingsDigest in internal/loop) when a
> durable record is unavailable, so the severity bracket and the
> `(<kind>): <evidence>` prefix after it are load-bearing.
>
> It APPENDS below the outcome as it stands. The plane's statusless outcome is
> a full replacement, and the outcome may already carry phase 1's delivery
> record (or, on a request_changes re-trip or a takeover, an earlier trip's
> round); writing a fresh block over it would lose that record. The fix phase
> keeps everything above the block for the same reason.

### reviewRecordTipNone

> reviewRecordTipNone is the branchless counterpart: there is no branch and no
> commit to review, and — per the operator's decision on question 03a12578,
> option 1 — no durable record either. The plane accepts review writes only
> from the task's LIVE holder: a settled task (phase 1's own session settled
> it) refuses the append outright, and on a still-held no-code task it would
> seal a round this variant's driver never closes, blocking the run's own
> outcome writes. The legacy block IS this variant's record, and the driver's
> seam gates on the block.

### reviewRecordDurableShape

> reviewRecordDurableShape is the durable half (CLA-637): one sealed
> per-reviewer record per run, written through record_review. It is the
> driver's seam evidence — round, role, run and reviewed tip are what the gate
> matches — and per-finding `raisedByModel` is what the analytics need.
>
> ORDER IS LOAD-BEARING. While a run has a sealed record in an open round the
> plane refuses that run's outcome, body and decision writes (CLA-636 B3), so
> the legacy outcome block must be written FIRST. The driver closes the round
> before spawning the fix phase, which releases the findings to it.

### reviewOnlyRecordShape

> reviewOnlyRecordShape is the verified review-only phase's full record
> contract: the legacy outcome block, then the durable record. The driver
> injected the phases' RESOLVED harness/model and the verified round, role and
> tip, because a session cannot reliably name its own model, cannot see its
> predecessor's, and cannot see whether the branch tip it read is the one the
> driver verified.

### noCodeReviewOnlyRecordShape

> noCodeReviewOnlyRecordShape is the branchless review-only phase's record
> contract: the LEGACY outcome block alone, with no durable-record half —
> the plane accepts review writes only from the task's LIVE holder and a
> settled task has none (operator decision on question 03a12578, option 1), so
> the seam gates this variant on the block (changed since the spawn, heading on
> its own line, and a last block the baseline did not carry).

### panelMemberRecordIntro

> panelMemberRecordIntro and panelMemberRecordBody are the two halves of a
> PANEL MEMBER's record contract (CLA-639 M5): the tip paragraph sits between
> them, so the verified and unverified variants share everything else.
>
> A panel member writes the DURABLE record and NOTHING ELSE on the task. The
> legacy outcome block reviewOnlyRecordShape prescribes is not merely deferred
> here, it is forbidden: member 1's block would be readable by member 2 through
> its own get_task — the exact cross-member leak the seal exists to prevent —
> and the plane refuses a run's outcome writes while its record sits sealed in
> the open round the panel writes into (CLA-636 B3), so the write would be
> refused anyway. The panel's findings reach the task's outcome through the FIX
> phase, from the merged list the driver releases to it (mergedFindingsDigest).
>
> The field list below is deliberately a second copy of the durable record's
> rather than a shared constant: the two differ wherever the legacy block is
> named ("one object per finding in the block", "write `- none` under
> Findings"), and one string cannot say both. Both copies are pinned by
> TestReviewOnlyBriefsPrescribeTheDurableRecord, so they cannot drift silently.
>
> No branchless variant exists: a branchless task writes no durable record (a
> settled task has no live holder to accept one), so a panel cannot run that
> way at all — the driver skips every member after the first, whose brief is
> the shipped no-code one.

### reviewMUSTBarGrades

> reviewMUSTBarGrades is the accepted bar for a MUST (task CLA-635; the
> review-panel design's section 14 A2 as amended by B5/B6, decisions 3a57db6a
> and 60ed0748 superseding it): the evidence a MUST must cite, the seventh kind
> the operator added after the second review, and the things that are never a
> MUST however confident the reviewer is.
>
> Only PRESENCE is checked structurally - here as the reviewer's own
> obligation, and by the driver when it parses the block back
> (loop.reviewFindingsDigest). The VALIDITY of the evidence is judged by a
> verifier in the next round, never by the fixer: a fixer that disagrees with a
> MUST refutes it with evidence, it does not downgrade it. Where a repo hard
> rule concerns style, prose or documentation wording, the not-a-MUST list wins
> over it (B6); missing tests stay not-a-MUST except the untested-guard case.
>
> The GRADING rules are one constant because they are the same for every
> reviewer; the attribution sentence that closes the bar is not, so it is
> separate (CLA-639) — see reviewMUSTBar and panelMemberMUSTBar.

### fixOutcomeRecord

> fixOutcomeRecord is the fix phase's outcome requirement, shared by both fix
> variants. It carries the review forward: the final outcome is the only
> durable task-record the round has (the ledgers excepted), so dropping the
> review block here would lose the very record the split exists to keep. And it
> must keep everything ABOVE the block: that is phase 1's delivery record (or
> an earlier round's), and the statusless outcome replaces the field wholesale,
> so a final outcome built only from the review block and ### Fixes would erase
> it.

### fixFindingUpdates

> fixFindingUpdates is the durable half of the fix phase's record requirement:
> the per-finding status updates, which must go out while this run is still the
> task's HOLDER because the plane refuses them once it is not. It is stated
> without reference to WHO hands the task over — the last fix of a sequence
> does, and a fix with a VERIFICATION round after it does not — so both
> terminals can embed it without one contradicting the other (CLA-686 D1). The
> ordering rule that DOES name the hand-over lives in fixTerminalRecord, where
> the phase making that call is the one reading it.

### fixTerminalRecord

> fixTerminalRecord is the BRANCH fix phase's record requirement when this fix
> OWNS the sequence's terminal step, stated BEFORE the shared reviewTerminalStep
> so the emphatic final block still ends the brief (the CLA-384 property): the
> durable finding updates go out before the call that ends the run and clears
> the holder, then the hand-over carries the outcome requirement
> (fixOutcomeRecord) forward.
>
> The ordering sentence is here rather than in fixFindingUpdates because it is
> advice about THIS phase's own hand-over call: the plane refuses the batch
> after that call, even when the same call carries the outcome.

### fixVerifyRecord

> fixVerifyRecord is the BRANCH fix phase's record requirement when a
> VERIFICATION round FOLLOWS it (CLA-686 D1). fixVerifyBrief and its handoff
> continuation end with fixOnlyTerminalStep — "do NOT move the task to
> in_review", because the verifier owns that move — so they must not carry
> fixTerminalRecord's "THEN hand the task over" as well: that is one brief
> telling the fixer both to hand the task over and not to, and a fixer that
> obeys the first half ships fixes no verification round has seen. Everything
> the RECORD owes is unchanged; only the hand-over sentence is dropped, because
> the hand-over is not this phase's.

### fixGradingRule

> fixGradingRule is the fix briefs' shared obligation for acting on the graded
> review record (CLA-635): every MUST fixed or refuted with evidence, never
> downgraded by the fixer, and SHOULD/NIT handled by the filing rule. B5's
> structural check runs driver-side, so a MUST that reaches this brief has
> already been judged to carry evidence; the fixer may not second-guess the
> grade - it refutes the finding with evidence of its own, or fixes it. A1 is
> restated because the interim record may hold a single reviewer: disagreement
> is not a second opinion, and a lone reviewer's MUST still gates.

### noCodeReviewTerminalStep

> noCodeReviewTerminalStep is the no-code review brief's LAST instruction
> (CLA-497), the counterpart of reviewTerminalStep for a phase-1 exit that was
> evidenced by the plane's record rather than a branch. There is nothing to
> commit and no PR to open - the task's correct delivery is no code - so the
> step is the handover alone, with the same emphatic, final-call shape
> CLA-384 pinned on the branch-shaped one. It also covers the form-(b) case
> where phase 1 ALREADY settled the task (in_review / done / parked /
> blocked): the handover happened before this phase ran, and the session's job
> is to verify the record reads honestly and stop - re-opening or releasing a
> settled task would restart the release loop this task exists to kill.


## `internal/config/prompts_review_run.go`

### promptsreviewrun.go

> prompts_review_run.go — the brief for `clankerbar review run` (CLA-870).
>
> The command reviews a local repository's base..head outside any task, claim
> or plane: an eval runner scores the REAL driver, and a human uses the same
> entry point later. The brief is therefore the review-only brief's substance
> with every task-shaped fact removed — no task id, no done-when bar, no
> record, no terminal hand-over — while the reading discipline, the gate scope,
> the MUST bar and the done report stay the phase's own constants. That is what
> keeps the command's review the same review the phase runs.

### reviewRunMUSTBarGrades

> reviewRunMUSTBarGrades is reviewMUSTBarGrades for a review with no done-when
> bar. Two clauses name the task's doneWhen as an input this review does not
> have, so they are removed rather than left to reach a session as an
> instruction it cannot follow; every other grading rule — the evidence kinds,
> the seventh kind, and the not-a-MUST list — is the phase's verbatim. A test
> pins that both replacements landed, because a silently failed Replace would
> hand the session a bar that names a bar it was told does not exist.

### reviewRunGateScope

> reviewRunGateScope is reviewGateScope for a review with no phase sequence.
> Three sentences name phases this command does not have - the implement
> phase's self-verification, the fix and verify phases, and the shared panel
> worktree - and left in they would tell a standalone session to rely on a
> checkpoint that does not exist. The discipline itself is the phase's verbatim
> (no full gate runs, targeted probes only, the probe's output as the finding's
> evidence), which is what keeps the command's review comparable. A test pins
> every replacement, for the same reason reviewRunMUSTBarGrades' does.

### ReviewRunBrief

> ReviewRunBrief renders the brief for a standalone review of base..head.
>
> It carries no placeholders: the command does not run the phase driver's
> substitution pass, so a placeholder here would reach the session standing.


## `internal/config/prompts_runner.go`

### The runner-claimed briefs

> The runner-claimed briefs. A runner-claimed session is started for ONE
> prepared task, in a worktree the runner created, on a branch the runner
> pushed and tracks, with the task already claimed and the lease held by the
> runner. Everything mechanical therefore leaves the brief: the session gets
> the context block (TaskContextBlock) above it and does only the phase's own
> work. It also writes no record: it ends with a done report
> (runnerDoneReport) and the runner turns that into every record. These
> constants are added beside the shipped briefs, not in place of them; the
> driver selects them in a later wiring step.

### runnerToolWaitBound

> runnerToolWaitBound is the longest a runner-claimed session may block inside
> ONE tool call. The adapters kill a call left open past the harness's
> max_tool_call, so the brief has to name a figure below it: "a bounded wait"
> with no number is how sessions chose a wait the driver killed (CLA-900).
>
> Ten minutes, because a fixed figure in a package-level brief cannot see the
> project's config. It has to clear three shipped bounds: both 30m tool caps, and
> opencode's 15m idle bound, which is still what ends a call the adapter never
> saw opened (claude's open calls are never idle-bounded, so its 10m idle bound
> is not a constraint - see idleGuard). A project that lowers max_tool_call below
> this must lower the brief too, or its sessions will choose a wait the driver
> kills. TestRunnerWaitBoundStaysUnderTheToolCallCaps pins all three.

### runnerForbidden

> runnerForbidden is the shared prohibition every runner-claimed brief carries.
> The runner owns the lifecycle around the session, and a session doing any of
> this competes with it: a second claim or a heartbeat races the lease, a PR or
> a CI poll duplicates what the runner opens and waits for, and a fetched skill
> page is the turn cost this design exists to remove.
>
> The PR clause is scoped to the brief's own terminal step because the fix and
> verify briefs still open the PR until the runner owns the close-out (B7/B8).

### runnerCloseOutBrief

> runnerCloseOutBrief is the close-out phase's brief for a runner-claimed
> session: the task is approved and awaiting merge, and the plane's own
> close-out instruction - which the context block carries, not this brief -
> owns the work after a red check is repaired. It must agree with that
> instruction: the plane tells an unclaimed session to "claim it", and here the
> runner already holds the claim, so the brief says so rather than leave the
> session claiming a task it already owns. This phase keeps its own writes: it
> hands the task back itself.


## `internal/config/repos.go`

### ErrRepoNotFound

> ErrRepoNotFound is the sentinel for a repo identity that resolves to no local
> checkout. Its text is the iteration-failure code the loop surfaces verbatim:
> a session that would otherwise start in the multi-repo parent — the exact
> wrong-repo-reads and cwd-scoped-grant shape this resolution exists to kill
> (docs/proposals/agent-rule-scoping.md, test cases 1 and 2) — is refused
> instead, loudly, with the identity that could not be resolved.

### ResolveCheckout

> ResolveCheckout maps one repo identity to the local checkout a session for it
> should start in.
>
> The identity is the task's `repo` field (`owner/name`), or — when the caller
> passes "" (no repo known) — the project's PRIMARY repo, which is what keeps a
> session out of the workdir even when the plane names no repo yet. Resolution
> order, first match wins:
>
>  1. an explicit `repos` entry keyed by the FULL identity ("owner/name");
>  2. an explicit `repos` entry keyed by its bare name ("name") — the common
>     spelling, since local checkouts are named by their last path segment;
>  3. the workdir itself, when its own basename IS the bare name — the
>     single-repo layout, where the operator points `workdir` at the checkout;
>  4. `<workdir>/<bare name>`, when that directory exists — the multi-repo
>     parent convention, no declaration needed;
>  5. nothing: ErrRepoNotFound. Never "start in the workdir anyway" — a silent
>     fallback to the parent is the bug being fixed, so once anything is
>     configured (or any step above could have matched), failure is loud.
>
> An empty identity is resolved through the same steps against the primary:
> `primary_repo` when set, else the sole `repos` key when exactly one is
> declared, else — only when NOTHING is configured at all — ("", nil), meaning
> "this config has no opinion"; the caller then keeps its legacy workdir
> behaviour, which is what keeps every existing single-repo config working
> unchanged. A project that declares repos but neither a resolvable primary nor
> a sole entry gets ErrRepoNotFound: ambiguity is the operator's to resolve,
> not the driver's to guess.
>
> Configured paths may be relative (resolved against workdir, like every other
> path in this config) or start with ~. Every result is verified to exist as a
> directory before it is returned — a declared-but-absent path fails here, at
> the resolution step, rather than as a cryptic exec error three frames deeper.

### DeclaredCheckouts

> DeclaredCheckouts resolves EVERY repo a project declares — each `repos` entry
> plus the primary — to the checkouts a session's permission policy must cover,
> whatever directory the session starts in (agent-rule-scoping piece 2). The
> spawn cwd is NOT included by construction; callers drop it if it collides.
>
> Entries that resolve to nothing are skipped here rather than failed: this is
> the reach-widening list, where omitting an unknown entry degrades to today's
> scope, while failing would stop a run over half a declaration. doctor reports
> each entry's resolution separately, which is where a broken path is seen.
>
> The output is deduplicated (two identities may share a checkout) and sorted
> (stable policy bytes across otherwise-identical configs).

### validateRepos

> validateRepos is the structural half of repo-map validation, run by Validate:
> keys and values must be non-blank. It deliberately does NOT stat the
> configured paths - a checkout that appears later (or sits on another machine
> sharing the config) must not refuse every run; resolution fails the iteration
> only when a session actually needs it, and doctor reports each entry's
> reachability before that. A primary naming no declared key is likewise left
> alone: the convention steps of the resolution can still legitimately match it.


## `internal/config/reviewmethod.go`

### reviewmethod.go

> reviewmethod.go — how a review phase produces its findings.
>
> The shipped method is one single-pass session: read the diff, write the
> record. `review_method: "maps-session"` replaces that session's turns with
> the maps+notes technique (CLA-864): the driver maps the diff into M/F items,
> sends them in chunks, and the session writes notes per item before a final
> findings turn turns those notes into the record. The technique is a runner
> feature through and through — the maps are built by the DRIVER in the worktree
> the runner prepared — so a harness that cannot continue one session across
> turns cannot run it, and Validate refuses rather than silently running only
> the first turn.

### MaxBytesTotal

> MaxBytesTotal caps the hunk text embedded across all M items. Zero
> selects DefaultMapsSessionHunkBytesTotal. The prompt is NOT chunked
> before it reaches a harness: opencode receives it as one argv string,
> and Linux caps a single argument at 128 KiB (MAX_ARG_STRLEN), so the
> default's ~80 KiB first turn leaves only ~48 KiB of headroom before a
> raised budget fails the spawn with E2BIG.

### StageModels

> StageModels resolves the model each stage runs on for a phase whose resolved
> model is phaseModel: a stage's own model wins, an empty one inherits the
> phase's, and a findings stage with neither runs on the notes stage's model —
> a live session cannot return to the harness's own default mid-conversation, so
> the notes model is what the findings turn actually produces. The runner and
> doctor both read this one function so the record and the preflight cannot
> disagree about which model wrote the findings.


## `internal/config/role.go`

### BuiltinPlannerPrompt

> BuiltinPlannerPrompt is the shipped planner brief, used when the stored
> `roles.planner` block carries no `prompt` of its own. It stays CLI-side, the
> same way the built-in phase briefs do: the plane stores policy, never prose
> the CLI hands a model.
>
> The envelope the composer adds around it carries the per-turn facts (claim
> token, session ref, pending messages, transcript). This half must therefore
> say only what is true of EVERY planner turn.

### RoleAssignment

> RoleAssignment is the stored `roles.planner` block as the CLI applies it: the
> harness, the model selection, the custom brief and the three per-turn bounds.
>
> It is decoded from the plane's document only (Config.Planner is `json:"-"`),
> and its harness/model are policy the operator ratified — never replaced by
> the local file's worker default. Validate deliberately does NOT check it: a
> bad planner role must not refuse a project's worker drain config, and a
> machine that serves several projects may serve a planner on only some of
> them. `doctor` and the planner loop report/refuse it at the point of use.

### ResolvePlanner

> ResolvePlanner resolves the stored planner role into a session. It is strict
> where the phase chain is forgiving: a role naming an unknown harness, or a
> tier that resolves to nothing, yields an error and NO session rather than
> running on a default. A planner answering under the wrong model is a policy
> breach the operator cannot see, which is why the whole no-fallback rule
> exists.


## `internal/config/routeschedule.go`

### RouteSchedule

> RouteSchedule is a named pricing schedule that can be attached to model
> ROUTES (CLA-947). A route inside one of its windows is unavailable for a NEW
> session exactly as a route carrying a usage mark is; a running session is
> never interrupted.
>
> It reuses PauseWindows wholesale rather than reimplementing the day/clock
> arithmetic: `pause_windows` answers "start nothing anywhere" (quiet hours,
> machine load), while a route schedule answers "do not pay peak rates for THIS
> provider+model". Both may be set, and they are different questions.
>
> The schedule hangs off the route ID (the exact model string the driver
> iterates), never off the provider or the model alone: the same DeepSeek model
> reached through a flat-priced provider has no peak tier, so a model-keyed
> schedule would wrongly pause it.

### RouteWindowAt

> RouteWindowAt reports whether route is inside one of its schedule's windows at
> now (or inside the window's lead), and when that window ends.
>
> It is a pure function of (route, now) — plus the config, which does not change
> under a drain — so a caller can drive it from a fixed clock.

### RouteChoiceAt

> RouteChoiceAt is the pricing-window filter over an ordered route list. It is
> shared by the loop (which layers usage marks on top) and the supervisor's
> conflict session, so both answer "which route may this new session start on?"
> the same way.
>
> A route with no active window is available. When every route is inside a
> window, the schedule's when_all_blocked decides: "run" makes those routes
> available again (peak price accepted), "wait" returns no routes and the
> earliest window end.

### validateRouteWindows

> validateRouteWindows refuses a schedule that cannot be read and a route
> attachment naming a schedule that does not exist — either is a setting the
> loop would silently ignore. A route ID that matches no configured route is
> deliberately NOT refused here: doctor reports it, so a config listing a route
> from a paused experiment still starts.
>
> Names and routes are checked in sorted order so the message does not depend on
> map iteration.


## `internal/config/runconfig.go`

### Run-config consumption (CLA-410)

> Run-config consumption (CLA-410): the CLI side of the plane's stored
> execution-config document (CLA-408, phase 1). The plane stores a per-project
> document whose keys are this file's own snake_case config names; at start of
> run and on every backlog-summary `runConfigVersion` bump, the loop overlays
> that document over its local file — per project — so each project's sessions
> run under its own ratified policy. Until a project has one (version 0, or a
> document carrying nothing), the local file rules byte for byte.
>
> What is consumed is exactly what CLA-410's bar names plus the two numeric
> backstops stored in this config's native units: harness, model, tier buckets
> (`models`, and the per-harness blocks' model/models half), budget,
> escalation rules, max_turns, max_session_wall_clock — and, since CLA-598,
> the PIPELINE: `prompt`, `phases` and `review_panel` (operator decision
> 30d69869 decoupled that from the CLA-454 state-machine reshape; the CLI still
> sequences sessions itself, it now reads the sequence from the plane). Those
> three are ONE section: carrying any of them replaces all three, so a stored
> `phases` clears a local `prompt` and panel, and a stored `prompt` clears a
> local sequence (decision 6bf1fa15's per-section rule).
>
> `review_panel_concurrency` is NOT part of that section: it is MACHINE-OWNED
> (operator decision e1365721). The run's RESULT must be the same on every
> machine; a machine may change only how fast it gets there, so the local
> file's cap stays in force whatever a stored document carries, and the overlay
> neither replaces nor clears it. A stored document that names it has the value
> accepted and ignored, never applied.
>
> Deliberately NOT consumed yet: transitions (the workflow document is data for
> the same reshape), backlog_url and allow_local_mcp_servers (part of the local
> bootstrap core), notes (console-facing reasoning).

### RunConfigSchemaVersion

> RunConfigSchemaVersion is the newest `$schema_version` this build
> understands. A stored document carrying a HIGHER version is refused at both
> consume points (the loop's applyRunConfig and doctor's checkRunConfigs),
> loudly and keeping the previous config — never partially applied. The
> version bump is the one signal that means "the keys you do know may no
> longer mean what you think", which is exactly what the unknown-keys rule
> below cannot cover: extra keys degrade to unconsumed, but a newer schema
> may have changed the meaning of the keys this build DOES know.

### RunConfigPanelMember

> RunConfigPanelMember is one stored review-panel member, mapped onto
> ReviewPanelMember. A member may set both `model` and `tier` (the alias wins
> on expansion), exactly as in the local file — the one-model rule applies to a
> configured PHASE, not to a member.
>
> Optional travels with the member (CLA-784): a trial seat configured in the
> stored document must stay a trial seat when the overlay applies it, or every
> fleet applying that pipeline would silently promote it to a required reviewer.

### RunConfigEvents

> RunConfigEvents mirrors the plane's `events` block (CLA-601/603/887). Absent
> reads as both dials off. Activity carries tool names, one identifying string
> per call and todo snapshots; Transcript is the second opt-in and adds a
> completed call's output and each assistant text block. The consumer is
> resolveRunConfigEvents on the plane; this side must fail closed on its own, and
> a stored `transcript: true` without activity is only ever read as off.

### RunConfigRole

> RunConfigRole is one stored role block (`roles.planner`), mapped onto
> RoleAssignment. It reuses the phase block's field names, plus
> `max_session_tokens` — the per-session bound a conversation resumes against.
> There is no `name`: a role is keyed by its own name, and a role's `prompt` is
> a custom brief, never a built-in selector.
>
> `roles` is a WHOLE SECTION of the stored document (studio-chat.md §1): absent
> inherits the org default, `roles: {}` is the explicit opt-out. Only `planner`
> exists in v1, and the plane refuses any other key before it is stored.

### RunConfigDoc

> RunConfigDoc mirrors the plane's stored execution-config document — only the
> keys this build consumes. JSON decoding ignores everything else, so a newer
> plane's added keys degrade to "unconsumed" rather than unmarshal errors;
> Empty reports that case so the caller can say "nothing applied" honestly.
> A newer `$schema_version` is NOT covered by that rule — it means the known
> keys themselves may have changed meaning — so the consume points refuse it
> outright; see RunConfigSchemaVersion and SchemaNewer.
>
> Prompt, Phases and ReviewPanel are the PIPELINE section (CLA-598). Its
> presence is judged by KEY, not by value: a nil Phases/ReviewPanel is an
> absent key, while an empty non-nil slice is a key that clears the local
> value, and a `prompt` key counts however empty its string is (presence rides
> in promptSet, which UnmarshalJSON fills — a plain string cannot tell `"prompt":
> ""` from an absent key).
>
> ReviewPanelConcurrency is deliberately NOT part of that section: it is
> machine-owned (decision e1365721), so the overlay never applies it. The field
> stays here only because the plane stores the key and a client must be able to
> decode the whole document; it is accepted and ignored, never copied onto the
> config.

### pipelineSet

> pipelineSet reports whether the stored document carries ANY key of the
> pipeline section (prompt, phases, review_panel), which is what makes the
> overlay replace all three: the section is one unit, exactly as the plane
> stores it (decision 6bf1fa15). Presence is judged the way JSON can express it
> — a nil slice is an absent key, an empty one is a key that clears — so
> `"phases": []` still counts. `prompt` counts by KEY too: an explicitly present
> empty string clears the local pipeline rather than reading as absent.
>
> `review_panel_concurrency` is NOT a section key (decision e1365721): a
> document carrying only that is not a pipeline statement at all, so the local
> pipeline stays untouched.

### ApplyRunConfig

> ApplyRunConfig overlays a stored document onto c IN PLACE: a field the
> document sets wins, a field it omits keeps the local value — except the
> `events` and `roles` sections, which belong to the stored document alone (an
> absent block is both events dials off, fail closed, and no planner, fail
> closed: no local file may turn either on). Maps replace
> wholesale when present — the document is the operator's ratified word for
> that map, and merging would resurrect entries the ratification dropped.
> Callers re-run Validate afterwards: an overlay CAN produce a combination the
> local file alone could not (a stored harness name nobody registered), and a
> refused combination must be loud, not silently half-applied.
> A newer `$schema_version` never overlays, not even through a direct call:
> the consume points (the loop's applyRunConfig, doctor's checkRunConfigs)
> refuse it LOUDLY before calling; this early return is the quiet backstop
> behind them, so a future caller that forgets the check still keeps the
> previous config instead of running keys whose meaning may have changed.
>
> A harness swap is refused outright when it would hand the new harness the
> old one's machine-local wiring (CLA-475): SessionFor attaches the run-wide
> model, models, config_dir, mcp_config_path and settings_path to whichever
> harness is top-level, and ResolveMCPConfig attaches a project's own
> mcp_config_path to the top-level harness, so re-pointing `harness` over a
> base that wires the old harness run-wide (or per project) silently re-homes
> that dialect. The document carries no machine fields to re-wire the new
> harness with, so the only safe swaps are the ones that inherit nothing:
> the base sets none of those fields run-wide (or per project), or the new
> harness already declares its own values for every one the base sets. Anything
> else returns an error and leaves c untouched — the callers log it on the
> existing loud "REFUSED locally" path and keep the previous config. The
> supervisor's per-instance harness override rides the same check through
> CheckHarnessSwap: it re-points the same top-level over the same machine
> layer through a different door.

### The event channel's dials (CLA-603) are reset FIRST, before the empty

> The event channel's dials (CLA-603) are reset FIRST, before the empty
> document returns: the stored, effective document is the authority, so an
> absent block means both dials off (fail closed) — including the plane's
> default `{}` document, so a caller reusing a live config cannot keep the
> channel on a project that says nothing. No local-file value can turn one
> on; the machine can only opt out (NoEvents), which this overlay never
> touches.

### The roles section is the stored document's alone too (studio-chat.md

> The `roles` section is the stored document's alone too (studio-chat.md
> §1/§2): a nil map means the project inherited no planner and chat is OFF,
> which is the fail-closed answer for a project that never ratified one.
> The empty document cannot swap a harness, so the fail-closed reset happens
> before its early return; a non-empty one is assigned only AFTER the swap
> gate, so a refused document leaves c untouched as the contract says.

### The PIPELINE is one section (decision 6bf1fa15)

> The PIPELINE is one section (decision 6bf1fa15): a document carrying any
> of prompt/phases/review_panel replaces all three, and a key it omits is
> cleared to its zero value rather than kept from the local file. That is
> what keeps prompt and phases mutually exclusive ACROSS the overlay
> boundary — a stored phases clears a local prompt, and the reverse — and
> what stops a stored phases running with a local panel it never named. A
> document carrying none of the three leaves the local pipeline untouched,
> as before (handled by pipelineSet, not by this branch).
>
> `review_panel_concurrency` is deliberately absent from this block: it is
> machine-owned (decision e1365721), so the local file's value survives a
> section replace whatever the document carries, and a document that sets
> only that key is not a pipeline statement at all.

### CheckHarnessSwap

> CheckHarnessSwap refuses a document that re-points the top-level harness
> when the swap would hand the new harness the old one's machine-local wiring
> (CLA-475) — the same guard ApplyRunConfig enforces, exported so the
> supervisor's per-instance harness override (which re-points the top-level
> over the same machine layer through a different door) rides exactly it. A
> nil or harness-less document, or one naming the current top-level
> (whitespace aside), is not a swap and always passes. An empty top-level is
> not a free pass: run-wide wiring with no named harness would still be
> handed to the new top-level on a swap, so it is refused like any other
> inheritance. A refusal leaves c untouched.

### refuseHarnessSwap

> refuseHarnessSwap reports whether re-pointing the top-level harness from the
> base's to newH would hand newH the old harness's dialect. It is called
> BEFORE any mutation, so an error leaves the base untouched for the caller's
> loud refusal path.
>
> SessionFor fills Model, Models, ConfigDir, MCPConfigPath and SettingsPath
> from the run-wide fields for the top-level harness only; ResolveMCPConfig
> additionally serves a project's own mcp_config_path to the top-level
> harness. The stored document carries none of the machine fields, so after a
> swap the new top-level inherits whatever the base still holds run-wide (or
> per project) unless it already declares its own value for that field —
> from the local harnesses block, or, for the policy pair, from the document
> itself. A "non-empty block" alone is not enough: a block carrying only a
> model still inherits the old config dir through its gaps, which is the exact
> failure this refuses. The check is therefore per field, not per block.

### RunConfigDocument

> RunConfigDocument renders the config's MOVABLE half as the plane-side
> proposal document `propose-config` imports: the pipeline (prompt, phases,
> review_panel, and each phase's model), the consumed families and the two
> backstops, in the plane's own snake_case vocabulary with wall clock as
> integer seconds. Nothing machine-local (workdir, wiring, env, state dirs) is
> included — importing those would ask the plane to custody what it must not,
> and the origin rule depends on the trusted statement staying where the
> operator wrote it until ratified.
>
> `review_panel_concurrency` is deliberately NOT exported (decision e1365721):
> it is how many panel seats run at once on THIS machine, not part of the
> run's result, so it stays machine-owned. A ratifying round-trip therefore
> cannot move one machine's cap onto every other.
>
> It returns an error for a pipeline shape the PLANE refuses even though this
> file's Validate tolerates it: a panel with no built-in, non-final review phase
> to expand (inert locally, a stored no-op on the plane — decision 94497792).
> propose-config must never offer a document the operator cannot ratify.

### The pipeline section (CLA-598), in the plane's own vocabulary

> The pipeline section (CLA-598), in the plane's own vocabulary. `prompt` is
> exported only when it is the OPERATOR's: defaults() fills in the built-in
> prompt, and exporting that would have propose-config replace the plane's
> pipeline with prose the operator never wrote.
>
> A panel the exported sequence cannot expand is refused, not exported: the
> plane stores a panel only beside the sequence it multiplies, and also
> refuses `prompt` beside one (decision 94497792), so an unphased config's
> panel is a document the operator could never ratify.

### CLA-784 (review 1.review-1.3)

> CLA-784 (review 1.review-1.3): the stored-run-config schema is the
> PLANE's, and it does not yet know `review_panel[].optional` — its
> validator refuses the key as unknown, so a document carrying a trial seat
> is one the operator could never ratify. Refuse here rather than emit it,
> and rather than silently DROPPING the flag: dropping it would promote the
> trial seat to a required reviewer on every machine that applies the stored
> pipeline, the exact harm `optional` exists to prevent. The two sides must
> move in lockstep — the plane's stored-run-config schema learns the key and
> bumps its `$schema_version`, this build's RunConfigSchemaVersion moves in
> step, and this gate lifts.

### CLA-869

> CLA-869: the v1 stored-run-config schema refuses unknown PHASE keys too, so
> `review_method` and `maps_session` are keys the plane would reject at
> ratification. Refuse to export them rather than silently dropping them:
> a dropped method is every machine running a single-pass review while this
> config says maps-session — the config and the run disagreeing with nothing
> said. The two sides move in lockstep, exactly as the trial seat above: the
> day the plane's schema learns the keys and bumps `$schema_version`, this
> build's RunConfigSchemaVersion moves in step and this gate lifts.


## `internal/config/session_idle.go`

### The shipped watchdogs

> The shipped watchdogs. Both harnesses that enforce the idle guard get a
> per-call cap, because each adapter can tell when a call is in flight: claude
> from its tool_use/tool_result blocks, opencode from the step whose tools it
> cannot see until they settle - step_start opens the window and step_finish
> closes it (CLA-900). The cap is what bounds a call that never returns; without
> one the paused idle clock would leave a stuck call unbounded.
>
> opencode's idle bound sits above its own 10-minute bash-tool ceiling, so a
> 10-minute call survives even when the adapter never sees the step opened. A
> LONGER call that never produces a step_start is still idle-killed: a stream
> that stopped producing events is read as silence, not as a call.


## `internal/config/state.go`

### ResolveStateDir

> ResolveStateDir returns the absolute path where control markers and iteration
> logs live.
>
> It defaults OUTSIDE the workdir (CLA-259), to
> `$XDG_STATE_HOME/clankerbar/loop/<slug>` — `~/.local/state/...` when that
> variable is unset. It used to be `<workdir>/.clankerbar-loop`, which put the
> daemon's own writes inside the one tree its spawned sessions are permitted to
> write, and that placement was the root of three separate defects rather than a
> detail: transcripts a session could read or commit, and paths a session could
> pre-plant a symlink at to make the daemon truncate a file outside the
> confinement the adapters impose on the session. Moving it removes the class
> instead of guarding three symptoms — the session cannot reach the directory at
> all.
>
> The slug is `<workdir basename>-<hash of its absolute path>`. The hash keeps
> two checkouts that share a basename apart; the basename keeps the directory
> recognisable when an operator goes looking for a transcript. It is derived
> from the cleaned absolute path and nothing else — deliberately NOT from
> EvalSymlinks, because a symlinked workdir that resolves differently once its
> target exists would silently move an operator's STOP marker out from under
> them.
>
> An explicit state_dir always wins, including one pointed back inside the
> workdir: that is a supported thing to want, and internal/statedir keeps its
> guarantees there too.

### absWorkDir

> absWorkDir is the workdir as an absolute, cleaned path.
>
> Validate already made WorkDir absolute (see the comment there — resolving it
> per use against the caller's cwd is what gave one config file two state dirs).
> The fallback is only for a Config that was never validated, which is tests and
> nothing else.

### SessionWorkDirs

> SessionWorkDirs is every directory a spawned session actually runs in,
> absolute, resolved the way the loop resolves them.
>
> It is the CONFINEMENT boundary expressed as paths: a session may write
> anywhere under one of these and nowhere else, so these are exactly the trees
> in which a symlink is something that may have been planted rather than
> something the operator laid out. statedir.Open takes them for that reason.


## `internal/config/task_context.go`

### TaskContext

> TaskContext is everything the runner hands a claimed session up front, so the
> session does not spend its first turns on get_task, git plumbing and
> archaeology. The runner fills it; the session reads the rendered block and
> starts work.
>
> Every empty field is omitted from the block: a session must be able to tell
> "the runner attempted no merge" from "the merge produced nothing to say".
>
> Resumed means the branch already carried prior work, so the session
> re-validates the task against the merged tip before anything else. Merge is
> "merged", "up-to-date" or "" (not attempted). Conflicts are the paths the
> runner's merge of PRBase stopped on before the runner aborted it. MergeHint
> is the plane's own close-out instruction: the session must be able to follow
> it verbatim, so it is rendered as a section of its own and never shrunk.
> DiffBase is the commit a diff in this phase starts from — the runner's
> merge-base SHA, or origin/PRBase when no SHA was available — so a later phase
> (a maps-building review) can reuse the base the runner resolved.
> Questions and Decisions are newest-first except for the blocking questions,
> which the plane appends at the END. RunnerNext is one sentence naming what
> the runner does after this session ends.
>
> TaskID and RunID are the ids every task write must be signed with. A
> runner-claimed session never claimed, so these are the only place it learns
> them: the runner briefs name record_progress(taskId, runId) but carry no
> placeholder for either.

### TaskContextBlock

> TaskContextBlock renders the block above the brief. It is deterministic (no
> map iteration, input order preserved) and never exceeds taskContextBlockMax
> bytes. Decisions and questions arrive newest-first, so the oldest (the tail)
> go first: non-blocking questions, then decisions, then blocking questions,
> and only then is the detail truncated; the truncation note says where the
> full text is. Blocking questions go last because the plane appends older
> live blockers at the END of the list and one of them is the reason the task
> stopped - trimming the tail blindly would drop exactly that. The close-out
> instruction is header-like and is never shrunk: dropping the plane's own
> instruction would leave the session inventing one.


## `internal/config/validate.go`

### StepHarnessError

> StepHarnessError is Validate's refusal for a configured STEP whose harness
> this machine cannot run: a phase, or a review-panel member, that names a
> harness nothing registered, or one other than the run's top-level harness
> with no local `harnesses.<h>` block to give it its machine wiring (CLA-598,
> extending the rule CLA-366 set).
>
> The prose stays in Error(); the step label and harness are carried apart so a
> caller can report them without matching on the message — `doctor` turns this
> one into a FAILED check naming the project, step and harness, because a
> stored document whose step this machine cannot spawn is a ratified policy
> that silently will not run here, not the ordinary "keep the local rules"
> trouble.

### Validate

> Validate normalizes path fields and checks the resolved config is runnable.
>
> The sections run in a fixed order and the first error wins: the harness
> must be known before the MCP auto-fill, and later checks read fields
> earlier ones fill in. Do not reorder.

### The workdir becomes absolute HERE, once, before anything is derived fr

> The workdir becomes absolute HERE, once, before anything is derived from it
> — the same treatment MCPConfigPath/SettingsPath/ConfigDir get on the next
> three lines. It used to stay as written and be resolved at each point of use
> against whatever cwd that process had, so one config file described
> different runs: `clankerbar run` from the repo and `clankerbar doctor` from
> ~ hashed to two DIFFERENT state dirs (ResolveStateDir -> stateSlug). Doctor
> then reported "no stop markers" about a directory the running loop had never
> opened, and a stray state dir accumulated at every cwd anyone invoked from.
> Before CLA-259 the same cwd-dependence was at least visible as
> `./.clankerbar-loop`; a hashed name under ~/.local/state hides it.
>
> An empty workdir still means "where the daemon was started" — that is the
> directory the child inherits either way. It is just pinned now, not re-read.
> Sticky: Validate is not guaranteed to be called only once, and a second call
> sees the absolute value the first one wrote — it would then answer
> "configured" about a workdir nobody configured.

### A harnesses block keyed by a name no adapter answers to is INERT

> A `harnesses` block keyed by a name no adapter answers to is INERT: nothing
> ever looks that key up, so its model, tier map and paths are read by nobody
> and the operator's settings quietly stop applying. That is the shape a
> rename leaves behind - CLA-675 moved the 2.x preview onto `opencode`, and a
> config still carrying a `harnesses.opencode2` block (with the top-level
> `harness: opencode`) was accepted and ignored - so it is refused here,
> against the same registry as `harness` above and the budget blocks below,
> with the same rename hint. Names are checked in sorted order so the message
> does not depend on map iteration.

### A per-harness block keyed by a name no adapter answers to is a ceiling

> A per-harness block keyed by a name no adapter answers to is a ceiling that
> can never trip: nothing is ever charged to it, so the run is unbounded on
> exactly the side the operator meant to bound. Refuse it here, against the
> same registry as `harness` above, rather than at 3am with an inert breaker.
> Names are checked in sorted order so the message does not depend on map
> iteration. A negative ceiling INSIDE a block is doctor's finding, not this
> one, exactly as for the dials beside it — see checkBudget.

### Two answers to the same question

> Two answers to the same question. A phased task is asked its PHASE's
> prompt and never this one, so a config carrying both is refused rather
> than resolved — an operator who set one and forgot the other gets told,
> instead of wondering which of two prompts their sessions were handed.
>
> "Set" means "differs from the built-in default": defaults() fills Prompt
> before the file is layered over it, so an untouched Prompt is
> indistinguishable from an absent one by the time we get here. The residue
> is that writing the default string out by hand alongside phases is
> accepted silently, which costs nothing — it is the same prompt, unused.

### Phases rest entirely on the adapter observing the session's claim

> Phases rest entirely on the adapter observing the session's claim: the
> handback across a seam, the salvage, and the delivery check all gate on
> Result.Claim.Held(). An adapter that never populates it would not fail —
> it would implement and push and then stop after phase 1 on EVERY task,
> reported by a log line that reads like an ordinary early finish. Refuse
> here instead, where an operator can see it, rather than at 3am.
> A per-harness config and a single-harness FLAG are two answers to the same
> question, and the flag's premise is the one that has stopped being true.
> Refused rather than resolved, exactly as `prompt` alongside `phases` is:
> --harness moves which harness SessionFor treats as top-level, so the other
> one silently starts inheriting the run-wide fields that belong to this one
> (a claude model alias reaching another harness's --model is the failure the
> whole per-harness block exists to make impossible), and --model applies to
> one phase's sessions while the rest quietly ignore it.

### A phase on some OTHER harness inherits none of the run-wide

> A phase on some OTHER harness inherits none of the run-wide
> harness-shaped fields, by design (see HarnessConfig), so with no block
> of its own it would spawn with no config dir, no MCP config and no
> settings — a session with no clankerbar tools, which looks from the log
> like a model that declined the work. Refuse here, where the operator can
> see it, rather than at 3am.

### The conflict-only session step (CLA-759)

> The conflict-only session step (CLA-759). It names a harness the SUPERVISOR
> will start a session on, so it is held to the same two rules as a phase's
> own harness above: it must exist in the registry, and when it is not the
> run-wide one it must carry a block of its own — inheriting another
> harness's dialect is the 3am failure the phase rule exists to prevent.
> Checked even when the block only pins the model, because the effective
> harness is then the implement step's and has already been checked.

### The review panel (CLA-639)

> The review panel (CLA-639). Its members run one review phase each, so a
> member naming a harness is naming a harness the sequence will spawn on —
> held to the same two rules as a phase's own harness above, and refused
> here for the same reason: the alternative is a paid review session with
> none of its harness's fields, or one that dies on a name nothing answers
> to, discovered at 3am rather than at startup.
>
> Validation runs on the EFFECTIVE panel: a member with no harness of its own
> inherits the REVIEW phase's, so that inherited harness is checked here too
> (the phase rules above resolve it to the run-wide harness when the phase
> names none, so there is exactly one answer).
>
> An empty panel is refused when it is the key the FILE wrote and it would
> MEAN something — a phased config with a built-in review phase to expand.
> With no such phase the panel is inert (a config with its own review prompt
> owns its contract, an unphased config has no phase sequence at all) and
> refusing it would break the marshalled round trip of a config that never
> mentioned a panel.

### A panel expands exactly ONE built-in review phase

> A panel expands exactly ONE built-in review phase. A sequence with more
> than one is refused rather than resolved: the panel would multiply one and
> leave the other as a lone review, and which one it picked would be
> positional trivia the operator could only discover from the log. A review
> with its own prompt is not built-in and is not counted here; the
> no-expand refusal below catches a sequence whose only review is custom.

### CLA-784

> CLA-784: an OPTIONAL member is a TRIAL seat — a reviewer the operator is
> evaluating, whose failure is recorded against that seat and never blocks
> the fix. A panel made ONLY of optional seats therefore has no reviewer a
> round could ever be said to have produced: every task would run the
> members, take whatever they wrote as nobody's review, and spawn no fix.
> Refused at the door rather than discovered from a log line.

### The label is also the iteration-log tag, the phasesRun entry and the

> The label is also the iteration-log tag, the phasesRun entry and the
> dead-phase tally cell, so it must not shadow a phase either: a member
> named `review` would merge with the real review phase in every log and
> tally (ReviewShaped/escalation still key on the phase's prompt, so only
> the reporting would be a lie — which is enough). An explicit name is
> refused rather than renamed because the operator wrote it; a DERIVED
> label that collides is refused with the remedy (set `name`), because
> the position alone cannot avoid the collision.

### A member runs one review phase of the sequence, so it is held to the

> A member runs one review phase of the sequence, so it is held to the
> same claim-tracking rule a configured phase is (the rule above): a
> member that cannot observe or resume the task claim would end the
> sequence early or hand the task back over its own work. Checked on the
> RESOLVED harness, so a member that inherits the review phase's
> harness is covered too.

### A panel multiplies the review phase, so a config that names one but ha

> A panel multiplies the review phase, so a config that names one but has a
> review phase it cannot multiply is two contracts for one sequence. Refused
> rather than resolved: a FINAL review runs the historical combined brief and
> a review phase with its own prompt owns its own contract, so in either
> shape the panel would be a no-op the operator could only discover from the
> log. An UNPHASED config has no phase sequence at all — the key is inert
> there, not a no-op a run could have honoured.

### Phases rest on EVERY phase's adapter observing the session's claim, no

> Phases rest on EVERY phase's adapter observing the session's claim, not only
> the claiming one's.
>
> The first phase has to OBSERVE a claim, or there is nothing to hand on. A
> later phase is handed one through Invocation.ResumeClaim — but seeding it is
> the adapter's job (harness.newSessionResult), and an adapter that does not
> track claims does not seed either: it returns a zero Result.Claim whatever
> it was given. That is not merely a lost capability. `drainPhase` records a
> checkpoint only for a phase that ends holding the task, so a non-tracking
> phase in the middle ends every sequence early with a log line that reads
> like an ordinary finish, and a non-tracking phase at the END leaves
> `drainPhases` carrying its predecessor's claim into the handback — which can
> post `ready` over a task that phase has just landed at in_review.
>
> So the rule is per phase, and the message names the phase, because in a
> mixed sequence "the harness" is no longer one thing.

### A configured phase names its model ONE way (operator decision

> A configured phase names its model ONE way (operator decision
> 3eddb7a5): a direct alias or a tier bucket, never both. ModelForPhase
> would silently let the alias win, which would leave an operator reading
> a tier that is stored but inert — the same ambiguity the plane refuses
> (CLA-722). Checked on c.Phases, which is the configured sequence: a
> phase SYNTHESIZED from a review-panel member is not here, and a member
> may still set both (its expansion clears the tier, so the alias wins by
> construction — see Phase.panelMember).

### The resume placeholders are filled from the claim the PREVIOUS phase l

> The resume placeholders are filled from the claim the PREVIOUS phase left
> held, so the first phase has nothing to fill them from. The driver leaves
> an unfilled one standing rather than blanking it — a literal {{runId}} in
> a log is a misconfiguration announcing itself — but there is no reason to
> spend a session discovering that when the config says it up front.
>
> Checked against the RESOLVED prompt, not the configured one: the usual way
> to get this wrong is `phases: [{"name": "review"}, …]`, whose configured
> prompt is empty and whose built-in brief is full of placeholders.

### The fix phase is the one that carries the branch-shaped terminal step

> The fix phase is the one that carries the branch-shaped terminal step (PR,
> in_review), so it must be LAST: a non-final fix would open the PR and hand
> the task over, and the phase after it would resume a task that has already
> left the run. It also needs a preceding review phase by name, because its
> shipped brief's first act is to read the review record's `## Review` block
> from the task — a fix with no review before it can only report a failed
> hand-off. Both rules apply to the BUILT-IN brief only; an operator's own
> prompt owns its own ordering.
>
> CLA-638 relaxes the LAST rule for exactly one successor: a built-in
> VERIFICATION phase. EffectivePhases swaps the fix's resolved prompt to the
> no-PR variant; the verifier then carries the terminal step, and the driver
> repeats the fix→verify pair until a round finds no open MUST or the round
> cap parks the task.

### The verification round (CLA-638) has one legal position and one legal

> The verification round (CLA-638) has one legal position and one legal
> predecessor. Position: LAST, because the driver repeats the fix→verify pair
> after it and a phase after the verifier would never be reached. Predecessor:
> the built-in fix brief, because there must be a phase that applies findings
> between the verifier's rounds — without it the loop cannot make progress,
> which is exactly the non-terminating sequence this rule refuses. The
> verifier also needs an earlier review phase whose MUSTs it adjudicates; a
> sequence with no review has no findings and no ids to verify.

### A non-final BUILT-IN review names the phase after it as the one that a

> A non-final BUILT-IN review names the phase after it as the one that applies
> its findings, and the driver's review→fix seam requires the review record to
> have been written. A built-in phase there that is NOT the fix brief cannot
> satisfy either (the other built-ins claim a task, or run the combined
> review-and-fix brief and re-review from scratch), so a config that names one
> is refused at startup rather than spending a review to discover it. A phase
> with its OWN prompt is allowed: applying the findings is then the operator's
> own contract.

### Default mcpconfigpath to workdir/.mcp.json when that file exists

> Default mcp_config_path to <workdir>/.mcp.json when that file exists. Claude's
> -p mode does NOT auto-discover .mcp.json, so without this a bare `clankerbar
> run` from a workdir that carries one would spawn sessions with no clankerbar
> tools at all — and the poller could derive no slug. Explicit config still wins.
>
> A file that arrives THIS way was found, not named, and is held to the
> discovered-file rule before anything else reads it (CLA-266): it may not
> declare local-process servers nor carry opencode-schema policy keys.
> checkMCPConfigOrigins below still applies to it either way — a discovered
> file can pass this gate and still fail the origin one.

### Where the account-scoped API key is allowed to go, settled once, here

> Where the account-scoped API key is allowed to go, settled once, here
> (CLA-257). backlog_url is the operator's own statement of it and is held to
> the TLS floor; the workdir's .mcp.json is untrusted input and may not name a
> different host. Refusing at Validate means `doctor` reports it as a failed
> config check and `run` never starts — neither one makes a credentialed
> request to the host the file named.
> An empty backlog_url used to mean "take the origin from .mcp.json"; with that
> road closed it would mean "no origin at all", which is a silent blind drain
> for a config that merely omitted a field. Fill it, so a validated config
> always has exactly one trusted origin to check the rest against.

### The single-project split-brain, refused the same way the multi-project

> The single-project split-brain, refused the same way the multi-project
> one is. With no `projects` block the poll's slug comes from
> `mcp_config_path` alone (see BacklogEndpoint), while this harness's
> sessions work whatever ITS file names — so two files whose slugs disagree
> gate on one project's counts and claim, work and hand back another's. It
> is a two-file typo away in exactly the shape the README documents, and
> nothing downstream would notice.
>
> Only when both files name a slug: a file that names none (or that this
> cannot parse) constrains nothing, which is the same latitude the
> per-project check allows.

### A phase on a harness other than the run's needs its own MCP config PER

> A phase on a harness other than the run's needs its own MCP config PER
> PROJECT, because that file carries both the schema (the harness's) and the
> slug (the project's). Falling back to the single
> `harnesses.<name>.mcp_config_path` would point every project's phase at one
> project's queue, which is the split-brain the slug check above refuses when
> it is written down — so refuse it here too, where it would otherwise be
> arrived at by omission.

### The KEY is checked against the harness registry too, and here that

> The KEY is checked against the harness registry too, and here that
> matters more than in the sibling env maps: an inert
> `env_per_harness` entry merely fails to add a variable, whereas an
> allowlist keyed by a name no adapter answers to is never consulted.
> SessionEnvAllowlist looks up the harness actually spawned, finds no
> per-project list for it, and can then fall back to nil — which
> inheritEnv reads as FULL INHERITANCE. A typo or a retired name would
> therefore switch the filter OFF instead of narrowing it, the one
> direction this feature exists to prevent. Refused here, against the
> same registry as `harnesses` and `budget.per_harness`, with the same
> rename hint.

