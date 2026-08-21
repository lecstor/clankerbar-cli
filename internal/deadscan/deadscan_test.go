package deadscan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture logs are written through this seam so the tests classify real file
// content without touching the machine's actual state dir.
func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixtureRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		writeFixture(t, root, name, content)
	}
	return root
}

// A DEAD opencode session, shaped like the 2026-08-20 deaths: claimed a task,
// worked, then the final step_finish carried reason "unknown" with no branch
// recorded and no error event — the "produced nothing" signature.
const deadLog = `{"type":"step_start","timestamp":1,"sessionID":"ses_dead","part":{"id":"p1","type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_next_task","callID":"c1","state":{"status":"completed","input":{},"output":"{\"next\":{\"id\":\"t1\",\"ref\":\"CLA-1\"}}"}}}
{"type":"tool_use","timestamp":3,"sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c2","state":{"status":"completed","input":{"taskId":"t1"},"output":"{\"task\":{\"id\":\"t1\",\"ref\":\"CLA-1\"},\"branch\":\"clanker/x\",\"hasWip\":false}"}}}
{"type":"step_finish","timestamp":4,"sessionID":"ses_dead","part":{"id":"p2","reason":"unknown","tokens":{"total":0,"input":0,"output":0}}}
`

// A HEALTHY opencode session: claimed, worked, ended with reason "stop".
const healthyLog = `{"type":"step_start","timestamp":1,"sessionID":"ses_ok","part":{"id":"p1","type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_ok","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c1","state":{"status":"completed","input":{"taskId":"t2"},"output":"{\"task\":{\"id\":\"t2\",\"ref\":\"CLA-2\"},\"branch\":\"clanker/y\",\"hasWip\":false}"}}}
{"type":"step_finish","timestamp":3,"sessionID":"ses_ok","part":{"id":"p2","reason":"stop","tokens":{"total":100,"input":50,"output":50}}}
`

// The NOT-A-DEATH that looks exactly like one (the 2026-08-20 06:32 shape): a
// session dispatched to take over a task, REFUSED with lease_live, and stopped
// rather than substituting another task. It ends with reason "unknown" and no
// branch, satisfying both conjuncts of deadPhase — but it never got past its
// claim, so it counts toward NEITHER counter (operator decision f518a454).
const refusedClaimLog = `{"type":"step_start","timestamp":1,"sessionID":"ses_refused","part":{"id":"p1","type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_refused","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c1","state":{"status":"error","input":{"taskId":"t3"},"error":"{\"error\":{\"code\":\"lease_live\",\"message\":\"the owner heartbeated four minutes ago\"}}"}}}
{"type":"step_finish","timestamp":3,"sessionID":"ses_refused","part":{"id":"p2","reason":"unknown","tokens":{"total":0,"input":0,"output":0}}}
`

// A session that recorded a branch and THEN died on an unknown reason: it
// produced something, so it is a run but not dead (deadPhase's second conjunct
// is load-bearing).
const branchThenDeadLog = `{"type":"step_start","timestamp":1,"sessionID":"ses_wip","part":{"id":"p1","type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_wip","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c1","state":{"status":"completed","input":{"taskId":"t4"},"output":"{\"task\":{\"id\":\"t4\",\"ref\":\"CLA-4\"},\"branch\":\"clanker/z\",\"hasWip\":false}"}}}
{"type":"tool_use","timestamp":3,"sessionID":"ses_wip","part":{"type":"tool","tool":"clankerbar_update_task","callID":"c2","state":{"status":"completed","input":{"taskId":"t4","branch":"clanker/z"},"output":"{\"task\":{\"id\":\"t4\",\"status\":\"in_progress\"},\"changed\":[\"branch\"]}"}}}
{"type":"step_finish","timestamp":4,"sessionID":"ses_wip","part":{"id":"p2","reason":"unknown","tokens":{"total":0,"input":0,"output":0}}}
`

// A claude (rendered-text) session: no JSON events, no finish reason, so it can
// never be dead — but it ran.
const claudeLog = `I'll work the next task.
  → mcp__clankerbar__claim_task
  · holding CLA-5 (release on exit if nothing is pushed)
  → Bash
  → Bash
`

