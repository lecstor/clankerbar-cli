package supervisor

// Roll — phase 5b of the daemon-supervisor proposal (docs/proposals/
// daemon-supervisor.md in the clankerbar repo): ONE operation updates a
// running fleet onto a new binary, replacing the per-daemon manual sequence
// (stop by hand, install, start back up — with nothing verifying the fleet
// returned). The roll runs as `clankerbar supervise roll`, from the NEW
// binary, after the operator installed it at the fleet's launch path.
//
// Per child, one at a time: the roll writes the RESTART marker into its state
// dir (the daemon drains at its iteration boundary, never mid-session), then
// waits for the child's NEXT LOCAL BEACON to report the target version — the
// version THIS binary was built as — before touching the next child. The
// beacon is the daemon's own self-report (its version.Current, written beside
// the plane beacon by internal/loop since phase 5b), so the verify gate
// observes what the child ACTUALLY runs, not what the supervisor assumes: a
// child that re-execs onto the swapped launch path reports the new version, a
// child that respawns through the supervisor reports what the new binary
// reports, and a child that comes back on anything else never satisfies the
// gate.
//
// A child that fails to come back on the target version within VerifyTimeout
// HALTS the roll: the error names the child, and the remaining children are
// left untouched — already-rolled children stay on the new version, and the
// operator can fix the stuck child and re-run the roll (children already on
// the target are skipped). The roll touches local placements only (Decision
// 7); remote entries are ignored exactly as reconcile ignores them.
//
// The pre-flight check makes the operator's swap explicit: the binary at the
// fleet's launch path must report the roll's own version. A roll whose launch
// path still carries another build refuses BEFORE any child is touched — the
// operator forgot to install, or is running the roll from a copy that is not
// the launch path — rather than rolling nothing and reporting success.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
	"github.com/lecstor/clankerbar-cli/internal/version"
)

const (
	// defaultVerifyTimeout bounds ONE child's drain-and-come-back. A drain
	// finishes at the child's iteration boundary and can legitimately take
	// minutes (an in-flight session completes), so the wait is generous; the
	// cap exists so a child that never comes back halts the roll instead of
	// hanging it. The roll is one-shot — a timed-out child is safe to retry.
	defaultVerifyTimeout = 10 * time.Minute

	// defaultRollLogEvery is how long the roll may wait on one child without
	// saying so — the same silence gate the fleet stop uses: the wait itself
	// is never bounded except by VerifyTimeout, only its silence is.
	defaultRollLogEvery = 30 * time.Second

	// rollPollInterval is how often the roll re-reads the child's local
	// beacon while waiting. The daemon refreshes its beacon on its own poll
	// cadence (idle interval), so a fast poll here costs nothing.
	rollPollInterval = 250 * time.Millisecond
)

