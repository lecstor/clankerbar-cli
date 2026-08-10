#!/usr/bin/env bash
# Regenerate the standing `staging -> main` promotion PR body with the list of
# tasks/PRs currently queued for release (everything on `staging` not yet on
# `main`). Idempotent — safe to run after every merge into `staging`.
#
# Ported from the web repo's scripts/update-promotion-pr.sh. The operator's
# decision was to MIRROR the web app's shape here rather than invent a second
# one, so the mechanism, the table and the create-or-update behaviour are
# deliberately the same; what differs is what merging the PR does — over there it
# deploys a Worker, here it publishes a public GitHub release with attested
# binaries, which is why the "How to promote" section below is not a copy.
#
# CI runs this automatically: the `promotion-pr` job in
# `.github/workflows/promote.yml` invokes it on every push to `staging`, so the
# release PR stays present and current with no agent in the loop. Runnable by
# hand too (e.g. --dry-run) — the two never conflict, it's idempotent.
#
# Usage: scripts/update-promotion-pr.sh [--dry-run|-n]
#   --dry-run  print the generated body to stdout and exit. Read-only: it still
#              fetches and reads PRs via gh, but creates/edits nothing on GitHub.
# Requires: gh (authed), run from the repo root.
set -euo pipefail

BASE=main
HEAD=staging
TITLE="Promote staging → main (publish a release)"

DRY_RUN=false
case "${1:-}" in
  --dry-run | -n) DRY_RUN=true ;;
  "") ;;
  *) echo "unknown argument: $1 (expected --dry-run)" >&2; exit 2 ;;
esac

git fetch origin --quiet

# Ensure the standing promotion PR exists; capture its number.
pr=$(gh pr list --state open --base "$BASE" --head "$HEAD" --json number --jq '.[0].number // empty')

# Build the queued-work table from PR numbers referenced in `main..staging`
# commits (on staging, not yet on main). Read subjects on the FIRST-PARENT line
# only — the merges/squashes that landed on staging, not the inner commits they
# carried in (whose subjects may reference other PRs). Match both the merge-commit
# form (`Merge pull request #NN from …`) and the squash form (`… (#NN)`), so this
# works whichever merge method produced them. Both patterns are ANCHORED — so a
# mid-subject token (e.g. `Revert "x (#42)" (#99)`) yields only the real trailing
# PR number (#99), never the interior #42.
rows=""
for n in $(git log "origin/$BASE..origin/$HEAD" --first-parent --format='%s' \
  | grep -oE '^Merge pull request #[0-9]+|\(#[0-9]+\)$' \
  | grep -oE '[0-9]+' | sort -un); do
  meta=$(gh pr view "$n" --json title --jq '.title' 2>/dev/null || echo "")
  [ -z "$meta" ] && continue
  ref=$(printf '%s' "$meta" | grep -oE 'CLA-[0-9]+' | head -1 || true)
  # Strip only a leading "CLA-NN:" or a trailing "(CLA-NN)" — never a mid-sentence
  # id — for a clean description, then escape pipes so a title can't break the table.
  desc=$(printf '%s' "$meta" \
    | sed -E 's/^\(?CLA-[0-9]+\)?:?[[:space:]]*//' \
    | sed -E 's/[[:space:]]*\(?CLA-[0-9]+\)?[[:space:]]*$//' \
    | sed -E 's/[[:space:]]+$//')
  desc=${desc//|/\\|}
  label=${ref:-—}
  rows+="| ${label} | #${n} | ${desc} |
"
done

[ -z "$rows" ] && rows="| — | — | _(nothing queued — staging is level with main)_ |
"

# What the release WOULD be called, so the operator sees the version before
# merging rather than after. Best-effort and never fatal: this is a preview, and
# a derivation failure must not stop the PR body being updated.
version_line="_(version preview unavailable — see the release-on-merge run after merging)_"
previous=$(git describe --tags --abbrev=0 --match 'v[0-9]*' origin/"$BASE" 2>/dev/null || true)
if [ -n "$previous" ] && command -v go >/dev/null 2>&1; then
  if derived=$(git log --no-merges --format=%B%x00 "$previous..origin/$HEAD" \
      | go run ./cmd/release-tool next-version --current "$previous" 2>/dev/null); then
    # `|| true` on each, and it is load-bearing rather than defensive. These are
    # plain assignments in an `if` BODY, so unlike the `if` condition above they
    # are NOT exempt from `set -e`, and `pipefail` propagates grep's exit 1 when a
    # key is missing. Without these, a future rename of an output key would abort
    # the whole script and the promotion PR body would quietly stop updating -
    # the opposite of the "never fatal" this block promises.
    rel=$(printf '%s\n' "$derived" | grep '^release=' | cut -d= -f2 || true)
    ver=$(printf '%s\n' "$derived" | grep '^version=' | cut -d= -f2 || true)
    bump=$(printf '%s\n' "$derived" | grep '^bump='    | cut -d= -f2 || true)
    if [ "$rel" = "true" ]; then
      version_line="Merging this will publish **${ver}** (a \`${bump}\` bump from \`${previous}\`)."
    else
      version_line="Merging this will publish **no release** — nothing releasable since \`${previous}\` (docs/chores only). That is expected, not a failure."
    fi
  fi
fi

body=$(cat <<EOF
**Live view of what's queued for release.** This standing PR tracks every approved change merged onto \`staging\` that has **not yet been published**. It stays open and is updated on each merge into \`staging\`.

${version_line}

## Queued in this batch (staging ahead of main)

| Task | PR | Description |
|---|---|---|
${rows}
## How to promote
- **Merge as a MERGE COMMIT — never squash.** Squashing severs \`staging\`'s history from \`main\` and turns the automatic post-promote realign into a destructive force-push. (Squash and rebase merging are disabled on this repo for exactly this reason.)
- **Merging publishes.** \`release-on-merge.yml\` waits for a green \`ci\` on the merge commit, derives the version from the conventional commits since the last tag, pushes the tag and publishes the GitHub release with checksums and a build-provenance attestation. Nothing else needs doing.
- **A red or missing \`ci\` publishes nothing.** An absent check is treated as a refusal, not a pass.
- **The staging realign is automatic.** \`realign-staging.yml\` fast-forwards \`staging\` up to \`main\` on the merge, so the next batch starts release-equal.
- Details: \`docs/releases.md\`.
EOF
)

if [ "$DRY_RUN" = true ]; then
  printf '%s\n' "$body"
  exit 0
fi

if [ -z "$pr" ]; then
  # No open promotion PR. If `staging` is level with `main` (e.g. just after a
  # promotion fast-forward), there's nothing to promote and GitHub would reject an
  # empty-diff PR — no-op cleanly instead of dying on that 422.
  if [ "$(git rev-list --count "origin/$BASE..origin/$HEAD")" -eq 0 ]; then
    echo "staging is level with main — nothing to promote, no PR created"
    exit 0
  fi
  gh pr create --base "$BASE" --head "$HEAD" --title "$TITLE" --body "$body"
else
  gh pr edit "$pr" --body "$body" >/dev/null
  echo "Updated promotion PR #$pr"
fi
