package loop

// The daemon's record of the claim its CURRENT child holds.

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// CurrentClaimName is the file the daemon writes into its state dir and the supervisor reads when a
// child exits.
const CurrentClaimName = "CURRENT_CLAIM"

// CurrentClaim is the on-disk record.
type CurrentClaim struct {
	TaskID    string    `json:"taskId"`
	RunID     string    `json:"runId"`
	Project   string    `json:"project"`
	Branch    string    `json:"branch"`
	ClaimedAt time.Time `json:"claimedAt"`
}

// ReadCurrentClaim reads the record from an OPEN state-dir handle.
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

// noteCurrentClaim folds one observed claim snapshot into the file. A snapshot that is not Held
// (never claimed, or already settled) removes the file: nothing is owed to the next process.
func (d *Driver) noteCurrentClaim(c harness.Claim, project string) {
	if d.state == nil {
		return
	}
	// Serialised because a review panel's members run CONCURRENTLY and each starts its own lease
	// renewer on the same run; without this, their identical snapshots race on WriteFileAtomic's fixed
	// `.tmp` name and one writer's error can leave the record missing.
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
		// The record is an optimisation for a dead process; a write failure must not affect the live
		// session.
		logCurrentClaimError("write", err)
	}
}

// clearCurrentClaim removes the record, if any. Safe on a nil state dir and on a file that is
// already gone.
func (d *Driver) clearCurrentClaim() {
	d.claimFileMu.Lock()
	defer d.claimFileMu.Unlock()
	d.clearCurrentClaimLocked()
}

// clearCurrentClaimLocked is clearCurrentClaim for a caller already holding claimFileMu.
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
