CLA-361 Phase 1 Checkpoint (2026-08-23)
----------------------------------------
Verified: `go test ./...` passes (all packages green). Guard test
(`TestNoTestWritesUnderRealLoopStatePath`) reports no pollution within the
config package. Full suite run creates 12 new pollution dirs under
`~/.local/state/clankerbar/loop` (4502 -> 4514 entries) - the injectable
`SetTestStateDir` override does not fully cover fixture-derived workdirs.

Durable state (already on repo/task):
- Branch: `clanker/7ae88db2-go-test-runs-pollute-the-live-loop-state`
- Commits: 7d8172c (guard), 7aba3a9 (isolation fix in config.go)
- Worktree: `/Users/jason/dev/clankerbar-cli-wt/7ae88db2`
- Task status: `in_progress`, branch recorded, outcome updated

Next session (same run, same brief scope):
Extend or supplement `SetTestStateDir` isolation so full suite creates zero
pollution. Read `internal/config/config.go` lines ~2085+ (`testStateDirOverride`,
`stateHome()`), `internal/config/statedir_test.go` (guard). The pollution gap
is the remaining work: either cover fixture-derived config paths, or upgrade
to a cross-package suite-level guard that fails the full suite.