// A TAKEOVER of a task that already carried WIP: claim_task's own result — an
// MCP tool result, flattened by the opencode adapter to a STRING (see
// opencodeToolState.Output, internal/harness/opencode.go) — reports
// "hasWip":true, and the session then dies with reason "unknown" WITHOUT ever
// calling update_task again. It must be a run, not dead: the inherited WIP is
// the driver's own Claim.HasWIP signal, carried on the claim itself.
const takeoverWipThenDeadLog = `{"type":"step_start","timestamp":1,"sessionID":"ses_takeover","part":{"id":"p1","type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_takeover","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c1","state":{"status":"completed","input":{"taskId":"t7","takeover":true},"output":"{\"task\":{\"id\":\"t7\",\"ref\":\"CLA-7\"},\"branch\":\"clanker/w\",\"hasWip\":true}"}}}
{"type":"step_finish","timestamp":3,"sessionID":"ses_takeover","part":{"id":"p2","reason":"unknown","tokens":{"total":0,"input":0,"output":0}}}
`

// The other half of the same WIP check: hasWip false but task.branch set — the
// real wire shape nests branch INSIDE task (internal/harness/claude.go's
// Task.Branch, which the fixed noteTool mirrors via res.Task.Branch), unlike
// takeoverWipThenDeadLog's top-level hasWip, so this exercises the
// res.Task.Branch != "" arm of the check on its own.
const takeoverBranchOnlyThenDeadLog = `{"type":"step_start","timestamp":1,"sessionID":"ses_takeover2","part":{"id":"p1","type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_takeover2","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c1","state":{"status":"completed","input":{"taskId":"t8","takeover":true},"output":"{\"task\":{\"id\":\"t8\",\"ref\":\"CLA-8\",\"branch\":\"clanker/v\"},\"hasWip\":false}"}}}
{"type":"step_finish","timestamp":3,"sessionID":"ses_takeover2","part":{"id":"p2","reason":"unknown","tokens":{"total":0,"input":0,"output":0}}}
`

// A codex session that died before ever reporting usage — the real shape from
// internal/harness/usagereported_test.go's died-early fixture: thread.started
// then item.completed, no turn.completed at all. Codex emits no claim state
// the scan can read (classifyEvents' doc comment), so this never lands in any
// cell either way — but it MUST be labelled harness "codex", not misclassified
// as "claude" by harnessOf falling through its default.
const codexDiedEarlyLog = `{"type":"thread.started","thread_id":"t1"}
{"type":"item.completed","item":{"type":"agent_message","text":"died early"}}
`

// The tool_count_limit APIError known-positive: a genuine error EVENT. This is
// what the three 2026-08-19 logs carry, and what the scan must find — while
// ignoring the same string when it appears as task-body TEXT an agent read.
const apiErrorLog = `{"type":"step_start","timestamp":1,"sessionID":"ses_err","part":{"id":"p1","type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_err","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c1","state":{"status":"completed","input":{"taskId":"t6"},"output":"{\"task\":{\"id\":\"t6\",\"ref\":\"CLA-6\"},\"branch\":\"clanker/e\",\"hasWip\":false}"}}}
{"type":"error","timestamp":3,"sessionID":"ses_err","error":{"name":"APIError","data":{"message":"Error from provider (Console Go): [unsupported_tool_schema] The tool schema is not supported (tool_count_limit).","statusCode":400}}}
{"type":"step_finish","timestamp":4,"sessionID":"ses_err","part":{"id":"p2","reason":"tool-calls","tokens":{"total":10,"input":5,"output":5}}}
`

// The trap in disguise: the SAME string appearing as text an agent read (a
// tool output quoting a task body, exactly what the 2026-08-20 logs contain)
// is NOT an error event and must not match FindErrors.
const quotedTextLog = `{"type":"step_start","timestamp":1,"sessionID":"ses_quote","part":{"id":"p1","type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_quote","part":{"type":"tool","tool":"read","callID":"c1","state":{"status":"completed","input":{"filePath":"/x"},"output":"the task body mentions tool_count_limit but this is text an agent read"}}}
{"type":"step_finish","timestamp":3,"sessionID":"ses_quote","part":{"id":"p2","reason":"stop","tokens":{"total":10,"input":5,"output":5}}}
`

func scanFixtures(t *testing.T, files map[string]string) []Log {
	t.Helper()
	logs, err := Scan(fixtureRoot(t, files))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return logs
}

func byName(t *testing.T, logs []Log, name string) Log {
	t.Helper()
	for _, l := range logs {
		if filepath.Base(l.Path) == name {
			return l
		}
	}
	t.Fatalf("no log named %s in %v", name, logs)
	return Log{}
}

