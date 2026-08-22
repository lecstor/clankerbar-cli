CLA-361 implementation record (2026-08-23)
------------------------------------------
Supersedes the phase-1 note on this file's earlier revision: the
`SetTestStateDir` override approach is GONE (removed from config.go by the
salvage commit). Isolation and the guard are now structural, in
`internal/teststate`.

How it works
------------
Every test binary that can reach the state-dir derivation installs

    func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }

`teststate.Isolate` resolves the REAL loop state root first (before any env
override), snapshots its entries, points `XDG_STATE_HOME` at a fresh temp dir
for the whole binary run (inherited by spawned subprocesses; a per-test
`t.Setenv` still overrides it where wanted), then after `m.Run()` compares the
real root against the snapshot: any new entry fails the binary naming the
paths. Coverage today: internal/config (via config_test), internal/cli,
internal/loop - exactly the importers of internal/config.

Verified 2026-08-23 (this branch)
---------------------------------
- Reproduced on clean origin/main (1aff567): one full `go test ./...` run
  created 4 new `001-<hash>` dirs under ~/.local/state/clankerbar/loop.
- This branch: full uncached `go test -count=1 ./...` green, zero new entries.
- Guard probe (isolation Setenv temporarily disabled): internal/cli FAILed
  naming the 4 dirs it created, so the guard detects rather than passes
  vacuously. Reverted before commit.
- go vet ./... and gofmt clean.

Note for reviewers
------------------
The guard flags ANY new entry under the real root during a run, including one
a live loop creates mid-suite. That is deliberate (the leak must not return
silently) and cheap: live statedirs appear only when a loop run starts, not
per iteration, so the false-positive window is negligible.

Cleanup of the ~1400 historical leaked dirs belongs to the operator:
    find ~/.local/state/clankerbar/loop -maxdepth 1 -name '001-*' -type d
