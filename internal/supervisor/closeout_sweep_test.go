package supervisor

// The close-out's task-state sweep (CLA-969): the per-task maps the supervisor
// keeps between polls must stay bounded over a long uptime, and the sweep must
// only ever act on a queue the pass actually read.

import (
	"fmt"
	"testing"
)

// The state sweep keeps a task's entries while it is approved (or was, on the
// previous pass) and drops them once it has left the queue for good. Keys scoped
// to a project or the fleet are not task state and stay, and the task a conflict
// session is working is kept even though the claim has taken it out of the queue.
func TestCloseOutSweepPrunesOnlyTasksNoLongerApproved(t *testing.T) {
	d := &Supervisor{
		closeOutSaid: map[string]string{
			"task:t-1":                "closed",
			"task:t-1:conflict-stale": "stale",
			"task:t-2":                "closed",
			"base:acme:acme/widgets":  "base",
			"policy:acme":             "policy",
		},
		closeOutConflicts: map[string]conflictEvidence{"t-1": {pair: "a@b"}, "t-2": {pair: "c@d"}},
		conflictSaid: map[string]string{
			"claimed:t-1":   "claimed",
			"exhausted:t-2": "exhausted",
			"policy:acme":   "policy",
			"claude":        "claude",
		},
		conflictTried:     map[string]conflictAttempt{"t-1": {n: 2}, "t-2": {n: 1}},
		closeOutSeenTasks: map[string]bool{"t-1": true},
	}

	// t-1 was approved on the previous pass, so the first pass without it is
	// grace: a conflict session's claim takes a task out of the queue while its
	// retry budget must survive.
	d.pruneCloseOutState(map[string]bool{"t-2": true})
	for _, check := range []struct {
		name string
		kept bool
	}{
		{"closeOutSaid task line", d.closeOutSaid["task:t-1"] != ""},
		{"closeOutSaid suffixed task line", d.closeOutSaid["task:t-1:conflict-stale"] != ""},
		{"closeOutConflicts evidence", d.closeOutConflicts["t-1"].pair == "a@b"},
		{"conflictSaid task line", d.conflictSaid["claimed:t-1"] == "claimed"},
		{"conflictTried budget", d.conflictTried["t-1"].n == 2},
	} {
		if !check.kept {
			t.Errorf("%s was pruned on the grace pass", check.name)
		}
	}

	// A second pass without it is knowledge: every per-task entry goes.
	d.pruneCloseOutState(map[string]bool{"t-2": true})
	if _, ok := d.closeOutSaid["task:t-1"]; ok {
		t.Error("a task that left the approved queue kept its close-out line")
	}
	if _, ok := d.closeOutSaid["task:t-1:conflict-stale"]; ok {
		t.Error("a task that left the approved queue kept its suffixed close-out line")
	}
	if _, ok := d.closeOutConflicts["t-1"]; ok {
		t.Error("a task that left the approved queue kept its conflict evidence")
	}
	if _, ok := d.conflictSaid["claimed:t-1"]; ok {
		t.Error("a task that left the approved queue kept its conflict line")
	}
	if _, ok := d.conflictTried["t-1"]; ok {
		t.Error("a task that left the approved queue kept its conflict retry budget")
	}

	// The still-approved task and the non-task keys are untouched: a project's
	// stand-down and the fleet-wide no-Claude line are not task state.
	if _, ok := d.closeOutSaid["task:t-2"]; !ok {
		t.Error("an approved task lost its close-out line")
	}
	if _, ok := d.conflictTried["t-2"]; !ok {
		t.Error("an approved task lost its conflict retry budget")
	}
	for _, key := range []string{"base:acme:acme/widgets", "policy:acme"} {
		if _, ok := d.closeOutSaid[key]; !ok {
			t.Errorf("the project-scoped key %q was pruned", key)
		}
	}
	if _, ok := d.conflictSaid["claude"]; !ok {
		t.Error("the fleet-wide no-Claude line was pruned")
	}

	// A conflict session's live task is kept outright: its claim is what took it
	// out of the approved queue, and dropping its budget would reset the bound.
	d.setConflictActive("t-3")
	d.conflictTried["t-3"] = conflictAttempt{n: 1}
	d.conflictSaid["busy:t-3"] = "busy"
	d.pruneCloseOutState(nil)
	d.pruneCloseOutState(nil)
	if _, ok := d.conflictTried["t-3"]; !ok {
		t.Error("the conflict session's live task lost its retry budget")
	}
	if _, ok := d.conflictSaid["busy:t-3"]; !ok {
		t.Error("the conflict session's live task lost its line")
	}
}

