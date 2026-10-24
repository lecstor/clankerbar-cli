package cli

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
	"github.com/lecstor/clankerbar-cli/internal/supervisor"
)

// --- 5. state dir ------------------------------------------------------------

// checkStateDir covers the one directory the DRIVER itself writes: iteration
// logs and the stop/pause markers. The directories the SESSIONS run in are a
// separate, per-project concern — see checkSessions.
func checkStateDir(cfg *config.Config) check {
	c := check{name: "state_dir"}
	stateDir, err := cfg.ResolveStateDir()
	if err != nil {
		c.status = fail
		c.detail = "cannot resolve the state dir: " + err.Error()
		c.remedy = "set state_dir explicitly, or XDG_STATE_HOME"
		return c
	}

	// Open it exactly as the loop will, so the preflight exercises the real
	// creation, the real mode tightening, the real symlink refusal and the real
	// .gitignore — not an approximation that could pass where the loop fails.
	dir, err := statedir.Open(stateDir, cfg.SessionWorkDirs()...)
	if err != nil {
		c.status = fail
		c.detail = err.Error()
		// Deliberately generic: Open refuses for several distinct reasons — mode,
		// ownership, a symlink on the path, a directory that is somebody else's —
		// and each of those errors already names the path and the way out. A
		// specific remedy here would contradict most of them.
		c.remedy = "point state_dir at a directory you own that is empty or already a clankerbar state dir; the message above says which part is wrong"
		return c
	}
	defer dir.Close()

	// WHERE the state dir sits is a capability question, not a tidiness one: STOP
	// and HALT live there, and a session may write anywhere under the workdir it is
	// spawned in. A state dir inside one hands every spawned session the daemon's
	// own stop switch. CLA-259 moved the DEFAULT out; an explicit state_dir still
	// wins, including one pointed back inside, so the operators this catches are
	// exactly the ones the new default never reached.
	inside := containingWorkDir(stateDir, cfg)

	// Creatable is not writable: an already-existing state dir can be read-only,
	// and the loop writes a log per iteration. Prove it with a real write, under
	// a name nobody could have got to first — a fixed probe name in a directory
	// somebody else can write is a path they can point at a file of their
	// choosing, and this used to truncate whatever it found there (CLA-259).
	probe := ".doctor-write-probe-" + randomTail()
	if err := dir.WriteFile(probe, []byte("ok")); err != nil {
		c.status = fail
		c.detail = stateDir + " is not writable: " + err.Error()
		c.remedy = "fix its permissions, or point state_dir somewhere writable"
		return c
	}
	_ = dir.Remove(probe)

	// A leftover marker makes the next start act on it straight away — the
	// failure that looks exactly like "the backlog was empty" (STOP/HALT), or
	// its CLA-461 siblings: a surprise re-exec or config reload on the first
	// tick. Same class, different costs, so each marker names its own.
	type controlMarker struct {
		name string
		cost string
	}
	markers := []controlMarker{
		{"HALT", "the loop halts on its first tick and stays halted"},
		{"STOP", "the loop stops on its first tick"},
		{loop.MarkerRestart, "the daemon re-executes on its first tick"},
		{loop.MarkerRestartNow, "the daemon kills its in-flight session and re-executes"},
		{loop.MarkerReload, "the config file is re-read on its first tick"},
	}
	var found []string
	var costs []string
	for _, m := range markers {
		if dir.Exists(m.name) {
			found = append(found, m.name)
			costs = append(costs, m.name+" — "+m.cost)
		}
	}
	if len(found) > 0 {
		c.status = warn
		c.detail = stateDir + " has a leftover " + strings.Join(found, " and ") + " marker"
		c.remedy = "delete it, or the loop acts on it immediately on start: rm " + filepath.Join(stateDir, found[0])
		for _, cost := range costs {
			c.info = append(c.info, cost)
		}
		if inside.session {
			// Otherwise the operator deletes the marker, runs again, and a session
			// writes it back - the remedy above is a symptom's remedy when the state
			// dir is somewhere the sessions can reach. Only for a SESSION workdir: a
			// configured-but-unused one is not somewhere a marker can have come from.
			c.info = append(c.info, "it is inside the session workdir "+inside.dir+", so a session could have written that marker itself")
		}
		return c
	}

	// Reported ahead of the legacy leftover below, which is the smaller problem: a
	// leftover directory is only ever READ from, while this is a live write
	// capability. WARN, not FAIL - an explicit state_dir is supported and the run
	// still makes progress, and `doctor && run` must not be gated on a setup that
	// works.
	if inside.dir != "" {
		c.status = warn
		c.detail, c.remedy = stateDirInWorkDir(stateDir, inside, cfg)
		// The legacy report below is unreachable once we return, and it is a
		// separate fact about a separate directory - carry it rather than lose it.
		if legacy := cfg.LegacyStateDir(); legacy != "" {
			c.info = append(c.info, legacy+" is also a leftover from before the state dir moved out of the workdir; markers there are ignored now")
		}
		// Same for the implicit-workdir note the PASS line carries below: with no
		// workdir configured, `workdir` is the one knob that fixes both this and the
		// state dir moving with whatever directory doctor was run from.
		if cfg.WorkDirIsImplicit() {
			c.info = append(c.info, "workdir is not configured - it was derived from "+cfg.WorkDir+", so set workdir too, or the state dir moves with the directory you run from")
		}
		return c
	}

	// The state dir moved out of the workdir in CLA-259. A leftover one in the old
	// place is worth a WARN on both counts an operator cares about: markers they
	// touch there do nothing, and the transcripts already in it are sitting inside
	// a repo an unattended agent commits from.
	if legacy := cfg.LegacyStateDir(); legacy != "" {
		c.status = warn
		c.detail = stateDir + " writable; " + legacy + " is a leftover from before the state dir moved out of the workdir"
		c.remedy = "markers there are ignored now — move anything you want out of it, then: rm -rf " + legacy
		return c
	}

	c.status = pass
	c.detail = stateDir + " writable, no stop markers"
	// Where the two logs live, so `doctor` answers "where do I look?" without an
	// operator reading the source (CLA-701). The daemon's own lines go to
	// daemon.log beside the iteration logs; the supervisor keeps its own under
	// its state dir, and is only named when one has run on this machine.
	c.info = append(c.info, "daemon log: "+filepath.Join(stateDir, statedir.DaemonLogName)+" (size-rotated; iteration logs in the same directory)")
	if sup, err := supervisor.CacheDir(); err == nil {
		if _, statErr := os.Stat(sup); statErr == nil {
			c.info = append(c.info, "supervisor log: "+filepath.Join(sup, supervisor.SuperviseLogName))
		}
	}
	// The state dir's name is a hash of the workdir, so when the workdir itself
	// came from the cwd, this line is about a directory that MOVES with where the
	// operator happened to be standing: doctor from ~ and `run` from the checkout
	// then talk about two different directories, and doctor's "no stop markers" is
	// true of one the loop never opens. Say which directory it resolved from.
	if cfg.WorkDirIsImplicit() {
		c.detail += " (workdir not configured - derived from " + cfg.WorkDir + ")"
		c.remedy = "set workdir so the state dir does not move with the directory you run from"
	}
	return c
}

