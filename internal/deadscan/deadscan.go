// Package deadscan is the retrospective half of CLA-402: a read-only scan over
// the daemon's iteration logs that reports, per day, per phase and per harness,
// how many sessions ran and how many of those died producing nothing.
//
// It exists because the dead-phase rate is the number that decides whether a
// fix to the opencode gateway worked, and before CLA-402 the only way to get
// it was a hand scan of the same logs — CLA-396 counted 6 dead of 16 opencode
// implement sessions on 2026-08-20 by hand, in the middle of an investigation.
// This package makes that number reproducible from the logs alone.
//
// The classification is the driver's OWN predicate, not an approximation of it:
//
//   - a session "got past its claim" when the log shows a completed
//     claim_task, or a completed heartbeat (a resumed phase holds a claim
//     without ever calling claim_task). A REFUSED claim (status "error", e.g.
//     lease_live on a takeover) is the 2026-08-20 06:32 false positive — a
//     correct refusal that satisfies both conjuncts of deadPhase() — and counts
//     toward NEITHER counter (operator decision f518a454).
//   - a session is dead when it got past its claim, its final step_finish
//     carried reason "unknown" (opencode's marker for a session that died
//     without a final answer), and NO branch was recorded on the task — the
//     "produced nothing" shape. A session that recorded a branch and THEN died
//     still produced something, so it is not dead.
//   - a session whose log carries the adapter's own cap/failure markers (a raw
//     "!! session outlived its wall-clock cap" / "!! session crossed its
//     per-session token ceiling" / "!! stream read failed" console line) ended
//     on an orderly backstop, not a silent death, mirroring the driver's
//     `!capped && !ceiling && !wallclock && !untrusted` conjuncts.
//
// The same rule is applied by the live tally in internal/loop (deadtally.go),
// so the historical rate and the live rate measure the same thing.
//
// # The known-positive control
//
// The doneWhen requires the scan to be verified against known matches before
// its output is trusted, and names two: the recorded 2026-08-20 figure (6 dead
// of 16 opencode implement sessions as of mid-day) and the three 2026-08-19
// logs carrying `tool_count_limit` as an APIError EVENT. The second is a trap
// in disguise: the string also appears in 2026-08-20 logs as task-body TEXT an
// agent read, never as an error event — so only a scan that matches structured
// error events finds exactly three, while a text search finds many. FindErrors
// is that structured search.
package deadscan

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Log is one iteration log's classification. The fields mirror the driver's
// Result/Claim vocabulary so the scan and the live tally read alike.
type Log struct {
	Path string

	Day     string // "2026-08-20"
	Phase   string // the -p<phase> filename tag; "unphased" when absent
	Harness string // inferred from content: opencode / claude / codex / opencode2

	// GotPastClaim reports whether the session got past its claim: a completed
	// claim_task, or a completed heartbeat on a resumed phase. A refused claim
	// leaves this false, and the session counts toward neither counter.
	GotPastClaim bool
	// BranchRecorded mirrors Claim.HasWIP: an update_task carrying a branch, or
	// a claim result that already carried WIP. A session that recorded a branch
	// before dying is not dead.
	BranchRecorded bool
	// LastReason is the final step_finish part's reason, as the driver reads it
	// off FinishReasonKey. Empty for harnesses that emit no step_finish (claude,
	// codex, opencode2) — and therefore never dead.
	LastReason string

	// Dead is the classification: got past its claim, reason "unknown", no
	// branch, no cap marker.
	Dead bool

	// APIErrors are the structured error events this log carried (name + data),
	// for the FindErrors known-positive control. Text an agent merely READ is
	// not an error event and never lands here.
	APIErrors []string
}

// Cell is one (day, phase, harness) row of the report.
type Cell struct {
	Day, Phase, Harness string
	Run, Dead           int
}

// Rate is dead/run as a percentage, 0 when nothing ran.
func (c Cell) Rate() float64 {
	if c.Run == 0 {
		return 0
	}
	return 100 * float64(c.Dead) / float64(c.Run)
}

// Scan walks root (the loop state root: the directory whose subdirectories are
// per-workdir slugs, each holding iteration-*.log) and classifies every log.
//
// The walk is done with fs.WalkDir so symlinked components cannot silently
// empty the scan the way BSD grep -r's refusal to follow symlinks emptied the
// 2026-08-20 investigation (a component of the state path is a symlink on the
// operating machine). The known-positive control in deadscan_test.go pins the
// behaviour: a corpus known to contain matches must yield them.
func Scan(root string) ([]Log, error) {
	var logs []Log
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasPrefix(d.Name(), "iteration-") || !strings.HasSuffix(d.Name(), ".log") {
			return nil
		}
		l, err := classify(path)
		if err != nil {
			return fmt.Errorf("deadscan: %s: %w", path, err)
		}
		logs = append(logs, l)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("deadscan: walk %s: %w", root, err)
	}
	return logs, nil
}

