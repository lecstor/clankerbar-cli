#!/usr/bin/env bash
# Report one CI check's outcome to the plane: POST /api/check-reports.
#
# Ported from `lecstor/clankerbar`'s scripts/report-check.sh (CLA-818, CLA-821).
# The mechanism is deliberately identical - one script, one shape of body, one
# never-fail rule - so the two repos' evidence cannot drift apart.
#
# WHY THIS EXISTS. Under the batch preset (`review_granularity: per_batch` or `none`
# with `authority_integrate: clanker`) a task leaves `in_review` for `approved` with no
# human verdict, and one of the three arms that decides it is CI. That arm accepts
# REPORTED evidence only - green rows a repo's own workflow POSTed to the plane at the
# task's head commit - and never the agent's attestation (`apps/web/src/server/auto-approve.ts`
# in `lecstor/clankerbar`, decision 7b9a7abc). Until this script existed, no workflow in
# this repo posted anything, so the arm always failed and no CLI task could auto-advance.
# The plane never polls GitHub: the repo has to say so itself, which is this.
#
# WHY IT NEVER FAILS THE JOB, AND WHY THAT IS NOT SLOPPINESS. The report describes the
# job's own conclusion, so a reporting step that could fail would be able to contradict
# itself: `job.status` is read at the start of the step, a POST that then failed would
# leave the plane holding "success" while GitHub turned the check red - two surfaces
# disagreeing about one run, with the optimistic one stored. Every path here exits 0. A
# failed POST is an `::warning::` annotation; a skip (no key, no jq/curl, nothing to report)
# is a `report-check:` log line. A dropped report costs evidence and so leaves the task at
# `in_review` for a human, which is the safe direction; a red `ci` because the plane had a
# bad minute is not.
#
# WHY THE SHA IS PASSED IN rather than derived here. `ci` runs on `refs/pull/<n>/merge` -
# the PR already merged into its base - so `github.sha` is a synthetic commit the branch
# does not have, and a report keyed to it matches no task. The caller passes the PR HEAD
# sha; see the step in `.github/workflows/ci.yml`.
#
# WHY THE SECRET IS EMPTY SOMETIMES, AND THAT IS NORMAL. Fork PRs are not given repository
# secrets at all, and a repo that has not minted a report-only key yet has none either. Both
# are expected states, not failures, so both take the same one-line skip.
#
# WHY ONLY `ci` HERE. The plane repo also reports conflict-guard's `merges-cleanly`, but
# this repo has no conflict-guard workflow: a check that never runs would leave a STALE
# GREEN row (reports are newest-wins per check), which reads as evidence for the one thing
# nobody checked.
#
# USAGE - the last step of a job, so it describes the gate it follows:
#
#   - name: Report the check outcome to the plane
#     if: always() && github.event_name == 'pull_request'
#     continue-on-error: true
#     env:
#       CLANKERBAR_REPORT_KEY: ${{ secrets.CLANKERBAR_REPORT_KEY }}
#       CHECK: ci
#       OUTCOME: ${{ job.status }}
#       COMMIT: ${{ github.event.pull_request.head.sha }}
#       BRANCH: ${{ github.event.pull_request.head.ref }}
#       PR: ${{ github.event.pull_request.number }}
#     run: scripts/report-check.sh
#
# ENV IN:
#   CLANKERBAR_REPORT_KEY  a REPORT-ONLY key (403 otherwise - docs/auth.md in the plane
#                          repo). Empty: skip.
#   GITHUB_REPOSITORY      from the runner; the `repo` field (`owner/name`).
#   CHECK                  the check name to store. This repo reports `ci`.
#   OUTCOME                GitHub's job conclusion: success | failure | cancelled. The
#                          plane's one green token is `success` (`isGreenOutcome`).
#   COMMIT                 the sha to key the report to, ideally the PR head sha.
#   BRANCH, PR             what the report is ABOUT. At least one is required; both is best.
#   CLANKERBAR_BASE        optional, default https://clankerbar.com. Test seam only.
#
# EXIT: always 0. See "WHY IT NEVER FAILS THE JOB" above.
set -uo pipefail