// Roll runs one fleet roll: every local running child is restarted at its
// iteration boundary and verified to report the target version — this
// binary's own — via its next local beacon, one child at a time, halting on
// the first child that fails to come back. Children already on the target
// version are skipped.
func Roll(ctx context.Context, o Options) error {
	o = o.withDefaults()
	if o.VerifyTimeout <= 0 {
		o.VerifyTimeout = defaultVerifyTimeout
	}
	target := version.Current

	// Pre-flight: the swap has to be real before any child is touched. The
	// operator installs the new build at the launch path and runs the roll
	// from it; a launch path that still reports another version means the
	// children would re-exec (or be respawned) onto the OLD binary, and the
	// roll would burn one VerifyTimeout per child before halting on the
	// first. Refuse now, naming both versions.
	launchV, err := o.launchVersion()
	if err != nil {
		return fmt.Errorf("roll: cannot read the version of the fleet launch binary %s: %w", o.Binary, err)
	}
	if launchV != target {
		return fmt.Errorf("roll: the fleet launch binary %s reports version %s, but this roll runs %s - install the new build at the launch path and run `clankerbar supervise roll` from it", o.Binary, launchV, target)
	}

	cacheDir := o.RosterCacheDir
	if cacheDir == "" {
		dir, err := rosterCacheDir()
		if err != nil {
			return fmt.Errorf("roll: %w", err)
		}
		cacheDir = dir
	}

	// The fleet comes from the same account-scoped roster the supervisor
	// reconciles against — the roll is an operator action over the DESIRED
	// fleet, never over process listings — with the same cached fallback when
	// the plane is unreachable (a roll over the last-known-good roster is
	// still a roll over the fleet the supervisor is serving).
	entries, err := fetchRoster(ctx, o, cacheDir)
	if err != nil {
		return fmt.Errorf("roll: %w", err)
	}

	rolled, already, notRunning, untouched := 0, 0, 0, 0
	for i := range entries {
		e := &entries[i]
		switch {
		case e.Placement == RosterPlacementRemote:
			// Decision 7: remote placement is not implemented; the roll
			// ignores it exactly as reconcile does.
			log.Printf("roll: skipping entry %q - placement remote is not implemented (Decision 7)", e.Name)
			untouched++
			continue
		case e.DesiredState != RosterDesiredRunning:
			log.Printf("roll: skipping entry %q - desired state is %q, not running", e.Name, e.DesiredState)
			untouched++
			continue
		}
		if err := checkEntry(e); err != nil {
			log.Printf("roll: skipping entry %q - %v", e.Name, err)
			untouched++
			continue
		}

		dir := rosterStateDir(cacheDir, e.Name)
		cur := readLocalBeacon(dir)
		if cur != nil && cur.Version == target {
			log.Printf("roll: %s already runs %s - skipping", e.Name, target)
			already++
			continue
		}
		if _, err := os.Lstat(dir); err != nil {
			// No state dir means no child ever ran under this supervisor —
			// a fresh entry or a refused one. Nothing is running, so nothing
			// is rolled; the line says so loudly rather than letting the
			// summary's "rolled N" read as the whole fleet.
			log.Printf("roll: %s has no state dir yet - it is not running under this supervisor; skipping (once it is up, re-run the roll)", e.Name)
			notRunning++
			continue
		}

		before := "no beacon yet"
		if cur != nil {
			before = cur.Version
		}
		log.Printf("roll: rolling %s (was %s) - writing RESTART; it drains at its iteration boundary, then comes back on %s", e.Name, before, target)
		if err := writeRestartMarker(dir, e.Name); err != nil {
			return fmt.Errorf("roll: %s: %w - halting the roll (%d child(ren) already rolled)", e.Name, err, rolled)
		}
		if err := waitForNextBeacon(ctx, dir, e.Name, target, o.VerifyTimeout); err != nil {
			return fmt.Errorf("roll: %w (%d of %d local running child(ren) rolled before the halt)", err, rolled, countRunning(entries))
		}
		rolled++
	}

	log.Printf("roll complete: %d child(ren) on %s (%d already on it, %d not running, %d not touched)", rolled, target, already, notRunning, untouched)
	return nil
}

// launchVersion is the Options.LaunchVersion hook with its default: run the
// binary at the launch path with `version` and parse the line.
func (o Options) launchVersion() (string, error) {
	if o.LaunchVersion != nil {
		return o.LaunchVersion()
	}
	if o.Binary == "" {
		return "", errors.New("no binary to probe")
	}
	out, err := exec.Command(o.Binary, "version").Output()
	if err != nil {
		return "", err
	}
	return parseVersionLine(string(out))
}

// parseVersionLine extracts the version from `clankerbar version` output
// ("clankerbar <version>\n"). Anything else is an unrecognized binary — the
// pre-flight must refuse rather than guess.
func parseVersionLine(out string) (string, error) {
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	rest, ok := strings.CutPrefix(line, "clankerbar ")
	if !ok || strings.TrimSpace(rest) == "" {
		return "", fmt.Errorf("unrecognized version output %q", line)
	}
	return strings.TrimSpace(rest), nil
}

// fetchRoster is the roll's roster read: one fetch, with the cached
// last-known-good fallback when the plane is unreachable — the same posture
// reconcile takes. A roll with no roster at all (not wired, unreachable and
// no cache) is refused: rolling nothing because the fleet could not be seen
// would read as success.
func fetchRoster(ctx context.Context, o Options, cacheDir string) ([]RosterEntry, error) {
	client := NewRosterClient(o.RosterURL, o.APIKey)
	entries, err := client.Fetch(ctx)
	if err == nil {
		return entries, nil
	}
	if errors.Is(err, ErrNotWired) {
		return nil, err
	}
	if cached := loadCachedRoster(cacheDir); len(cached) > 0 {
		log.Printf("roll: plane unreachable (%v) - rolling against the cached roster", err)
		return cached, nil
	}
	return nil, fmt.Errorf("plane unreachable (%v) and no cached roster - nothing to roll against", err)
}

