# Releases

How a change gets from a task PR to a published binary, and which parts a human
holds.

## The shape

```
task PR  ──merge──▶  staging  ──standing promotion PR──▶  main  ──▶  release
   ▲                    ▲                    ▲                        │
 a clanker          a clanker           THE OPERATOR              published
 opens it           merges it           merges this               automatically
```

Three rules carry the whole model:

1. **Task PRs target `staging`**, never `main`. The task's last phase (the `fix`
   phase, or a final combined `review`) opens the PR when it hands the task to
   `in_review` (see the README's Phases section); a clanker servicing its
   close-out queue merges it there only once a human has approved it.
2. **A standing `staging -> main` promotion PR** stays open and current. It is
   created and updated automatically on every push to `staging`.
3. **Only the operator merges that promotion**, and merging it is what publishes.

This mirrors the web repo (`lecstor/clankerbar`) deliberately - one mental model
across both, decided 2026-08-10 - though what a promotion *does* differs: over
there it deploys a Worker, here it publishes a public GitHub release.

## This release requires the matching plane

The intent-specific tool cutover (CLA-746 in the plane, CLA-748 here) deleted
`update_task` and replaced it with the nine task tools. This release is the
**breaking CLI half of a plane/CLI pair**: it speaks plane protocol **41** and
nothing else.

- **Older planes are not supported.** There is no fallback and no feature
  detection: every baked brief names the new tools, and every driver write calls
  one. An older plane answers them as unknown tools, and the driver surfaces that
  as an error naming protocol 41 - never as a retry with the deleted spelling.
- **There is no compatibility floor to maintain.** That is what the coordinated
  cutover bought: the fleet is stopped, the plane promotion and this release go
  out, the new binary is installed, and the fleet is restarted. A bad cutover is
  rolled back on both sides together, because a mixed pair is never a supported
  state.
- **Do not publish ahead of the plane.** The plane promotion (CLA-746) must land
  in the same window as this release; publishing this binary against the old
  plane leaves every daemon unable to write a task.

The constant the code and its tests hold themselves to is
`plane.RequiredPlaneProtocol` in `internal/plane`; the version is the plane's
`protocolVersion`, reported beside `runConfigVersion` on every backlog summary.

## Why publishing is gated this way

Before this, clankers merged CLI task PRs straight to `main` and a human cut
releases by hand. That kept losing the race: on 2026-08-10, twice in one session,
`main` moved ahead of the released binary within minutes of a release being cut.
And a stale binary is *silent* - `~/.local/bin/clankerbar` once sat two weeks
behind the repo with nothing saying so.

Auto-releasing on merge to `main` would have fixed the staleness and created a
worse problem: unattended agents able to publish public releases with attested
binaries. `staging` is what resolves it. Clankers keep merging (the close-out
queue still works), and the publish gate stays a human's.

## The pieces

| Workflow | Trigger | What it does |
|---|---|---|
| `ci.yml` | PRs, pushes to `main` and `staging` | build, vet, `go test -race`, govulncheck - on the self-hosted runner; on PRs, also reports the `ci` outcome to the plane |
| `promote.yml` | push to `staging` | create-or-update the standing promotion PR |
| `release-on-merge.yml` | push to `main` | gate on `ci`, derive the version, tag, publish |
| `realign-staging.yml` | push to `main` | fast-forward `staging` back up to `main` |
| `release.yml` | a pushed tag, or called by `release-on-merge` | GoReleaser: build, checksums, attestation |

The decision logic - what version, and did CI pass - lives in Go
(`internal/release`, driven by `cmd/release-tool`), not in YAML, because those
are exactly the rules that need tests. `cmd/release-tool` is development tooling;
`.goreleaser.yaml` builds `./cmd/clankerbar` only, so it never ships.

## Where CI runs

`ci.yml`'s job runs on the **self-hosted runner** labelled `headless`, a container on
the fleet box (its name, as the run's log header reports it, is
`headless-clankerbar-cli`). The reason is cost: this repo is private, so every hosted job
minute is drawn from a finite allowance, and a self-hosted runner bills none. The other
workflows stay on GitHub-hosted runners - `promote.yml`, `release-on-merge.yml`,
`realign-staging.yml` and `release.yml` publish releases or hold tokens, and they run
rarely enough not to be what spends the allowance.

Moving the job here also moves a trust boundary. It checks out the PR's branch and
`go test -race` compiles and executes that branch's own test code on a persistent
container, whose installed toolchain, apt state and caches survive between jobs and are
inherited by later runs - including the push runs on `main` and `staging` whose `ci` the
release and realign gates read. The job holds the run-scoped `contents: read`
`GITHUB_TOKEN` and, on pull requests, the report-only `CLANKERBAR_REPORT_KEY` in its last
step's environment. That key is long-lived but can do one thing: post a check outcome to
the plane. A branch that persisted code into the container could read it and post a false
green, which under the per-batch policy can auto-approve a CLI task; the promotion PR's
human review is what still stands between that and a release. The runner is
registered to this repo alone - but any collaborator who can open a PR can reach this
box's CI execution path, so the check's result is only as trustworthy as every branch
that has ever run on it. Keep the container isolated from the fleet's own state and
credentials: no host mounts, no long-lived secrets beyond the report-only key, and rebuild it periodically (or run
it ephemeral) so a PR cannot persist into later checks.

Two things to know before waiting on a check:

- **It takes one job at a time**, capped at 2 CPUs and 4 GB. Two PRs pushed together
  compile in sequence, and `go test -race` on two cores is slower than on the hosted
  image - so allow more wall-clock, not less. The job carries a 20-minute timeout, so a
  hang cannot hold the only slot for GitHub's six-hour default.
- **If the runner is offline** - the box down, the container stopped, the runner
  unregistered - jobs **queue** and report nothing for as long as it stays offline.
  `ci` is a required check on `main` and `staging`, so nothing merges until it runs. A
  PR whose check never appears is this, not a red build: look for the runner rather than
  re-pushing.

The one-slot queue also lengthens the wait for the two workflows gated on `ci` for the
merge commit on `main`: `release-on-merge.yml` and `realign-staging.yml` both run
`await-check --required ci` against `${{ github.sha }}`, and that window now covers the
queued jobs ahead of this one as well as the job itself. Both allow 60 minutes
(`--timeout 60m` inside a 75-minute job), widened from the 30 minutes that sufficed when
`ci` was a parallel hosted run. A `ci` job hung behind a queued one can still exhaust the
window; when it does, publishing is refused and `staging` is not fast-forwarded.

That image is Ubuntu `actions-runner`, not the GitHub-hosted image, so a step needing a
preinstalled tool must install it. `ci.yml` conditionally apt-installs `zstd` (what
`actions/cache` shells out to, via `setup-go`'s `cache: true`), a C toolchain
(`go test -race` needs cgo), and `jq` and `curl` (the report step skips without them); the step is conditional because the container persists, so
only a fresh one pays the apt round trip. To move the job back to GitHub-hosted, change
its `runs-on` to `ubuntu-latest`: the install step then no-ops, because the hosted image
ships all of them.

## Versioning

Derived from **conventional commits** since the last `v*` tag. No one picks a
number.

| Commit | Bump |
|---|---|
| `feat:` | minor |
| `fix:`, `perf:`, `revert:` | patch |
| `docs:`, `chore:`, `test:`, `refactor:`, `style:`, `ci:`, `build:`, `review:` | none |
| any type with `!`, or a `BREAKING CHANGE:` footer | major |
| anything else | **patch** |

Three rules in that table are deliberate and easy to mistake for bugs:

- **Unrecognised means patch, not none.** A commit nobody classified is far more
  likely to be a real change written in a hurry than a no-op, and a change that
  ships nothing while saying nothing is the failure this whole pipeline exists to
  prevent.
- **At major 0, a breaking change bumps MINOR** - `v0.3.4` goes to `v0.4.0`, never
  to `v1.0.0`. Reaching 1.0 is a statement about stability that a maintainer
  makes; it must not be a mechanical consequence of someone typing `fix(config)!:`,
  which this repo has already done at 0.x without meaning 1.0.
- **A promotion carrying nothing releasable publishes nothing** - no tag, no
  release, and the run stays green. A docs-only promotion is a normal event, not a
  failure, and not an empty version bump either.

Derivation reads **non-merge commits** (`git log --no-merges`). Task PRs land as
merge commits, so the first-parent subjects are all `Merge pull request #NN`; the
conventional-commit prefixes live on the inner commits and nothing else can see
them.

An existing tag carrying a prerelease suffix (`v0.4.0-rc1`) is **refused**, not
stripped: what follows it is not derivable without knowing whether the rc shipped,
so the workflow goes red and a human picks.

## The gate: a missing check is a refusal

`release-on-merge.yml` gates on the `ci` check of **the merge commit on `main`** -
not on the promotion PR's own check.

That distinction is the point. `main`'s protection is `strict: false`, so a PR can
merge without being up to date with its base. CI runs against
`refs/pull/<n>/merge`, which is the merge *as it stood when the run happened*; if
the base advances afterwards, the commit that actually lands is a combination
nothing ever compiled.

Because `ci` and the release workflow are triggered by the same push, the gate
**waits** for the check to appear and finish. The wait resolves to a **failure**:

- red `ci` -> no release
- `ci` never appears -> no release
- `ci` never finishes -> no release
- `ci` `skipped`, `neutral` or `cancelled` -> no release

An absent check must never read as a pass. That is pinned by tests in
`internal/release/checks_test.go`, not merely asserted here.

## The trap that shapes `release-on-merge.yml`

**A tag pushed using `GITHUB_TOKEN` does not trigger another workflow.** GitHub
suppresses it to prevent recursion.

So the obvious implementation - a job on push-to-`main` that derives a version and
pushes a tag, letting `release.yml`'s `push: tags` trigger fire - creates the tag
and then *nothing builds*. The result is a tag with no release, which reads as a
broken release rather than as a missing trigger.

Two fixes were available: an app token / PAT that can push tags which do
re-trigger, or making `release.yml` `workflow_call`-able and invoking it directly.
**We invoke it directly.** It needs no new secret, nothing to rotate, and no
credential in the repo capable of pushing tags outside a workflow's control. The
`push: tags` trigger is kept as the break-glass path for a human-pushed tag.

## Supply chain

`release.yml` holds `contents: write`, `id-token: write` and
`attestations: write`, and every action in it is pinned to a full commit SHA -
CLA-261 did that deliberately, because the job publishes the binaries users
install and a moved tag on any dependency could substitute what ships. **Every
workflow that tags or releases inherits that obligation**: `release-on-merge.yml`
and `realign-staging.yml` pin to SHAs too, keep the trailing version comment as a
label only, and take the narrowest token scope that works (the gate job holds
`contents: read`; only the tag job gets `contents: write`).

The automated path publishes **checksums and a Sigstore build-provenance
attestation**, exactly as a hand-cut release did. Verify one:

```bash
gh attestation verify clankerbar_0.4.0_darwin_arm64.tar.gz -R lecstor/clankerbar-cli
```

## Setup this depends on

- **Branch protection.** `main` and `staging` both require the `ci` check and
  disallow force-pushes and deletion.
- **Merge method.** Squash and rebase merging are **disabled** repo-wide. The
  promotion must be a merge commit, or `staging` stops being an ancestor of `main`
  and `realign-staging.yml`'s fast-forward would become a destructive force-push.
- **"Allow GitHub Actions to create and approve pull requests" is ON.** Operator
  decision, 2026-08-10, taken over provisioning a scoped PAT. `promote.yml` opens
  the standing PR with the default `GITHUB_TOKEN`, and with this setting off that
  token can edit an open PR but is refused when it must *create* one - which
  happens every cycle, since merging the promotion closes it. This repo held **no
  secrets** when that decision was taken, which is the point: the alternative was
  its first long-lived credential (CLA-821 has since added one report-only key,
  below). Turning the setting back off breaks the standing PR on the cycle
  *after* the next publish, not immediately - so the failure lands well away from
  the change that caused it.
- **`CLANKERBAR_REPORT_KEY`, and only that secret.** The last step of `ci.yml`
  POSTs the check's own outcome to the plane on pull requests, because the plane's
  CI arm accepts only rows a repo's own workflow POSTed - never an agent's
  attestation. The secret is a **report-only** key (mint it at
  `/projects/<slug>/api-keys`; see `docs/auth.md` in `lecstor/clankerbar`): it can
  post one check outcome and reach nothing else, so a leaked CI secret cannot
  claim a task, record a verdict or write the backlog. Empty - unset, or a fork
  PR, which receives no repository secrets - logs one line and skips; it never
  fails the run.
- **`ci.yml` running on push to `staging`.** Not a convenience: it is what makes
  the promotion PR mergeable at all. A PR created by `GITHUB_TOKEN` triggers a
  `pull_request` run that parks at `action_required`, contributing no check run
  until approved - unlike the tag trap above, nothing is suppressed here, the
  run just parks. The `ci` check `main` requires is satisfied by the push run on
  the head commit, which a required check resolves against. See the comment in
  `ci.yml`, where each branch in the push list is load-bearing for a different
  reason.

## Two exposures this model creates, named rather than left implicit

**1. Who may merge to `main` is now load-bearing, and it is not fully enforced.**

The whole point of the promotion gate is that a clanker cannot publish. That
holds only as far as `main`'s merge rules hold. Today `main` requires the `ci`
check and **zero approving reviews**, and GitHub push restrictions are not
available on a user-owned repo - so nothing at the git layer stops an agent with
write access from opening a PR straight at `main`, watching `ci` go green, and
merging it.

Two things stand in the way, and it is worth knowing which is which:

- `release-on-merge.yml` **refuses to publish any commit that did not arrive
  through `staging`** (it checks in git that the merge's second parent is
  reachable from `origin/staging`). That is a real control, and it is the one
  actually stopping the scenario above from producing a release.
- Everything else is convention: the served protocol and `AGENTS.md` tell
  clankers to target `staging`.

If you want the merge itself refused rather than just the publish, that is a
branch-protection change on `main` and it is **yours to make deliberately**,
because the obvious version of it backfires: requiring one approving review would
also block *you* from merging the promotion, since GitHub forbids approving your
own PR and the promotion PR is authored by the same account. Making it work means
the promotion PR being authored by a *different* identity than the approver.

**2. Actions can now approve pull requests, and that setting is repo-wide.**

The setting `promote.yml` depends on is "create **and approve**" - GitHub does not
separate the two. So enabling PR creation for the promotion also grants every
workflow in this repo the ability to submit approving reviews, and workflows run
from the merged `staging` tree, which clankers merge into without human review.

Nothing exploits this today, because exposure 1 records that `main` requires
**zero** approving reviews - there is no review requirement for a workflow-submitted
approval to satisfy. The exposure is therefore **latent and conditional**: it
becomes real the moment someone adds a review requirement to `main` under exposure
1, which is exactly the fix that section invites. Doing so without also revisiting
this setting would install a gate that a workflow can satisfy by itself, which is
worse than no gate, because it looks like one.

The trade this replaced, stated so the choice stays legible: a scoped PAT would
have confined the capability to one credential with `Contents: read` +
`Pull requests: write`, but it would have been this repo's first long-lived secret,
readable by that same agent-mergeable tree - a narrower capability guarded by a
thing that can leak, versus a broader one that cannot. The operator took the
second. Revisit it if a review requirement ever lands on `main`.

## What the operator sees on the promotion PR

When the promotion PR is open but the `pull_request` run sits parked at
`action_required`, the PR reads `mergeStateStatus: UNSTABLE` rather than
`CLEAN`. This is **expected**, not a fault: the parked run has not executed,
so it contributes no check result. Merging is permitted anyway - the
push-to-`staging` `ci` run already satisfies the required check against the
PR's head SHA.

Approving the parked run is nonetheless **usually the right call**. Only the
`pull_request` run tests the actual merge (`refs/pull/<n>/merge`); the push run
tests the `staging` tip. When `main` and `staging` have diverged (as on the
2026-08-11 cycle, where `main` still carried the CLA-326 lineage that `staging`
lacked), the two compile different combinations. Both runs green is the
strongest signal available before merging - though per the gate above, even a
green `pull_request` run is a snapshot of its moment, and the final word is
`ci` on the merge commit that actually lands on `main`. Before merging, check
whether the branches have diverged
(`git fetch origin && git log --graph --oneline --left-right origin/main...origin/staging`);
if they have, approve the run. Approving has its own visible effect: the run
starts executing and reports pending under the required `ci` context, so the
PR stops reading CLEAN until it finishes.

Evidence: verified live on 2026-08-22 against the standing promotion PR #91
(open at the time) - its `pull_request` run sat parked at `action_required`,
the check rollup carried a single `ci` entry (the push run's), and the PR read
`MERGEABLE` and `UNSTABLE` at once - and against the 2026-08-11 cycle (PR #36:
the same parked run, an approval, then a second green `ci` check from the
`pull_request` event alongside the push run).

## Break glass

Cut a release by hand:

```bash
git tag v0.4.0 && git push origin v0.4.0
```

That still triggers `release.yml` via `push: tags`. Use it when the derivation is
wrong or a release must be re-cut; prefer fixing the commits otherwise.

### A tag that exists with no release behind it

`release-on-merge.yml` validates (`go test -race`, `govulncheck`) **before** it
pushes a tag, precisely so this should not happen. If it happens anyway - a
GoReleaser failure, a cancelled run between the tag and the publish - **delete the
tag rather than leaving it**:

```bash
git push --delete origin v0.4.0 && git tag -d v0.4.0
```

A dangling tag is not cosmetic. The next derivation takes `previous` from
`git describe --tags --abbrev=0`, and `.goreleaser.yaml` uses
`changelog: use: github`, which diffs against the previous **tag** - so every
commit the dangling tag covers would silently never appear in any published
release's notes. Deleting it puts those commits back in the next release.

Re-running the workflow does *not* clear this: the `tag` job sees the tag already
at that commit, reuses it, and the publish fails the same way.