BASE="${CLANKERBAR_BASE:-https://clankerbar.com}"
ENDPOINT="$BASE/api/check-reports"

say() { echo "report-check: $*"; }
warn() { echo "::warning title=clankerbar check report::$*"; }

# A skip is a log line a human reading the run can find, then a clean exit. Never an
# `::error::`, and never `exit 1` - that would be the self-contradiction in the header.
skip() {
  say "$1 - skipping, nothing reported"
  exit 0
}

key="${CLANKERBAR_REPORT_KEY:-}"
repo="${GITHUB_REPOSITORY:-}"
check="${CHECK:-}"
outcome="${OUTCOME:-}"
commit="${COMMIT:-}"
branch="${BRANCH:-}"
pr="${PR:-}"

[ -n "$key" ] || skip "CLANKERBAR_REPORT_KEY is empty (unset secret, or a fork PR)"
[ -n "$repo" ] || skip "GITHUB_REPOSITORY is empty"
[ -n "$check" ] || skip "CHECK is empty"
[ -n "$outcome" ] || skip "OUTCOME is empty"
[ -n "$commit" ] || skip "COMMIT is empty"
# The endpoint refuses a report that names neither (the plane could not tie it to any work),
# so say so in the log rather than posting a body it will 400.
if [ -z "$branch" ] && [ -z "$pr" ]; then
  skip "neither BRANCH nor PR is set, so the report would be about nothing"
fi

# Tasks record their PR as `#N` (delivery.pr), and the plane matches it exactly.
case "$pr" in [0-9]*) pr="#$pr" ;; esac

command -v jq >/dev/null 2>&1 || skip "jq is not installed"
command -v curl >/dev/null 2>&1 || skip "curl is not installed"

# Built with jq rather than printf so a branch name (generated from a task title by an LLM,
# so it can hold anything git refs permit) cannot break out of the JSON string.
payload=$(
  jq -n \
    --arg repo "$repo" \
    --arg check "$check" \
    --arg outcome "$outcome" \
    --arg commit "$commit" \
    --arg branch "$branch" \
    --arg pr "$pr" \
    '{repo: $repo, check: $check, outcome: $outcome, commit: $commit}
     + (if $branch != "" then {branch: $branch} else {} end)
     + (if $pr != "" then {pr: $pr} else {} end)'
)
if [ $? -ne 0 ] || [ -z "$payload" ]; then
  warn "could not build the JSON body (jq failed) - nothing reported"
  exit 0
fi

body_file=$(mktemp)
# shellcheck disable=SC2064  # expand now: the file exists now and the name is final
trap "rm -f '$body_file'" EXIT

# The key goes in on stdin (`--config -`), not argv, which other processes can read.
code=$(
  curl --silent --show-error \
    --output "$body_file" \
    --write-out '%{http_code}' \
    --max-time 10 \
    --retry 2 --retry-delay 2 --retry-connrefused \
    --request POST \
    --header 'Content-Type: application/json' \
    --config - \
    --data "$payload" \
    "$ENDPOINT" 2>/dev/null <<<"header = \"Authorization: Bearer $key\""
)
curl_status=$?
if [ "$curl_status" -ne 0 ]; then
  warn "POST $ENDPOINT failed (curl exit $curl_status) - nothing reported"
  exit 0
fi

case "$code" in
  2[0-9][0-9])
    say "reported $check=$outcome at $commit for ${pr:+PR $pr}${pr:+${branch:+ / }}$branch (HTTP $code)"
    ;;
  *)
    # `--show-error` output went to /dev/null above, so the response body is the whole
    # diagnosis, and it is the plane's own `{ error: { code, message } }`.
    detail=$(head -c 400 "$body_file" | tr '\r\n' '  ' | sed 's/%/%25/g')
    warn "POST $ENDPOINT returned HTTP $code: $detail"
    ;;
esac
exit 0
