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

1. **Task PRs target `staging`**, never `main`. A clanker servicing its close-out
   queue merges approved task PRs there.
2. **A standing `staging -> main` promotion PR** stays open and current. It is
   created and updated automatically on every push to `staging`.
3. **Only the operator merges that promotion**, and merging it is what publishes.

This mirrors the web repo (`lecstor/clankerbar`) deliberately - one mental model
across both, decided 2026-08-10 - though what a promotion *does* differs: over
there it deploys a Worker, here it publishes a public GitHub release.

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
| `ci.yml` | PRs, pushes to `main` and `staging` | build, vet, `go test -race`, govulncheck |
| `promote.yml` | push to `staging` | create-or-update the standing promotion PR |
| `release-on-merge.yml` | push to `main` | gate on `ci`, derive the version, tag, publish |
| `realign-staging.yml` | push to `main` | fast-forward `staging` back up to `main` |
| `release.yml` | a pushed tag, or called by `release-on-merge` | GoReleaser: build, checksums, attestation |

The decision logic - what version, and did CI pass - lives in Go
(`internal/release`, driven by `cmd/release-tool`), not in YAML, because those
are exactly the rules that need tests. `cmd/release-tool` is development tooling;
`.goreleaser.yaml` builds `./cmd/clankerbar` only, so it never ships.

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
  happens every cycle, since merging the promotion closes it. This repo holds **no
  secrets**, which is the point: the alternative was its first long-lived
  credential. Turning the setting back off breaks the standing PR on the cycle
  *after* the next publish, not immediately - so the failure lands well away from
  the change that caused it.
- **`ci.yml` running on push to `staging`.** Not a convenience: it is what makes
  the promotion PR mergeable at all. A PR created by `GITHUB_TOKEN` triggers no
  `pull_request` run, so the `ci` check `main` requires can only come from the push
  run on the head commit, which a required check resolves against. See the comment
  in `ci.yml`, where each branch in the push list is load-bearing for a different
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
