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
- **`PROMOTION_PR_TOKEN`.** A scoped PAT with `Contents: read` + `Pull requests:
  write` on this repo, used only by `promote.yml`. It is needed because the repo
  setting "Allow GitHub Actions to create and approve pull requests" is off, so
  the default `GITHUB_TOKEN` can edit an open PR but is refused when it must
  *create* one - which happens every cycle, since merging the promotion closes it.
  Without the secret, `promote.yml` goes red rather than silently skipping.

## Break glass

Cut a release by hand:

```bash
git tag v0.4.0 && git push origin v0.4.0
```

That still triggers `release.yml` via `push: tags`. Use it when the derivation is
wrong or a release must be re-cut; prefer fixing the commits otherwise.