// workDirMatch is the configured workdir a state dir was found at or under. dir
// is "" when it is outside every one of them.
//
// session says whether the loop actually SPAWNS sessions in that directory, and
// the two cases cost different things - see stateDirInWorkDir. Reporting the
// wrong one is not a wording detail: telling an operator a session can write
// their state dir when no session runs there is a claim they can check and
// disprove, which is how a warning gets skimmed past.
// outer is the OUTERMOST containing workdir, which is what a remedy has to name:
// every match is an ancestor of the same state dir, so they form a chain, and
// "point it outside ~/dev/acme" is satisfied by ~/dev/state - which warns again.
type workDirMatch struct {
	dir     string
	session bool
	outer   string
}

// containingWorkDir finds the configured workdir the state dir sits at or under.
//
// EVERY configured workdir is considered, not just the ones sessions run in: the
// top-level `workdir` and each project's (resolved with the same fallback the
// loop uses). A `projects[]` entry that omits `workdir` inherits the top-level
// one, so a state dir inside it is a capability waiting for the next config edit
// even when nothing is spawned there today.
//
// A session workdir wins over a merely-configured one, and the DEEPEST match wins
// within each group: for `workdir: ~/dev` with `projects[0].workdir: ~/dev/acme`,
// the answer an operator can act on is ~/dev/acme, the directory their sessions
// are really in.
//
// Both a lexical and a symlink-resolved comparison are made, and either one
// counts. The lexical pass is what an operator can check against their own config
// by eye; the resolved pass catches the case where two different-looking paths are
// the same directory - /var vs /private/var on macOS, or a workdir reached through
// a symlinked parent. Resolution is best-effort: an unresolvable path falls back
// to its lexical form rather than dropping the comparison. Both passes are
// CASE-SENSITIVE, so on a case-insensitive filesystem `~/Dev` against `~/dev` is a
// known miss - the same lexical assumption config and statedir already make, and
// a miss here costs a warning rather than a guarantee.
func containingWorkDir(stateDir string, cfg *config.Config) workDirMatch {
	sessions := make(map[string]bool)
	for _, d := range cfg.SessionWorkDirs() {
		sessions[d] = true
	}

	var best, bestConfigured, outer string
	for _, wd := range configuredWorkDirs(cfg) {
		if !pathWithin(stateDir, wd) && !pathWithin(resolvePath(stateDir), resolvePath(wd)) {
			continue
		}
		// Longest path == deepest directory, since every candidate is absolute and
		// cleaned and they are all ancestors of the same state dir.
		if sessions[wd] {
			if len(wd) > len(best) {
				best = wd
			}
		} else if len(wd) > len(bestConfigured) {
			bestConfigured = wd
		}
		if outer == "" || len(wd) < len(outer) {
			outer = wd
		}
	}
	if best != "" {
		return workDirMatch{dir: best, session: true, outer: outer}
	}
	return workDirMatch{dir: bestConfigured, outer: outer}
}

