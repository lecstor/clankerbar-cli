package supervisor

// Handing a dead child's claim back (CLA-708) — the supervisor half of the
// CURRENT_CLAIM record the daemon maintains.
//
// The driver hands its own claims back: a no-WIP claim with `release`, a
// pushed-work claim with `expireLease`. Neither can happen when the DAEMON
// process dies (a crash, a kill, the machine going to sleep): the lease then
// ticks down for the full 30 minutes while the task sits with nobody working
// it. The supervisor is the only process left that knows a child is gone, so
// when a child exits it reads the record the daemon left in that child's state
// dir and makes the handback itself, before any respawn.
//
// Two properties make this safe:
//
//   - The key is the holding key. Children inherit CLANKERBAR_API_KEY from the
//     supervisor (materialize), so the plane's NOT-YOURS check passes.
//   - The plane refuses a claim whose run already ended or was taken over
//     (`not_holding`, `run_superseded`), and that refusal is a logged no-op.
//     Removing the file after a refusal is therefore safe: a stale record can
//     cost one refused call and never a second ended lease.

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// handbackTimeout bounds one handback. Same 20 seconds the driver gives its own
// release: the call races a lease that is already ticking, but a wedged plane
// must not hold up a respawn.
const handbackTimeout = 20 * time.Second

// handBackDeadClaim reads the exited child's CURRENT_CLAIM and hands it back.
// It always removes the file, whether the call succeeded, was refused, or could
// not be made: the record describes a process that is gone, and leaving it for
// the NEXT child's exit would hand back the wrong claim.
func (d *Supervisor) handBackDeadClaim(inst *Instance) {
	inst.mu.Lock()
	dir := inst.stateDir
	cfg := inst.cfg
	inst.mu.Unlock()
	if dir == "" {
		return
	}
	// A state dir that never existed (sleep-no-dir, a spawn that failed before
	// the dir was made) has nothing to read. statedir.Open would CREATE it, which
	// is not what an exit path should do.
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return
	}
	var roots []string
	if cfg != nil {
		roots = cfg.SessionWorkDirs()
	}
	st, err := statedir.Open(dir, roots...)
	if err != nil {
		log.Printf("%s: cannot read %s (%v) - its lease will expire instead", inst.name, loop.CurrentClaimName, err)
		return
	}
	defer st.Close() //nolint:errcheck // read-side handle on the way out
	claim, err := loop.ReadCurrentClaim(st)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("%s: %s is present but unreadable (%v) - its lease will expire instead", inst.name, loop.CurrentClaimName, err)
			d.removeCurrentClaim(inst, st)
		}
		return
	}
	// Remove after the attempt, not before: a crash between the plane call and
	// the removal repeats one refused call on the next exit, which is a no-op.
	defer d.removeCurrentClaim(inst, st)

	rel := d.planeFor(cfg, claim.Project)
	if rel == nil {
		log.Printf("%s: cannot hand %s back - no plane endpoint could be resolved; its lease will expire", inst.name, claimLabel(claim))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), handbackTimeout)
	defer cancel()

	branch := strings.TrimSpace(claim.Branch)
	action := "ended the lease on"
	if branch == "" {
		action = "released"
		// A no-branch task needs only the old `release` primitive; the driver
		// marker is the one new thing, and a plane without it still has release.
		err = rel.ReleaseDriver(ctx, claim.TaskID, claim.RunID)
		if errors.Is(err, plane.ErrFieldUnsupported) {
			err = rel.Release(ctx, claim.TaskID, claim.RunID)
		}
	} else {
		err = rel.ExpireLease(ctx, claim.TaskID, claim.RunID)
	}

	switch {
	case err == nil && branch == "":
		log.Printf("%s: %s %s back to the queue (the daemon exited still holding it)",
			inst.name, action, claimLabel(claim))
	case err == nil:
		log.Printf("%s: %s %s (pushed work kept for takeover; claimed %s)",
			inst.name, action, claimLabel(claim), claimAge(claim))
	case refusedClaimHandback(err):
		log.Printf("%s: nothing to hand back for %s - the plane says that run already ended (%v)",
			inst.name, claimLabel(claim), err)
	case errors.Is(err, plane.ErrFieldUnsupported):
		log.Printf("%s: the backlog plane predates expireLease - %s keeps its lease for the takeover hand-off; it will expire as before",
			inst.name, claimLabel(claim))
	case errors.Is(err, plane.ErrNotWired):
		log.Printf("%s: cannot hand %s back - no plane writes are configured; its lease will expire",
			inst.name, claimLabel(claim))
	default:
		log.Printf("%s: could not hand %s back: %v - its lease will expire instead",
			inst.name, claimLabel(claim), err)
	}
}

func (d *Supervisor) removeCurrentClaim(inst *Instance, st *statedir.Dir) {
	if err := st.Remove(loop.CurrentClaimName); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("%s: could not remove %s (%v)", inst.name, loop.CurrentClaimName, err)
	}
	// A crash mid-write leaves the temporary half; it is ours by name.
	_ = st.Remove(loop.CurrentClaimName + ".tmp")
}

// planeFor builds the write client for the project the claim belongs to. The
// child's own config is the authoritative source: it is the config the daemon
// ran on, so its endpoint is the one that claim was taken against. A
// multi-project config names each project's endpoint; a single-project one has
// only the backlog endpoint.
//
// The roster origin is the fallback for a config whose endpoint cannot be
// derived, which is also what makes the supervisor's tests able to point the
// whole fleet at one httptest server.
func (d *Supervisor) planeFor(cfg *config.Config, project string) plane.Releaser {
	endpoint := ""
	if cfg != nil {
		if project != "" {
			for _, p := range cfg.Projects {
				if strings.EqualFold(p.Slug, project) {
					endpoint = cfg.ProjectEndpoint(p)
					break
				}
			}
		}
		if endpoint == "" {
			endpoint = cfg.BacklogEndpoint()
		}
	}
	if endpoint == "" {
		endpoint = mcpEndpointFromRoster(d.o.RosterURL, project)
	}
	if endpoint == "" || d.o.APIKey == "" {
		return nil
	}
	if d.o.PlaneFor != nil {
		return d.o.PlaneFor(endpoint, d.o.APIKey)
	}
	return plane.New(endpoint, d.o.APIKey)
}

// mcpEndpointFromRoster derives an MCP endpoint from the roster URL: the same
// origin, the `/mcp/<project>` path the plane serves. It is a fallback, not the
// usual path — a child's config normally carries the endpoint it ran against.
func mcpEndpointFromRoster(rosterURL, project string) string {
	u, err := url.Parse(rosterURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.Path = path.Join("/mcp", project)
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u.String()
}

// refusedClaimHandback reports the two refusals that mean "there is nothing to
// do here": the run already ended, or a later run holds the task. Both are
// normal after a child exit — the daemon may have settled the task and died
// before removing its record — so they are logged and not treated as errors.
func refusedClaimHandback(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	msg = strings.NewReplacer(" ", "_", "-", "_").Replace(msg)
	return strings.Contains(msg, "not_holding") || strings.Contains(msg, "run_superseded")
}

func claimLabel(c *loop.CurrentClaim) string {
	// The record has no ref: the task id is what the plane accepts and what the
	// supervisor can print.
	return c.TaskID
}

func claimAge(c *loop.CurrentClaim) string {
	if c.ClaimedAt.IsZero() {
		return "unknown"
	}
	return time.Since(c.ClaimedAt).Round(time.Second).String() + " ago"
}