// writeRestartMarker drops a RESTART marker into one child's state dir — the
// same marker `clankerbar ctl restart` writes, honoured at the daemon's
// iteration boundary. The dir must already exist (the caller gates on it): a
// marker written into a dir the supervisor created for a child that never
// started would sit until a spawn and then re-exec a child that was never
// asked to roll. A marker already present is one pending request too many and
// is left alone: it will land, and the wait for the beacon starts regardless.
func writeRestartMarker(dir, name string) error {
	st, err := statedir.Open(dir)
	if err != nil {
		return fmt.Errorf("cannot open its state dir: %w", err)
	}
	defer st.Close() //nolint:errcheck // read-side handle on the way out
	body := []byte(fmt.Sprintf("fleet roll requested by the supervisor at %s\n", time.Now().UTC().Format(time.RFC3339)))
	if err := st.WriteFile(loop.MarkerRestart, body); err != nil {
		if errors.Is(err, statedir.ErrExists) {
			log.Printf("roll: %s already has a RESTART pending - waiting for it to land", name)
			return nil
		}
		return fmt.Errorf("cannot write RESTART into %s: %w", dir, err)
	}
	return nil
}

// waitForNextBeacon waits until the child's local beacon reports the target
// version — the NEXT beacon, not a stale one — or VerifyTimeout elapses.
// Timeout is the failure the doneWhen names: the child never came back on the
// new version (it never re-exec'd or respawned, or it came back on the old
// binary and its fresh beacons keep reporting the old version), and the roll
// must halt rather than continue through the fleet.
//
// The freshness slack is load-bearing: a beacon written in the same second
// the wait starts truncates to a timestamp BEFORE the wait (RFC3339 seconds
// precision on the wire, nanoseconds in memory), and a fresh target beacon
// must not read as stale. A pre-existing target-version beacon cannot exist
// here anyway — the caller already skipped children whose beacon reported the
// target, and the last write wins — so the version match is the real gate and
// the timestamp is belt-and-braces.
func waitForNextBeacon(ctx context.Context, dir, name, target string, timeout time.Duration) error {
	start := time.Now()
	deadline := start.Add(timeout)
	lastLog := start
	ticker := time.NewTicker(rollPollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", name, ctx.Err())
		}
		if b := readLocalBeacon(dir); b != nil && b.Version == target && !b.At.Before(start.Add(-time.Second)) {
			log.Printf("roll: %s: the next beacon reports %s - rolled", name, target)
			return nil
		}
		now := time.Now()
		if !now.Before(deadline) {
			last := "no beacon yet"
			if b := readLocalBeacon(dir); b != nil {
				last = fmt.Sprintf("%s (at %s)", b.Version, b.At.UTC().Format(time.RFC3339))
			}
			return fmt.Errorf("%s did not come back on %s within %s (last beacon: %s) - halting the roll; its session drains at the iteration boundary, so re-running the roll after fixing it is safe", name, target, timeout, last)
		}
		if now.Sub(lastLog) >= defaultRollLogEvery {
			lastLog = now
			log.Printf("roll: still waiting for %s to come back on %s (since %s) - its in-flight session finishes at its iteration boundary", name, target, start.UTC().Format(time.RFC3339))
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

// localBeacon mirrors internal/loop's LocalBeaconName JSON shape (the same
// roster-style lockstep: two packages, one contract).
type localBeacon struct {
	Version string    `json:"version"`
	State   string    `json:"state"`
	At      time.Time `json:"at"`
}

// readLocalBeacon reads one child's local beacon. Absent, corrupt, or
// version-less all read as nil — the roll treats "cannot see the child's
// self-report" as "not on the target yet" and keeps waiting.
func readLocalBeacon(dir string) *localBeacon {
	data, err := os.ReadFile(filepath.Join(dir, loop.LocalBeaconName))
	if err != nil {
		return nil
	}
	var b localBeacon
	if err := json.Unmarshal(data, &b); err != nil {
		return nil
	}
	if b.Version == "" {
		return nil
	}
	return &b
}

// countRunning is the roll's halt-error denominator: the local running
// children the roll set out to roll.
func countRunning(entries []RosterEntry) int {
	n := 0
	for i := range entries {
		if entries[i].Placement != RosterPlacementRemote && entries[i].DesiredState == RosterDesiredRunning {
			n++
		}
	}
	return n
}
