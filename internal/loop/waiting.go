package loop

// waiting.go — the daemon's report that one of its own blocking questions stands unanswered
// (CLA-535). A blocking ask sets its task `blocked` and releases the run's claim, leaving the
// target idle-polling with nothing claimable; the plane's `waiting` state (CLA-527) says so.
//
// The summary counts open questions but cannot say WHICH task asked, so the marker carries the
// ref. It is REPORTING ONLY: no gate, back-off or spawn path may read it.

import (
	"encoding/json"
	"errors"
	"log"
	"os"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// waitState is one target's outstanding blocking ask.
type waitState struct {
	on  bool
	ref string // the task whose answer the daemon waits on
	// openQ is the project's open-question count at the target's last poll BEFORE the ask raised
	// it. An answer is observed as the count falling back to (or below) it.
	openQ int
}

// waitingAt is the bounds-checked read of the per-target waiting marker, mirroring iterAt. Callers
// hold iterMu.
func (d *Driver) waitingAt(ti int) waitState {
	if ti < 0 || ti >= len(d.waiting) {
		return waitState{}
	}
	return d.waiting[ti]
}

// noteWaiting records that target ti's last drain ended by filing a blocking question on ref. The
// baseline is this target's open-question count from its last poll, before the ask landed — the
// count the poll checks against until the answer comes back.
func (d *Driver) noteWaiting(ti int, ref string) {
	if ref == "" {
		return
	}
	base := 0
	if ti >= 0 && ti < len(d.openQs) {
		base = d.openQs[ti]
	}
	d.iterMu.Lock()
	if ti < 0 || ti >= len(d.waiting) {
		d.iterMu.Unlock()
		return
	}
	changed := !d.waiting[ti].on || d.waiting[ti].ref != ref
	d.waiting[ti] = waitState{on: true, ref: ref, openQ: base}
	d.iterMu.Unlock()
	if changed {
		log.Printf("waiting on %s's answer to a blocking question — beaconing waiting until it lands", ref)
	}
	d.persistWaiting()
}

// clearWaiting drops target ti's marker, reporting whether it held one. A drain that starts is a
// fresh claim: whatever the daemon was waiting on was either answered or reclaimed, and the new
// drain's own outcome will say what it is doing now.
func (d *Driver) clearWaiting(ti int) bool {
	d.iterMu.Lock()
	if ti < 0 || ti >= len(d.waiting) || !d.waiting[ti].on {
		d.iterMu.Unlock()
		return false
	}
	ref := d.waiting[ti].ref
	d.waiting[ti] = waitState{}
	d.iterMu.Unlock()
	log.Printf("no longer waiting on %s — the question was answered or the task reclaimed", ref)
	d.persistWaiting()
	return true
}

// clearAnsweredWaiting clears the marker on a SUCCESSFUL poll once the open-question count is back
// to (or below) where it stood before the ask: the answer landed. A poll failure must not call this
// — its zero Summary would read as "every question answered". The summary carries no per-task
// attribution, so another writer's question filed after ours can hold the count up and postpone the
// flip; that is the price of keeping the beacon off a plane read of its own per poll.
func (d *Driver) clearAnsweredWaiting(ti int, sum backlog.Summary) {
	d.iterMu.Lock()
	if ti < 0 || ti >= len(d.waiting) || !d.waiting[ti].on || sum.OpenQuestions > d.waiting[ti].openQ {
		d.iterMu.Unlock()
		return
	}
	ref := d.waiting[ti].ref
	d.waiting[ti] = waitState{}
	d.iterMu.Unlock()
	log.Printf("the answer to the question on %s landed — dropping the waiting state", ref)
	d.persistWaiting()
}

// waitRecord is one target's saved marker, keyed by target name so a restarted daemon finds the
// same project rather than a round-robin position.
type waitRecord struct {
	TaskRef       string `json:"taskRef"`
	OpenQuestions int    `json:"openQuestions"`
}

// waitingKey identifies a target in the saved record: its project name when it has one, "" for a
// single unnamed target.
func (d *Driver) waitingKey(ti int) string {
	if ti >= 0 && ti < len(d.targets) {
		return d.targets[ti].Name
	}
	return ""
}

// persistWaiting saves the live markers so a restarted daemon keeps reporting what it waits on.
// Fail-soft like every other telemetry write: a lost file costs the waiting state until the next
// ask, never the loop.
func (d *Driver) persistWaiting() {
	if d.state == nil {
		return
	}
	d.iterMu.Lock()
	out := make(map[string]waitRecord, len(d.waiting))
	for ti := range d.waiting {
		if !d.waiting[ti].on {
			continue
		}
		out[d.waitingKey(ti)] = waitRecord{TaskRef: d.waiting[ti].ref, OpenQuestions: d.waiting[ti].openQ}
	}
	d.iterMu.Unlock()
	var err error
	if len(out) == 0 {
		if err = d.state.Remove(statedir.WaitingName); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		var b []byte
		if b, err = json.Marshal(out); err == nil {
			err = d.state.WriteFileAtomic(statedir.WaitingName, b)
		}
	}
	if err != nil {
		log.Printf("could not save the waiting record to the state dir (a restart will not know which task it waits on): %v", err)
	}
}

// loadWaiting restores the markers a previous process saved. A missing file is the normal first
// start; an unreadable or corrupt one is logged and ignored, never fatal.
func (d *Driver) loadWaiting() {
	if d.state == nil {
		return
	}
	b, err := d.state.ReadFile(statedir.WaitingName)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("ignoring the saved waiting record: %v", err)
		return
	}
	var saved map[string]waitRecord
	if err := json.Unmarshal(b, &saved); err != nil {
		log.Printf("ignoring a corrupt saved waiting record: %v", err)
		return
	}
	d.iterMu.Lock()
	defer d.iterMu.Unlock()
	for ti := range d.waiting {
		rec, ok := saved[d.waitingKey(ti)]
		if !ok || rec.TaskRef == "" {
			continue
		}
		d.waiting[ti] = waitState{on: true, ref: rec.TaskRef, openQ: rec.OpenQuestions}
		log.Printf("restored the waiting state: the question on %s is still unanswered", rec.TaskRef)
	}
}
