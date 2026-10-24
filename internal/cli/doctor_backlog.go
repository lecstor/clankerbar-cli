package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
)

// --- 4. backlog wiring -------------------------------------------------------

// checkBacklog verifies the driver's cheap control-plane read. A multi-project
// instance gets ONE CHECK PER PROJECT: each has its own slug-ful route and its
// own .mcp.json, so one of them can be wired wrong while the others are fine —
// and a single aggregate line would hide exactly that.
func checkBacklog(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	if len(cfg.Projects) == 0 {
		return []check{backlogCheck(ctx, "backlog", cfg.BacklogSummaryURL(), e)}
	}
	out := make([]check, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		out = append(out, backlogCheck(ctx, "backlog["+p.Slug+"]", cfg.ProjectSummaryURL(p), e))
	}
	return out
}

// checkFleets reports whether fleet activity reporting (CLA-466) is wired and
// its endpoint reachable — the same PASS/WARN table shape as the MCP wiring
// checks (the CLA-448 precedent). One check per project, like backlog's.
//
// Every outcome is PASS or WARN, never FAIL: reporting is telemetry, so an
// outage or a gap degrades console visibility while the loop runs exactly as
// before. A rejected key already FAILs in the backlog check above it, where it
// actually stops the run; saying it twice here at FAIL strength would make
// doctor exit non-zero over a condition that costs nothing but a picture.
func checkFleets(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	if len(cfg.Projects) == 0 {
		return []check{fleetCheck(ctx, "fleet", cfg.FleetReportURL(), e)}
	}
	out := make([]check, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		out = append(out, fleetCheck(ctx, "fleet["+p.Slug+"]", cfg.ProjectFleetReportURL(p), e))
	}
	return out
}

func fleetCheck(ctx context.Context, name, endpoint string, e doctorEnv) check {
	c := check{name: name}

	// Not-wired cases first: no key, no derivable slug-ful report URL. Both are
	// the "reporting off, loop fine" shape.
	if e.apiKey == "" {
		c.status = warn
		c.detail = "fleet activity reporting is off: no creds — CLANKERBAR_API_KEY is unset"
		c.remedy = "export CLANKERBAR_API_KEY (mint one at clankerbar.com/projects/<slug>/api-keys)"
		return c
	}
	if endpoint == "" {
		c.status = warn
		c.detail = "fleet activity reporting is off: no project-scoped /api/projects/<slug>/fleet/report endpoint could be derived"
		c.remedy = "declare projects[].slug, or point mcp_config_path at an .mcp.json naming /mcp/<slug>"
		return c
	}

	// The probe POSTs an EMPTY body: auth is checked before body validation, so a
	// 400 proves reachable + routed + key accepted without writing anything. A
	// real beacon would upsert this machine's presence row with doctor's own
	// state, fighting whatever a live daemon on the same host reports.
	err := e.fleetProbe(ctx, endpoint, e.apiKey)
	switch {
	case err == nil:
		c.status = pass
		c.detail = fmt.Sprintf("%s — reachable, key accepted; presence updates on each poll", endpoint)
	case timedOut(err):
		c.status = warn
		c.detail = "fleet endpoint timed out — reports would be dropped (the loop is unaffected): " + endpoint
		c.remedy = "check the plane is reachable"
	default:
		var pe *fleet.ProbeError
		switch {
		case errors.As(err, &pe) && (pe.Status == http.StatusUnauthorized || pe.Status == http.StatusForbidden):
			c.status = warn
			c.detail = fmt.Sprintf("fleet endpoint reachable but the key was rejected (HTTP %d) — reporting is off: %s", pe.Status, endpoint)
			c.remedy = "check CLANKERBAR_API_KEY covers this project (a wrong project's key is refused before any write)"
		case errors.As(err, &pe) && pe.Status == http.StatusNotFound:
			c.status = warn
			c.detail = "the plane has no fleet-report route (an older clankerbar deployment) — reporting is off"
			c.remedy = "upgrade clankerbar to a build with fleet activity (CLA-465); the CLI runs fine without it"
		case errors.As(err, &pe):
			c.status = warn
			c.detail = fmt.Sprintf("fleet endpoint answered unexpectedly (HTTP %d) — reporting may be degraded: %s", pe.Status, endpoint)
			c.remedy = "check the plane is reachable: " + endpoint
		default:
			c.status = warn
			c.detail = "fleet endpoint unreachable — reporting is off (the loop is unaffected): " + err.Error()
			c.remedy = "check the plane is reachable: " + endpoint
		}
	}
	return c
}

// timedOut reports whether err is a timeout, however the HTTP client chose to
// express it: a context deadline or the Client.Timeout wrapper (a net.Error
// with Timeout set), which are not the same error chain.
func timedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// --- fleet identity -----------------------------------------------------------

