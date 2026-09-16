// Run-config consumption in the driver (CLA-410): the plane's stored execution
// config, fetched per project and applied AT THE ITERATION BOUNDARY.
//
// The signal is free: the backlog-summary poll this loop already makes between
// iterations carries `runConfigVersion` (CLA-408). When it moves for a target —
// or on the very first poll, which is how "at start of run" is satisfied — the
// target's effective config is rebuilt here, before the spawn gate reads it.
// A session already in flight finishes on the config it started with: the poll
// never happens mid-session, so "never applies mid-session" is structural,
// not a promise to keep.
//
// The failure posture copies applyReload's (CLA-461): a fetch that fails is
// retried at a poll-bounded rate; a document that arrives but fails local
// validation is refused loudly and the previous config stays. Neither converts
// an operator's ratify click into a daemon outage.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// SetOverrides records the run's flag overrides so they can be re-applied after
// every stored-config overlay. Flags are the operator's explicit invocation-time
// statement; a ratified document outranks the FILE but not the command line.
func (d *Driver) SetOverrides(o config.Overrides) { d.overrides = o }

// targetCfg is the config one drain reads: the target's own effective config
// when a stored document is in force, else the driver's base — which is exactly
// yesterday's behaviour for every target that never had one.
func (d *Driver) targetCfg(t Target) *config.Config {
	if t.Cfg != nil {
		return t.Cfg
	}
	return d.cfg
}

// checkRunConfigVersion applies the boundary rule for one polled target: a
// version other than the one the current effective config was built from (or
// any version at all, before the first fetch) triggers a refetch-and-overlay.
// Called only from the poll's default case — the iteration boundary.
func (d *Driver) checkRunConfigVersion(ctx context.Context, i int, t *Target, version int) {
	if t.RCfg == nil || version == d.rcVersions[i] {
		return
	}
	// Permanent nothing-stored (CLA-481): version 0 IS the plane's "never set"
	// answer, so when nothing is in force (unfetched or already at none) the
	// answer is known without a fetch. Latch silently — staying at none is not
	// news — instead of fetching every boundary only to learn ErrNoConfig.
	// A 0 arriving while a stored overlay is in force (rcVersions > 0) still
	// fetches below, so the drop to local is confirmed and logged.
	if version == 0 && d.rcVersions[i] <= 0 {
		if d.targets[i].Cfg != nil {
			log.Printf("%srun-config: nothing stored on the plane - the local file rules", d.prefix(i))
			d.targets[i].Cfg = nil
		}
		d.rcVersions[i] = 0
		d.rcAttempt[i] = time.Time{}
		d.rcAttemptVer[i] = 0
		d.rcFails[i] = 0
		return
	}
	// Rate-limit RETRIES after a failed fetch of the SAME version to one per
	// backoff window, so a blip at ratify time retries without spamming the log.
	// A DIFFERENT version always fetches: the operator moved the document again,
	// and no throttle may delay that past the boundary it arrived on. The window
	// widens with consecutive failures (CLA-481), so a permanently unreachable
	// plane backs off instead of logging every 30s forever.
	if version == d.rcAttemptVer[i] && !time.Now().After(d.rcAttempt[i]) {
		return
	}
	st, err := t.RCfg.RunConfig(ctx)
	if err != nil {
		if errors.Is(err, plane.ErrNoConfig) || errors.Is(err, plane.ErrNotWired) {
			// Permanent: nothing to fetch, ever, for this version. ErrNotWired
			// is the slug-less / project-scoped-key shape (rcNotWired) — the
			// same "local file rules" posture as nothing stored, silent
			// thereafter, never retried. Say it once per transition, not every
			// poll — dropping from a stored doc to none is real news, staying
			// at none is not.
			if d.targets[i].Cfg != nil || d.rcVersions[i] > 0 {
				log.Printf("%srun-config: nothing stored on the plane - the local file rules", d.prefix(i))
				d.targets[i].Cfg = nil
			}
			d.rcVersions[i] = version
			d.rcAttempt[i] = time.Time{}
			d.rcAttemptVer[i] = 0
			d.rcFails[i] = 0
			return
		}
		if version != d.rcAttemptVer[i] {
			d.rcFails[i] = 1
		} else {
			d.rcFails[i]++
		}
		d.rcAttemptVer[i] = version
		wait := rcFetchBackoff(d.rcFails[i])
		d.rcAttempt[i] = time.Now().Add(wait)
		log.Printf("%srun-config: fetch failed (%v) - keeping %s; retrying in %s",
			d.prefix(i), err, d.cfgSourceDesc(i), wait)
		return // rcVersions unchanged, so the next poll retries (throttled above)
	}
	d.rcAttempt[i] = time.Time{}
	d.rcAttemptVer[i] = 0
	d.rcFails[i] = 0
	d.applyRunConfig(i, t, st)
}