// The pass wires the sweep: a task that leaves the approved queue is pruned, but
// only after one grace pass, so a claim that hides it for a pass does not reset
// its state.
func TestCloseOutPassSweepsTaskStateOnceTheTaskLeavesTheQueue(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	d := f.supervisor(0)

	f.run(d)
	key := "task:" + closeOutTestTask
	if _, ok := d.closeOutSaid[key]; !ok {
		t.Fatalf("the pass did not record its task line: %v", d.closeOutSaid)
	}

	f.plane.serveTool("list_tasks", `{"tasks":[],"count":0,"total":0,"view":"full"}`)
	f.run(d)
	if _, ok := d.closeOutSaid[key]; !ok {
		t.Errorf("the sweep dropped a task on the first pass that no longer listed it: %v", d.closeOutSaid)
	}

	f.run(d)
	if _, ok := d.closeOutSaid[key]; ok {
		t.Errorf("the sweep kept a task that has left the approved queue: %v", d.closeOutSaid)
	}
}

// A pass that could not read a queue learned nothing about it: it must not sweep,
// because a task missing from an unread queue is not a task that left the queue.
func TestCloseOutPassDoesNotSweepOnAnUnreadQueue(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	d := f.supervisor(0)

	f.run(d)
	key := "task:" + closeOutTestTask
	if _, ok := d.closeOutSaid[key]; !ok {
		t.Fatalf("the pass did not record its task line: %v", d.closeOutSaid)
	}

	f.plane.refuseTool("list_tasks", "plane read failed")
	f.run(d)
	f.run(d)

	if _, ok := d.closeOutSaid[key]; !ok {
		t.Errorf("an unread queue was treated as an empty one and swept: %v", d.closeOutSaid)
	}
}

// A pass with no project to work - every instance stopped, refused or
// config-less - read no queue at all. Its empty approved set is the absence of
// knowledge, not an empty queue, so two such passes (a stopped fleet at the 15s
// poll) must not prune anyone's state, least of all the conflict retry budget a
// resumed fleet continues to spend from.
func TestCloseOutPassDoesNotSweepWithNoTargets(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	d := f.supervisor(0)
	d.instances[0].desired = RosterDesiredStopped

	d.closeOutSaid = map[string]string{"task:gone": "closed"}
	d.conflictSaid = map[string]string{"exhausted:gone": "exhausted"}
	d.closeOutSeenTasks = map[string]bool{"gone": true}
	d.conflictTried = map[string]conflictAttempt{"gone": {n: maxConflictAttempts}}

	f.run(d)
	f.run(d)

	if _, ok := d.closeOutSaid["task:gone"]; !ok {
		t.Errorf("a stopped fleet's empty target list was treated as an empty queue and swept a close-out line: %v", d.closeOutSaid)
	}
	if _, ok := d.conflictSaid["exhausted:gone"]; !ok {
		t.Errorf("a stopped fleet's empty target list was treated as an empty queue and swept a conflict line: %v", d.conflictSaid)
	}
	if got := d.conflictTried["gone"].n; got != maxConflictAttempts {
		t.Errorf("a stopped fleet's empty target list reset the conflict retry budget (%d, want %d)", got, maxConflictAttempts)
	}
}

// A capped queue page does not list every approved task, so the tasks past the cap
// are unknown rather than gone: the sweep must not drop their state.
func TestCloseOutPassDoesNotSweepACappedQueue(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.writePRList() // the served task stands down at "no OPEN pull request"
	f.plane.serveTool("list_tasks", fmt.Sprintf(
		`{"tasks":[{"id":%q,"ref":%q,"title":"a task","branch":%q,"repo":%q}],"count":1,"total":60,"view":"full"}`,
		closeOutTestTask, closeOutTestRef, closeOutTestHead, closeOutTestRepo))
	d := f.supervisor(0)
	d.closeOutSaid = map[string]string{"task:gone": "closed"}
	d.closeOutSeenTasks = map[string]bool{"gone": true}

	f.run(d)
	f.run(d)

	if _, ok := d.closeOutSaid["task:gone"]; !ok {
		t.Errorf("a capped queue was treated as the whole queue and swept a task past the cap: %v", d.closeOutSaid)
	}
}