// Summarize aggregates classified logs into per-day/phase/harness cells, sorted
// by day then phase then harness. Sessions that never got past their claim are
// in NO cell's run count — the operator ruling (f518a454) excludes them from
// both counters, so an inflated numerator is impossible by construction.
func Summarize(logs []Log) []Cell {
	idx := map[string]int{}
	var cells []Cell
	cell := func(l Log) *Cell {
		k := l.Day + "\x00" + l.Phase + "\x00" + l.Harness
		if i, ok := idx[k]; ok {
			return &cells[i]
		}
		idx[k] = len(cells)
		cells = append(cells, Cell{Day: l.Day, Phase: l.Phase, Harness: l.Harness})
		return &cells[len(cells)-1]
	}
	for _, l := range logs {
		if !l.GotPastClaim {
			continue
		}
		c := cell(l)
		c.Run++
		if l.Dead {
			c.Dead++
		}
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Day != cells[j].Day {
			return cells[i].Day < cells[j].Day
		}
		if cells[i].Phase != cells[j].Phase {
			return cells[i].Phase < cells[j].Phase
		}
		return cells[i].Harness < cells[j].Harness
	})
	return cells
}

// FindErrors returns the logs carrying at least one structured APIError event
// whose message contains text. This is the known-positive control for the scan:
// it must match the three genuine 2026-08-19 tool_count_limit APIError logs and
// NOT the 2026-08-20 logs where the same string appears as task-body text an
// agent read — those are tool outputs, never error events.
func FindErrors(logs []Log, text string) []Log {
	var out []Log
	for _, l := range logs {
		for _, e := range l.APIErrors {
			if strings.Contains(e, text) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// --- log parsing -----------------------------------------------------------

// iterName matches an iteration log's filename and pulls out the day and the
// phase tag. Shapes observed in the wild:
//
//	iteration-20260820-004335-d3-pimplement-a0-5ff482cb.log   (phased)
//	iteration-20260819-200641-d1-pimplement-h1-a0-c78fd0d4.log (handoff respawn)
//	iteration-20260809-202123-d1-a0-2c107e2a.log              (unphased)
//
// The `-h<N>` suffix marks a handoff respawn of the SAME phase (the driver
// appends it to the tag), so the phase captured is the phase before it — a
// respawn is still an implement session.
var iterName = regexp.MustCompile(`^iteration-(\d{8})-\d{6}-d\d+(?:-p([A-Za-z0-9_-]+?)(?:-h\d+)?)?-a\d+-[0-9a-f]+\.log$`)

// classify reads one iteration log and applies the dead-phase predicate.
func classify(path string) (Log, error) {
	l := Log{Path: path}
	base := filepath.Base(path)
	if m := iterName.FindStringSubmatch(base); m != nil {
		t, err := time.Parse("20060102", m[1])
		if err == nil {
			l.Day = t.Format("2006-01-02")
		}
		if m[2] != "" {
			l.Phase = m[2]
		} else {
			l.Phase = "unphased"
		}
	} else {
		// Not a name the driver writes — report it rather than guess.
		return l, fmt.Errorf("unrecognised iteration log name %q", base)
	}

	data, err := readFile(path)
	if err != nil {
		return l, err
	}
	l.Harness = harnessOf(data)
	l.GotPastClaim, l.BranchRecorded, l.LastReason, l.APIErrors, l.Dead =
		classifyEvents(l.Harness, data)
	return l, nil
}

// harnessOf classifies a log's harness from its content: opencode emits JSON
// step events (step_start/step_finish/tool_use); codex emits stream/turn JSON;
// opencode2 emits only text-part JSON; claude emits rendered text.
func harnessOf(data []byte) string {
	for _, line := range splitLines(data) {
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "step_start", "step_finish", "tool_use":
			return "opencode"
		case "stream_start", "turn.completed", "exec_result":
			return "codex"
		case "text":
			return "opencode2"
		}
	}
	return "claude"
}

// classifyEvents walks the log's events and applies the predicate. It also
// detects the adapter's own raw cap/failure console lines, which the live
// driver reads as !capped && !ceiling && !wallclock && !untrusted.
//
// harness is passed in because the CLAUDE surface is rendered text, not JSON:
// its claim observation shows as a "· holding" console line (noteClaimed) and a
// resumed phase's heartbeat as a "→ mcp__clankerbar__heartbeat" marker, and
// without reading those a claude session that ran would count toward neither
// counter. Codex and opencode2 emit no claim state the scan can read, so their
// sessions count as run only when a claim marker appears; they can never be
// dead either way (no step_finish reason on either surface).
func classifyEvents(harness string, data []byte) (gotPastClaim, branchRecorded bool, lastReason string, apiErrors []string, dead bool) {
	// The driver's dead conjuncts, mirrored from the log:
	//   reason "unknown" AND no branch AND got past its claim AND not a cap.
	reasonUnknown := false
	capKilled := false
	for _, line := range splitLines(data) {
		if len(line) == 0 || line[0] != '{' {
			// Raw console line: the adapter's own backstop markers, or the
			// claude surface's rendered claim observation. The same text can
			// appear INSIDE a tool output (an agent reading source code),
			// which is why only non-JSON lines are consulted.
			s := string(line)
			if strings.Contains(s, "!! session outlived its wall-clock cap") ||
				strings.Contains(s, "!! session crossed its per-session token ceiling") ||
				strings.Contains(s, "!! stream read failed") {
				capKilled = true
			}
			if harness == "claude" && !gotPastClaim {
				// noteClaimed renders "· holding <ref>" only when a claim
				// result actually parsed — the claude mirror of a completed
				// claim_task. A resumed phase never claims; it heartbeats on
				// its seeded claim, and the "→ mcp__clankerbar__heartbeat"
				// tool marker is that observation.
				if strings.Contains(s, "· holding") || strings.Contains(s, "→ mcp__clankerbar__heartbeat") {
					gotPastClaim = true
				}
			}
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Part  json.RawMessage
			Error *struct {
				Name string          `json:"name"`
				Data json.RawMessage `json:"data"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "step_finish":
			var part struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(ev.Part, &part)
			if part.Reason != "" {
				lastReason = part.Reason
				if part.Reason == "unknown" {
					reasonUnknown = true
				} else {
					reasonUnknown = false
				}
			}
		case "tool_use":
			gotPastClaim, branchRecorded = noteTool(ev.Part, gotPastClaim, branchRecorded)
		case "error":
			if ev.Error != nil {
				msg := ev.Error.Name
				if len(ev.Error.Data) > 0 {
					msg += ": " + string(ev.Error.Data)
				}
				apiErrors = append(apiErrors, msg)
			}
		}
	}
	dead = gotPastClaim && reasonUnknown && !branchRecorded && !capKilled
	return gotPastClaim, branchRecorded, lastReason, apiErrors, dead
}

// noteTool mirrors the shared claim observer (harness.noteToolUse /
// noteClaimed) far enough for the dead-phase predicate:
//
//   - a COMPLETED claim_task means the session got past its claim; a refused
//     one has status "error" and changes nothing.
//   - a COMPLETED heartbeat means a resumed phase holding a claim.
//   - an update_task whose INPUT carries a branch, or a claim result that
//     already carried WIP (hasWip / task.branch), is the HasWIP mirror.
func noteTool(part json.RawMessage, gotPastClaim, branchRecorded bool) (bool, bool) {
	var p struct {
		Tool  string `json:"tool"`
		State *struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
			Output json.RawMessage `json:"output"`
		} `json:"state"`
	}
	if json.Unmarshal(part, &p) != nil || p.State == nil {
		return gotPastClaim, branchRecorded
	}
	completed := p.State.Status == "completed"
	switch {
	case strings.Contains(p.Tool, "claim_task"):
		if completed {
			gotPastClaim = true
			// A takeover of a task that already had a branch records WIP on
			// claim: noteClaimed reads hasWip / task.branch off the result.
			var res struct {
				HasWip bool `json:"hasWip"`
				Task   *struct {
					Branch string `json:"branch"`
				} `json:"task"`
			}
			if json.Unmarshal(p.State.Output, &res) == nil {
				if res.HasWip || (res.Task != nil && res.Task.Branch != "") {
					branchRecorded = true
				}
			}
		}
	case strings.Contains(p.Tool, "update_task") && completed:
		// HasWIP is set on the REQUEST in the live adapter (the branch arg),
		// which is the state.input here.
		var args struct {
			Branch string `json:"branch"`
		}
		if json.Unmarshal(p.State.Input, &args) == nil && args.Branch != "" {
			branchRecorded = true
		}
	case strings.Contains(p.Tool, "heartbeat") && completed:
		gotPastClaim = true
	}
	return gotPastClaim, branchRecorded
}

// splitLines splits a log into trimmed lines, skipping blanks. The opencode
// stream is one JSON object per line; claude renders text lines.
func splitLines(data []byte) [][]byte {
	var out [][]byte
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, []byte(line))
		}
	}
	return out
}

// readFile is os.ReadFile behind a seam so the known-positive-control tests can
// classify fixture logs without touching the real state dir.
var readFile = func(path string) ([]byte, error) {
	return os.ReadFile(path)
}
