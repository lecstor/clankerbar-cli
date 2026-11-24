package loop

// waiting.go — the daemon's report that one of its own blocking questions stands unanswered
// (CLA-535). A blocking ask sets its task `blocked` and releases the run's claim, leaving the
// target idle-polling with nothing claimable; the plane's `waiting` state (CLA-527) says so.
//
// The summary counts open questions but cannot say WHICH task asked, so the marker carries the
// refs. It is REPORTING ONLY: no gate, back-off or spawn path may read it.

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// waitState is one target's outstanding blocking asks. A target can have several of its own
// questions open at once — a later drain may ask while an earlier answer is still pending — and each
// must be tracked separately, because the reclaim that answers one says nothing about the others.
type waitState struct {
	on   bool
	refs []string // every outstanding own ask, oldest first; the beacon names the newest
	// openQ is the project's open-question count at the target's last poll BEFORE its FIRST ask
	// raised one. An answer is observed as the count falling back to (or below) it.
	openQ int
}

// ref names the newest outstanding ask: the task the beacon reports it waits on.
func (w waitState) ref() string {
	if len(w.refs) == 0 {
		return ""
	}
	return w.refs[len(w.refs)-1]
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
// baseline is this target's open-question count from its last poll, before the FIRST of its own asks
// landed — the count the poll checks against until every answer comes back.
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
	// A marker already on means another of this target's questions is still outstanding: keep its
	// pre-FIRST-ask baseline. Taking the later ask's count instead would clear the marker as soon as
	// EITHER answer lands (the count falls to the other ask's baseline) and beacon idle while one of
	// the daemon's own questions still stands.
	w := d.waiting[ti]
	if w.on {
		base = w.openQ
	}
	// Re-noting a ref already tracked moves it to newest rather than duplicating it: one task has one
	// question of this target's to answer.
	refs := make([]string, 0, len(w.refs)+1)
	for _, r := range w.refs {
		if r != ref {
			refs = append(refs, r)
		}
	}
	refs = append(refs, ref)
	changed := !w.on || w.ref() != ref
	d.waiting[ti] = waitState{on: true, refs: refs, openQ: base}
	d.iterMu.Unlock()
	if changed {
		if len(refs) > 1 {
			log.Printf("waiting on %s's answer to a blocking question (%d of this target's own questions stand) — beaconing waiting until they land", ref, len(refs))
		} else {
			log.Printf("waiting on %s's answer to a blocking question — beaconing waiting until it lands", ref)
		}
	}
	d.persistWaiting()
}

// clearWaiting drops ONE of target ti's outstanding asks when a drain is evidenced as that task
// reclaimed under ref: a blocking ask leaves its task blocked, so a drain of that task can only start
// after the answer landed. Answering one question says nothing about the target's other open asks, so
// the marker clears only when none remains; a drain of a different task (or of no task yet) leaves all
// of them standing. The poll's open-question count is the other clear path (clearAnsweredWaiting).
func (d *Driver) clearWaiting(ti int, ref string) {
	if ref == "" {
		return
	}
	d.iterMu.Lock()
	if ti < 0 || ti >= len(d.waiting) || !d.waiting[ti].on {
		d.iterMu.Unlock()
		return
	}
	w := d.waiting[ti]
	refs := make([]string, 0, len(w.refs))
	for _, r := range w.refs {
		if r != ref {
			refs = append(refs, r)
		}
	}
	if len(refs) == len(w.refs) {
		d.iterMu.Unlock()
		return
	}
	if len(refs) == 0 {
		d.waiting[ti] = waitState{}
	} else {
		d.waiting[ti] = waitState{on: true, refs: refs, openQ: w.openQ}
	}
	d.iterMu.Unlock()
	if len(refs) == 0 {
		log.Printf("reclaimed %s after its blocking question was answered — no longer waiting", ref)
	} else {
		log.Printf("reclaimed %s after its blocking question was answered — still waiting on %s", ref, refs[len(refs)-1])
	}
	d.persistWaiting()
}

// clearAnsweredWaiting clears the marker on a SUCCESSFUL poll once the open-question count is back
// to (or below) where it stood before the FIRST ask: every answer landed. A poll failure must not
// call this — its zero Summary would read as "every question answered". The summary carries no
// per-task attribution, so another writer's question filed after ours can hold the count up and
// postpone the flip, and with several of the daemon's own asks open the count has to fall past ALL;
// that is the price of keeping the beacon off a plane read of its own per poll.
func (d *Driver) clearAnsweredWaiting(ti int, sum backlog.Summary) {
	d.iterMu.Lock()
	if ti < 0 || ti >= len(d.waiting) || !d.waiting[ti].on || sum.OpenQuestions > d.waiting[ti].openQ {
		d.iterMu.Unlock()
		return
	}
	refs := strings.Join(d.waiting[ti].refs, ", ")
	d.waiting[ti] = waitState{}
	d.iterMu.Unlock()
	log.Printf("the answers to the questions on %s landed — dropping the waiting state", refs)
	d.persistWaiting()
}

// waitRecord is one target's saved marker, keyed by target name so a restarted daemon finds the
// same project rather than a round-robin position.
type waitRecord struct {
	// TaskRefs is every outstanding own ask, oldest first (the beacon names the last).
	TaskRefs []string `json:"taskRefs,omitempty"`
	// TaskRef is the single-ref format a previous binary wrote: read for an upgrade, never written.
	TaskRef       string `json:"taskRef,omitempty"`
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
		out[d.waitingKey(ti)] = waitRecord{
			TaskRefs:      append([]string(nil), d.waiting[ti].refs...),
			OpenQuestions: d.waiting[ti].openQ,
		}
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
		if !ok {
			continue
		}
		refs := make([]string, 0, len(rec.TaskRefs)+1)
		for _, r := range rec.TaskRefs {
			if r != "" {
				refs = append(refs, r)
			}
		}
		if len(refs) == 0 && rec.TaskRef != "" {
			refs = []string{rec.TaskRef}
		}
		if len(refs) == 0 {
			continue
		}
		d.waiting[ti] = waitState{on: true, refs: refs, openQ: rec.OpenQuestions}
		// Restored, not verified: the first successful poll decides whether the answer landed while
		// the daemon was down.
		log.Printf("restored the waiting state on %s from the saved record — the next poll decides whether its answer has landed", strings.Join(refs, ", "))
	}
}