// checkInstanceIdentity warns when two config files in THIS config's directory
// would resolve to the same Fleet-page instance identity (CLA-501).
//
// The collision is invisible from inside one daemon  -  every daemon believes its
// own name is fine  -  so the check has to look sideways at its siblings: each
// top-level *.json in the config's directory is read for instance_name and
// resolved through the SAME function the loop beacons under
// (config.ResolveInstanceName), and any identity claimed by two or more files
// warns. Files that do not parse as JSON are skipped: a file the loader cannot
// read cannot start a daemon, so it cannot beacon anything to collide with.
//
// Since CLA-501 the default identity embeds the config basename, so distinct
// filenames cannot collide; what remains is two siblings declaring the same
// explicit instance_name (or a truncation-induced prefix clash)  -  exactly the
// silent overwrite this check exists to catch before an overnight run.
func checkInstanceIdentity(cfg *config.Config) check {
	c := check{name: "instance identity"}
	host, err := os.Hostname()
	if err != nil {
		// The loop logs the same failure and beacons a blank host half; here a
		// blank simply resolves against basenames alone.
		host = ""
	}
	resolved := cfg.ResolvedInstanceName(host)
	c.info = append(c.info, "identity: "+resolved)

	src := cfg.Source()
	if src == "" {
		c.detail = "no config file in play; nothing to scan for sibling collisions"
		return c
	}
	dir := filepath.Dir(src)
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		c.detail = "could not list " + dir + ": " + err.Error()
		return c
	}

	type named struct {
		InstanceName string `json:"instance_name"`
	}
	byIdentity := map[string][]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue // unreadable sibling: cannot run, cannot collide; say nothing
		}
		var n named
		if err := json.Unmarshal(data, &n); err != nil {
			continue // not a clankerbar config (or unparseable): see above
		}
		id := config.ResolveInstanceName(n.InstanceName, f, host)
		byIdentity[id] = append(byIdentity[id], filepath.Base(f))
	}

	var collided []string
	for id, names := range byIdentity {
		if len(names) > 1 {
			sort.Strings(names)
			collided = append(collided, id+" <- "+strings.Join(names, ", "))
		}
	}
	if len(collided) == 0 {
		c.detail = fmt.Sprintf("no identity collisions among %d config file(s) in %s", len(files), dir)
		return c
	}
	sort.Strings(collided)
	c.status = warn
	c.detail = fmt.Sprintf("%d config file(s) in %s would beacon under the SAME fleet identity; their presence rows overwrite each other: %s",
		len(files), dir, strings.Join(collided, "; "))
	c.remedy = "give each co-located daemon a distinct config filename, or set a distinct instance_name in each"
	return c
}

func backlogCheck(ctx context.Context, name, summaryURL string, e doctorEnv) check {
	c := check{name: name}

	// The four not-wired cases are kept distinct on purpose. "Blind" (no creds, no
	// endpoint, unreachable) still makes progress, so it WARNs; a rejected key or a
	// key/route mismatch will never self-heal and burns doomed sessions, so it FAILs.
	if e.apiKey == "" {
		c.status = warn
		c.detail = "loop would run blind: no creds — CLANKERBAR_API_KEY is unset"
		c.remedy = "export CLANKERBAR_API_KEY (mint one at clankerbar.com/projects/<slug>/api-keys)"
		return c
	}
	if summaryURL == "" {
		c.status = warn
		c.detail = "loop would run blind: no summary endpoint could be derived"
		c.remedy = "declare projects[].slug, or point mcp_config_path at an .mcp.json naming /mcp/<slug>"
		return c
	}

	sum, err := e.newPoller(summaryURL, e.apiKey).Poll(ctx)
	switch {
	case err == nil:
		c.status = pass
		c.detail = fmt.Sprintf("%s — %d claimable, %d open question(s)", summaryURL, sum.Claimable, sum.OpenQuestions)
		// Named only when there is some, and named separately from `claimable`. The
		// warning below already reads through Spawnable() and so goes quiet on its
		// own, but a PASS reading "0 claimable" in front of an operator whose loop is
		// about to start spawning is the same drift one line earlier: they would read
		// it as an idle night (CLA-274).
		if sum.StaleClaimable > 0 {
			c.detail += fmt.Sprintf(", %d abandoned to recover", sum.StaleClaimable)
		}
		// A paused queue is the most certain no-op there is: the driver polls all
		// night and spawns nothing. That is precisely the "the window produced
		// nothing and I did not know" class doctor exists for, so it WARNs rather
		// than decorating a PASS with a footnote the operator skims past.
		if sum.Paused {
			c.status = warn
			c.detail += " — paused from the console; the loop will not spawn sessions"
			c.remedy = "resume the loop at clankerbar.com, or expect an idle run"
			return c
		}
		// Nothing claimable is not itself a problem — the loop idle-polls for free
		// rather than spawning. It IS worth saying out loud when the operator holds
		// the only key, because "no work" and "work you have not unblocked" look
		// identical in the counts and read as a healthy queue.
		if !sum.Spawnable() && sum.OpenQuestions > 0 {
			c.status = warn
			c.detail += " — nothing to claim; the loop will idle without spawning"
			c.remedy = "answer the open question(s) at clankerbar.com, or expect an idle run"
		}
	case errors.Is(err, backlog.ErrUnauthorized):
		c.status = fail
		c.detail = "key rejected (401/403) by " + summaryURL
		c.remedy = "CLANKERBAR_API_KEY is revoked, wrong or malformed — mint a fresh key; the harness sessions carry this same key"
	case errors.Is(err, backlog.ErrProjectRequired):
		// The remedy follows the 2026-07-29 decision: the loop runs on the ACCOUNT
		// key, so the fix is a project SELECTOR, not a different key.
		c.status = fail
		c.detail = "key/route mismatch: 400 project_required from the slug-less route"
		c.remedy = "declare the project slug(s) so the driver polls /api/projects/<slug>/backlog-summary (a project-scoped key also works, CI-style)"
	case errors.Is(err, backlog.ErrNotWired):
		c.status = warn
		c.detail = "loop would run blind: backlog polling is not wired"
		c.remedy = "set both a derivable project endpoint and CLANKERBAR_API_KEY so the loop can gate on live counts"
	default:
		c.status = warn
		c.detail = "loop would run blind: endpoint unreachable — " + err.Error()
		c.remedy = "check the plane is reachable: " + summaryURL
	}
	return c
}
