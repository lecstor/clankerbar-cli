package loop

// The daemon's record of the claim its CURRENT child holds (CLA-708).
//
// A session that dies with the daemon process — a crash, a kill, a machine
// going to sleep — cannot hand its claim back, and nothing in the supervisor
// used to know there was one. The lease then ran out its full 30 minutes while
// the work sat idle. CURRENT_CLAIM is the missing fact: the driver writes it
// when a claim is observed, updates the branch when one is recorded, and removes
// it on every path where it lets go. A file still present when the child exits
// therefore means the child did not let go, and the supervisor hands the claim
// back on its behalf.
//
// It lives in the daemon's own state dir — the directory the supervisor already
// owns and the child is told to use — so the supervisor reads it without any
// new channel, and a normal daemon exit leaves nothing to find.
//
// The schema is deliberately small and stable: the supervisor only needs the
// ids to call update_task, the project to pick the right MCP endpoint, and the
// branch to choose between release (none) and expireLease (pushed work). The
// claim file is NOT a source of truth about the plane: the plane refuses the
// call if the run already ended or was taken over, and that refusal is a
// logged no-op.

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// CurrentClaimName is the file the daemon writes into its state dir and the
// supervisor reads when a child exits. It is a statedir artifact, so it is in
// that package's adoptable set.
const CurrentClaimName = "CURRENT_CLAIM"

// CurrentClaim is the on-disk record.
type CurrentClaim struct {
	TaskID    string    `json:"taskId"`
	RunID     string    `json:"runId"`
	Project   string    `json:"project"`
	Branch    string    `json:"branch"`
	ClaimedAt time.Time `json:"claimedAt"`
}

// ReadCurrentClaim reads the record from an OPEN state-dir handle. It returns
// the underlying not-exist error when there is no file, so a caller can tell
// "nothing to hand back" from "something is there but unreadable".
func ReadCurrentClaim(st *statedir.Dir) (*CurrentClaim, error) {
	if st == nil {
		return nil, errors.New("current claim: no state dir")
	}
	raw, err := st.ReadFile(CurrentClaimName)
	if err != nil {
		return nil, err
	}
	var c CurrentClaim
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.TaskID == "" || c.RunID == "" {
		return nil, errors.New("current claim: taskId and runId are required")
	}
	return &c, nil
}

// noteCurrentClaim folds one observed claim snapshot into the file. It is the
// daemon's only writer: the lease renewer sees every claim transition, including
// the settle, and the salvage path calls it after recording a late branch.
//
// A snapshot that is not Held (never claimed, or already settled) removes the
// file: nothing is owed to the next process. ClaimedAt is preserved across
// updates of the SAME claim, so it means "when this claim was taken", not "when
// this record was last rewritten" — the supervisor logs it when it hands a dead
// claim back.
func (d *Driver) noteCurrentClaim(c harness.Claim, project string) {
	if d.state == nil {
		return
	}
	// Serialised because a review panel's members run CONCURRENTLY (CLA-697) and
	// each starts its own lease renewer on the same run; without this, their
	// identical snapshots race on WriteFileAtomic's fixed `.tmp` name and one
	// writer's error can leave the record missing. The content is the same claim,
	// so serialising costs nothing but removes the race.
	d.claimFileMu.Lock()
	defer d.claimFileMu.Unlock()
	if !c.Held() || c.TaskID == "" || c.RunID == "" {
		d.clearCurrentClaimLocked()
		return
	}
	claimedAt := time.Now().UTC()
	if prev, err := ReadCurrentClaim(d.state); err == nil &&
		prev.TaskID == c.TaskID && prev.RunID == c.RunID && !prev.ClaimedAt.IsZero() {
		claimedAt = prev.ClaimedAt
	}
	body, err := json.MarshalIndent(CurrentClaim{
		TaskID:    c.TaskID,
		RunID:     c.RunID,
		Project:   project,
		Branch:    c.Branch,
		ClaimedAt: claimedAt,
	}, "", "  ")
	if err != nil {
		return
	}
	body = append(body, '\n')
	if err := d.state.WriteFileAtomic(CurrentClaimName, body); err != nil {
		// The record is an optimisation for a dead process; a write failure must
		// not affect the live session. The supervisor's fallback is the same
		// expiring lease that existed before this file.
		logCurrentClaimError("write", err)
	}
}

// clearCurrentClaim removes the record, if any. Safe on a nil state dir and on
// a file that is already gone.
func (d *Driver) clearCurrentClaim() {
	d.claimFileMu.Lock()
	defer d.claimFileMu.Unlock()
	d.clearCurrentClaimLocked()
}

// clearCurrentClaimLocked is clearCurrentClaim for a caller already holding
// claimFileMu.
func (d *Driver) clearCurrentClaimLocked() {
	if d.state == nil {
		return
	}
	if err := d.state.Remove(CurrentClaimName); err != nil && !errors.Is(err, os.ErrNotExist) {
		logCurrentClaimError("remove", err)
	}
	// A crash between the write and the rename can leave the temporary half.
	_ = d.state.Remove(CurrentClaimName + ".tmp")
}

func logCurrentClaimError(action string, err error) {
	log.Printf("could not %s %s: %v — a child that dies after this will fall back to the lease expiring", action, CurrentClaimName, err)
}
