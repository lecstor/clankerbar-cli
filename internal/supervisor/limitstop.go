package supervisor

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/loop"
)

// limitStopMarker is the daemon-written marker for a Stop-class usage limit; see
// docs/limit-stop.md.
const limitStopMarker = loop.LimitStopName

// limitStopHolds reports whether the instance's LIMIT_STOP marker is present,
// recording its reason. Presence is an Lstat, like the other markers, so an
// unreadable or planted entry still holds the instance stopped. An absent marker
// on a previously stopped instance is the operator's resume.
func (d *Supervisor) limitStopHolds(inst *Instance) bool {
	inst.mu.Lock()
	dir := inst.stateDir
	inst.mu.Unlock()
	if markerPresent(dir, limitStopMarker) {
		reason := readLimitStopReason(filepath.Join(dir, limitStopMarker))
		if !inst.limitStopped {
			log.Printf("%s: stopped on %s - leaving it stopped; delete %s to resume", inst.name, reason, filepath.Join(dir, limitStopMarker))
		}
		inst.limitStopped, inst.limitReason = true, reason
		return true
	}
	if inst.limitStopped {
		log.Printf("%s: %s marker cleared - resuming", inst.name, limitStopMarker)
		inst.limitStopped, inst.limitReason = false, ""
		inst.lastBackoff = 0
		d.publishStatus()
	}
	return false
}

// readLimitStopReason reads the first line of a regular-file marker, capped. A
// symlink, FIFO or unreadable file yields the generic reason rather than a
// blocked or unbounded read.
func readLimitStopReason(path string) string {
	const generic = "usage limit"
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return generic
	}
	f, err := os.Open(path)
	if err != nil {
		return generic
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 200))
	if r := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]); r != "" {
		return r
	}
	return generic
}