func TestScan_ClassifiesBothShapes(t *testing.T) {
	files := map[string]string{
		"w1/iteration-20260820-004335-d3-pimplement-a0-5ff482cb.log": deadLog,
		"w1/iteration-20260820-141405-d4-pimplement-a0-d869542f.log": healthyLog,
		"w1/iteration-20260820-063200-d1-pimplement-a0-a1768e20.log": refusedClaimLog,
		"w1/iteration-20260820-024914-d4-pimplement-a0-b0b2155a.log": branchThenDeadLog,
		"w1/iteration-20260820-035000-d6-preview-a0-fa53f575.log":    claudeLog,
	}
	logs := scanFixtures(t, files)

	dead := byName(t, logs, "iteration-20260820-004335-d3-pimplement-a0-5ff482cb.log")
	if !dead.GotPastClaim || !dead.Dead || dead.LastReason != "unknown" {
		t.Errorf("dead fixture misclassified: gotPastClaim=%v dead=%v lastReason=%q", dead.GotPastClaim, dead.Dead, dead.LastReason)
	}
	if dead.Day != "2026-08-20" || dead.Phase != "implement" || dead.Harness != "opencode" {
		t.Errorf("dead fixture metadata: day=%q phase=%q harness=%q", dead.Day, dead.Phase, dead.Harness)
	}

	ok := byName(t, logs, "iteration-20260820-141405-d4-pimplement-a0-d869542f.log")
	if !ok.GotPastClaim || ok.Dead || ok.LastReason != "stop" {
		t.Errorf("healthy fixture misclassified: gotPastClaim=%v dead=%v lastReason=%q", ok.GotPastClaim, ok.Dead, ok.LastReason)
	}

	// The refused-claim session is the not-a-death that looks exactly like one:
	// reason unknown, no branch — but never got past its claim.
	refused := byName(t, logs, "iteration-20260820-063200-d1-pimplement-a0-a1768e20.log")
	if refused.GotPastClaim || refused.Dead {
		t.Errorf("refused-claim fixture must count toward NEITHER counter: gotPastClaim=%v dead=%v", refused.GotPastClaim, refused.Dead)
	}
	if refused.LastReason != "unknown" {
		t.Errorf("refused-claim fixture should still carry the unknown reason (that is what makes it look dead): %q", refused.LastReason)
	}

	// A branch recorded before dying: run, not dead.
	wip := byName(t, logs, "iteration-20260820-024914-d4-pimplement-a0-b0b2155a.log")
	if !wip.GotPastClaim || !wip.BranchRecorded || wip.Dead {
		t.Errorf("branch-then-dead fixture must be a run, not dead: gotPastClaim=%v branch=%v dead=%v", wip.GotPastClaim, wip.BranchRecorded, wip.Dead)
	}

	// Claude: ran, can never be dead (no finish reason on its surface).
	claude := byName(t, logs, "iteration-20260820-035000-d6-preview-a0-fa53f575.log")
	if claude.Harness != "claude" {
		t.Errorf("claude fixture harness = %q, want claude", claude.Harness)
	}
	if !claude.GotPastClaim || claude.Dead {
		t.Errorf("claude fixture misclassified: gotPastClaim=%v dead=%v", claude.GotPastClaim, claude.Dead)
	}
}

func TestSummarize_CountsPerDayPhaseHarness(t *testing.T) {
	files := map[string]string{
		"w1/iteration-20260820-004335-d3-pimplement-a0-5ff482cb.log": deadLog,
		"w1/iteration-20260820-141405-d4-pimplement-a0-d869542f.log": healthyLog,
		"w1/iteration-20260820-063200-d1-pimplement-a0-a1768e20.log": refusedClaimLog,
		"w1/iteration-20260820-024914-d4-pimplement-a0-b0b2155a.log": branchThenDeadLog,
		"w1/iteration-20260820-035000-d6-preview-a0-fa53f575.log":    claudeLog,
		"w2/iteration-20260819-090233-d1-pimplement-a0-83ca1b2b.log": deadLog,
	}
	cells := Summarize(scanFixtures(t, files))
	if len(cells) != 3 {
		t.Fatalf("got %d cells, want 3 (two days x implement/opencode + 08-20 review/claude): %+v", len(cells), cells)
	}
	// 08-19 implement/opencode: one dead of one run.
	if c := cells[0]; c.Day != "2026-08-19" || c.Phase != "implement" || c.Harness != "opencode" ||
		c.Run != 1 || c.Dead != 1 {
		t.Errorf("08-19 cell = %+v, want run=1 dead=1", c)
	}
	// 08-20 implement/opencode: dead, healthy, branch-then-dead run; refused-claim in NEITHER counter.
	if c := cells[1]; c.Day != "2026-08-20" || c.Phase != "implement" || c.Harness != "opencode" ||
		c.Run != 3 || c.Dead != 1 {
		t.Errorf("08-20 implement cell = %+v, want run=3 dead=1 (refused claim counts toward neither)", c)
	}
	// 08-20 review/claude: one run, never dead.
	if c := cells[2]; c.Day != "2026-08-20" || c.Phase != "review" || c.Harness != "claude" ||
		c.Run != 1 || c.Dead != 0 {
		t.Errorf("08-20 review cell = %+v, want run=1 dead=0", c)
	}
}