// rcFetchBackoff is how long a failed refetch waits before trying the SAME
// version again: 30s, 60s, 120s, ..., capped at 5m. The first failure retries
// promptly (a blip at ratify time), a persistently unreachable plane backs off
// instead of spamming every boundary. A DIFFERENT version always fetches
// immediately — the operator moved the document, and no throttle may delay it.
func rcFetchBackoff(fails int) time.Duration {
	const (
		base = 30 * time.Second
		cap_ = 5 * time.Minute
	)
	if fails <= 1 {
		return base
	}
	d := base
	for i := 1; i < fails; i++ {
		d *= 2
		if d >= cap_ {
			return cap_
		}
	}
	return d
}

// cfgSourceDesc names what a target's sessions currently run under, for log
// lines that have to distinguish "the stored document" from "the local file".
func (d *Driver) cfgSourceDesc(i int) string {
	if d.targets[i].Cfg != nil {
		return "run-config overlay"
	}
	return "local rules"
}

// invalidateRunConfigs marks every wired target's stored-config state as
// unfetched again, so the next poll refetches and re-overlays on the CURRENT
// base. Called from applyReload: a RELOAD swaps d.cfg, and a target's existing
// overlay was built from the OLD base — its phases, prompts and any dial the
// stored document does not set are the pre-reload file's, and must not outlive
// it. The refetch lands on the poll immediately after the RELOAD marker was
// consumed, which is still an iteration boundary — never mid-session. The
// existing overlay is NOT dropped here: if that refetch fails, the previous
// config stays in force (the same posture as any failed fetch), and it
// self-heals on a later boundary.
func (d *Driver) invalidateRunConfigs() {
	for i := range d.targets {
		if d.targets[i].RCfg == nil {
			continue
		}
		d.rcVersions[i] = -1 // unfetched again: the next poll re-reads
		d.rcAttempt[i] = time.Time{}
		d.rcAttemptVer[i] = 0
		d.rcFails[i] = 0
	}
}

// applyRunConfig overlays a fetched document onto a clone of the base config
// and swaps it in as the target's effective config. Flag overrides re-apply on
// top (they outrank the document), then Validate referees: an overlaid combo
// the local rules refuse — a stored harness name nobody registered, a phase on
// an unconfigured harness — keeps the PREVIOUS config rather than half-running.
func (d *Driver) applyRunConfig(i int, t *Target, st *plane.RunConfigState) {
	var doc config.RunConfigDoc
	if err := json.Unmarshal(st.Config, &doc); err != nil {
		log.Printf("%srun-config v%d: undecodable document (%v) - keeping %s",
			d.prefix(i), st.Version, err, d.cfgSourceDesc(i))
		d.rcVersions[i] = st.Version // deterministic decode failure: don't hot-loop
		return
	}
	if doc.Empty() {
		// A stored-but-empty document consumes to nothing: same posture as
		// version 0, said once on the transition.
		if d.targets[i].Cfg != nil {
			log.Printf("%srun-config v%d: document sets nothing consumable - the local file rules", d.prefix(i), st.Version)
			d.targets[i].Cfg = nil
		}
		d.rcVersions[i] = st.Version
		return
	}
	eff := d.cfg.Clone()
	eff.ApplyRunConfig(&doc)
	if d.overrides != (config.Overrides{}) {
		eff.ApplyFlagOverrides(d.overrides)
	}
	if err := eff.Validate(); err != nil {
		log.Printf("%srun-config v%d: REFUSED locally (%v) - keeping %s; fix the stored document and ratify again",
			d.prefix(i), st.Version, err, d.cfgSourceDesc(i))
		d.rcVersions[i] = st.Version // deterministic refusal: don't hot-loop
		return
	}
	was := ""
	if d.rcVersions[i] > 0 {
		was = " (was v" + strconv.Itoa(d.rcVersions[i]) + ")"
	} else if d.rcVersions[i] == 0 {
		was = " (was: local)"
	}
	log.Printf("%srun-config v%d applied%s - harness/model/tiers/budget/escalation now follow the plane", d.prefix(i), st.Version, was)
	t.Cfg = eff
	d.rcVersions[i] = st.Version
}