// stateDirInWorkDir words the in-workdir warning. Two populations, because they
// are owed different sentences: a SESSION workdir is a live capability, while a
// configured-but-unused one is a trap set for whoever next adds a `projects[]`
// entry without a workdir of its own.
//
// The remedy is the half that has to survive being FOLLOWED, so it is checked
// against the config rather than assumed. "Remove state_dir" is only offered when
// the default would actually land outside - a workdir of ~ contains
// ~/.local/state, so for those operators removing it changes the path and not the
// warning - and it names the OUTERMOST containing workdir, since moving just
// outside the innermost one lands in the next one out.
func stateDirInWorkDir(stateDir string, m workDirMatch, cfg *config.Config) (detail, remedy string) {
	if m.session {
		detail = stateDir + " is inside the session workdir " + m.dir + ": a session spawned there can write the loop's own STOP/HALT markers"
	} else {
		detail = stateDir + " is inside the configured workdir " + m.dir + ": no session runs there today, but a projects[] entry that omits workdir inherits it, and every session it spawns could then write the loop's STOP/HALT markers"
	}

	switch {
	case cfg.StateDir != "" && defaultStateDirIsOutside(cfg):
		remedy = "remove state_dir to take the default outside the workdir, or point it somewhere outside " + m.outer
	case cfg.StateDir != "":
		remedy = "point state_dir somewhere outside " + m.outer + " - removing it will not help, the default lands inside that workdir too"
	default:
		remedy = "set state_dir to a directory outside " + m.outer + " - the default lives under your home, which is inside the workdir here"
	}
	return detail, remedy
}

// defaultStateDirIsOutside reports whether dropping an explicit state_dir would
// actually move the state dir out of every workdir. Answering it by resolving the
// default is the only honest way: the default is under the state home, which an
// outer-enough workdir (~, or the cwd a workdir was derived from) contains.
func defaultStateDirIsOutside(cfg *config.Config) bool {
	def := *cfg
	def.StateDir = ""
	dir, err := def.ResolveStateDir()
	if err != nil {
		return false // cannot say it is outside, so do not advise it
	}
	return containingWorkDir(dir, cfg).dir == ""
}

// configuredWorkDirs is every directory this config names as a workdir, absolute,
// cleaned and de-duplicated.
//
// An empty value is resolved rather than skipped, matching config.absWorkDir and
// config.SessionWorkDirs: an unset workdir means the cwd everywhere else in the
// tool, and skipping it here would make the containment check silently answer
// about fewer directories than the loop uses.
func configuredWorkDirs(cfg *config.Config) []string {
	dirs := []string{cfg.WorkDir}
	for _, p := range cfg.Projects {
		dirs = append(dirs, projectWorkDir(cfg, p))
	}

	out := make([]string, 0, len(dirs))
	seen := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

// pathWithin reports whether path is dir itself or sits under it, COMPONENT-WISE.
// A string prefix would say yes to /a/workdir-2 against /a/workdir, which is a
// different directory a session never touches - the false positive that would
// teach operators to skim past this warning.
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false // different volumes, or one is relative: not containment
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolvePath is EvalSymlinks that never fails: an unresolvable path (it does not
// exist yet, or a component is not readable) comes back as it went in, so the
// caller compares something rather than nothing.
func resolvePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// randomTail is an unguessable filename suffix. See loop.randomTail — the same
// reasoning, for the probe this check writes.
func randomTail() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