// The known-positive control: FindErrors must find the genuine APIError event
// and ignore the same string quoted as tool-output text — a text search finds
// both, which is exactly the broken-search trap the task names.
func TestFindErrors_KnownPositiveControl(t *testing.T) {
	files := map[string]string{
		"w1/iteration-20260819-164201-d1-pimplement-a0-26be477b.log": apiErrorLog,
		"w1/iteration-20260820-004335-d3-pimplement-a0-5ff482cb.log": quotedTextLog,
	}
	logs := scanFixtures(t, files)

	matches := FindErrors(logs, "tool_count_limit")
	if len(matches) != 1 {
		t.Fatalf("FindErrors(tool_count_limit) = %d logs, want exactly 1 (the genuine APIError event; the quoted-text log must NOT match): %+v", len(matches), matches)
	}
	if !strings.HasSuffix(matches[0].Path, "iteration-20260819-164201-d1-pimplement-a0-26be477b.log") {
		t.Errorf("matched the wrong log: %s", matches[0].Path)
	}
}

func TestScan_WalksSlugDirs(t *testing.T) {
	// Logs live one level below the root (per-workdir slug dirs), plus the
	// sentinel files a state dir carries. Both must be handled.
	files := map[string]string{
		"dev-abc123/iteration-20260820-004335-d3-pimplement-a0-5ff482cb.log": deadLog,
		"dev-abc123/.clankerbar-statedir":                                    "sentinel\n",
		"dev-abc123/.gitignore":                                              "*\n",
		"other-slug/iteration-20260819-090233-d1-pimplement-a0-83ca1b2b.log": healthyLog,
	}
	logs := scanFixtures(t, files)
	if len(logs) != 2 {
		t.Fatalf("Scan found %d logs, want 2 (sentinel/gitignore must not be classified): %v", len(logs), logs)
	}
}

// A takeover that inherits WIP off claim_task's OWN result must not be
// misread as dead: the opencode adapter flattens an MCP tool result to a
// STRING (opencodeToolState.Output), so noteTool must double-unwrap it the way
// noteClaimed does, not unmarshal the still-quoted bytes directly.
func TestScan_TakeoverInheritsWipFromClaimResultItself(t *testing.T) {
	files := map[string]string{
		"w1/iteration-20260820-050000-d2-pimplement-a0-c1a95b21.log": takeoverWipThenDeadLog,
	}
	l := byName(t, scanFixtures(t, files), "iteration-20260820-050000-d2-pimplement-a0-c1a95b21.log")
	if !l.GotPastClaim {
		t.Fatalf("takeover fixture: GotPastClaim = false, want true")
	}
	if !l.BranchRecorded {
		t.Errorf("takeover fixture: BranchRecorded = false, want true — hasWip:true on claim_task's own result must be read")
	}
	if l.Dead {
		t.Errorf("takeover fixture: Dead = true, want false — inherited WIP makes this a run, not a death")
	}
}

// The res.Task.Branch != "" arm of the same check, on its own: hasWip false
// but the branch nested inside task (the real wire shape) must still be read.
func TestScan_TakeoverInheritsBranchNestedInTask(t *testing.T) {
	files := map[string]string{
		"w1/iteration-20260820-051000-d2-pimplement-a0-e2b06c32.log": takeoverBranchOnlyThenDeadLog,
	}
	l := byName(t, scanFixtures(t, files), "iteration-20260820-051000-d2-pimplement-a0-e2b06c32.log")
	if !l.GotPastClaim {
		t.Fatalf("takeover fixture: GotPastClaim = false, want true")
	}
	if !l.BranchRecorded {
		t.Errorf("takeover fixture: BranchRecorded = false, want true — task.branch on claim_task's own result must be read even when hasWip is false")
	}
	if l.Dead {
		t.Errorf("takeover fixture: Dead = true, want false — inherited branch makes this a run, not a death")
	}
}

// A codex session must be labelled harness "codex", not fall through
// harnessOf's default to "claude" because its event types don't match any
// case — the real codex vocabulary is thread./item./turn.-prefixed, not
// stream_start/exec_result.
func TestScan_ClassifiesCodexHarness(t *testing.T) {
	files := map[string]string{
		"w1/iteration-20260819-070000-d1-pimplement-a0-9b3f1d44.log": codexDiedEarlyLog,
	}
	l := byName(t, scanFixtures(t, files), "iteration-20260819-070000-d1-pimplement-a0-9b3f1d44.log")
	if l.Harness != "codex" {
		t.Errorf("codex fixture: Harness = %q, want %q", l.Harness, "codex")
	}
}
