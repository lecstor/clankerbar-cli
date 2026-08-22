CLA-361 implementation record (2026-08-23)
------------------------------------------
Isolation and the guard are structural, in `internal/teststate`. (History
note for anyone diffing this branch against its base: the net diff never
touches config.go. An intermediate commit on this branch did add a
`SetTestStateDir` override there, and a later commit removed it in favour of
this package; neither exists relative to the base.)

How it works
------------
Every test binary that can reach the state-dir derivation installs

    func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m)) }

`teststate.Isolate` resolves the REAL loop state root first (before any env
override), snapshots its entries and those of the parent `clankerbar` dir,
points `XDG_STATE_HOME` at a fresh temp dir for the whole binary run
(inherited by spawned subprocesses; a per-test `t.Setenv` still overrides it
where wanted), then after `m.Run()` compares both real dirs against the
snapshots: any new entry fails the binary naming the paths. Coverage today:
internal/config (via config_test), internal/cli, internal/loop - exactly the
importers of internal/config.

That coverage is pinned, not hoped for: `TestEnforcedEverywhereConfigIsImported`
(internal/teststate) runs `go list -json ./...` and fails the suite if any
package with tests can reach internal/config without installing the TestMain.
A future package that grows a doctor-style test therefore fails in CI instead
of leaking to disk. If the real root cannot be resolved at all (HOME unset in
a hermetic sandbox), Isolate degrades to isolation alone - a warning names the
disabled guard and the tests still run.

Verified 2026-08-23 (this branch)
---------------------------------
- Reproduced on clean origin/main (1aff567): one full `go test ./...` run
  created 4 new `001-<hash>` dirs under ~/.local/state/clankerbar/loop.
- This branch: full uncached `go test -count=1 ./...` green, zero new entries.
- Guard probe (isolation Setenv temporarily disabled): internal/cli FAILed
  naming the 4 dirs it created, so the guard detects rather than passes
  vacuously. Reverted before commit. (internal/loop and internal/config did
  not leak in that probe; their TestMains are precautionary coverage.)
- Enforcement probe: a scratch package importing internal/config with no
  TestMain made TestEnforcedEverywhereConfigIsImported fail naming it.
- go vet ./... and gofmt clean.

Note for reviewers
------------------
The guard flags ANY new entry under the real root (or its parent) during a
run, including one a live loop creates mid-suite - and in THIS repo that is
the recurring case, not a corner: the loop's state dir slug is a hash of the
workdir, and the workflow gives every task a fresh worktree, so every loop
start in a new worktree mints a new entry. The red is therefore
self-healing rather than permanent: the entry joins the next run's baseline
snapshot, so a re-run is green. The guard's own output says exactly that.
Treat one red guard line naming a `dev-`/`<repo>-<hash>`-shaped entry as a
loop that started mid-suite (re-run before investigating); anything else is a
real leak.

Cleanup of the ~1400 historical leaked dirs belongs to the operator:
    find ~/.local/state/clankerbar/loop -maxdepth 1 -name '001-*' -type d
