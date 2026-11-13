package loop

import (
	"encoding/json"
	"errors"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// LimitStopName is the marker a daemon leaves in its state dir when it ends on
// a Stop-class usage limit (budget or credits exhausted, no reset to wait for).
// First line is the reason, the route that stopped and the provider's message.
// The supervisor reads it to leave the child stopped instead of respawning it
// into the same wall; deleting the file is the operator's resume. See
// docs/limit-stop.md.
const LimitStopName = "LIMIT_STOP"

// limitStopLineMax bounds the marker's first line in BYTES, well under the 200
// readLimitStopReason reads (internal/supervisor/limitstop.go) so the console
// always gets the whole line rather than a truncated one.
const limitStopLineMax = 160

// LimitStopReason is the reason a Stop limit is recorded under.
func LimitStopReason(lim harness.Limit) string {
	if r := strings.TrimSpace(lim.Reason); r != "" {
		return strings.SplitN(r, "\n", 2)[0]
	}
	return "usage limit"
}

// limitStopRoute names the model route that ended the session. A session that
// switched routes mid-way reports the one it finished (stopped) on, never the
// one it started on; a session that never switched is on the model its
// invocation was configured with.
func limitStopRoute(res harness.Result, model string) string {
	if r := strings.TrimSpace(harness.RouteFinished(res)); r != "" {
		return oneLine(r)
	}
	return oneLine(model)
}

// limitStopMessage is the provider's own message for the marker. It reads the
// adapter's diagnostic scope - stderr plus a terminal error event, never the
// agent's narration - and prefers the terminal error event's message, so a
// JSON envelope does not become the reason. Nothing an agent wrote can reach
// this: the adapter's Diagnostic is the boundary that keeps it out, the same
// boundary DetectLimit reads.
func limitStopMessage(a harness.Adapter, res harness.Result) string {
	diag := a.Diagnostic(res)
	if m := terminalErrorMessage(diag); m != "" {
		return oneLine(m)
	}
	return oneLine(diag)
}

// terminalErrorMessage picks the provider's own message out of the last JSON
// error event in an adapter's diagnostic text. The shapes differ per harness:
// opencode puts it under error.message (2.x) or error.data.message (the older
// APIError shape), codex under error as a string or an object's message.
func terminalErrorMessage(diag string) string {
	lines := strings.Split(diag, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || len(ev.Error) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(ev.Error, &s) == nil {
			if m := strings.TrimSpace(s); m != "" {
				return m
			}
			continue
		}
		var obj struct {
			Message string `json:"message"`
			Data    struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		if json.Unmarshal(ev.Error, &obj) != nil {
			continue
		}
		if m := strings.TrimSpace(obj.Message); m != "" {
			return m
		}
		if m := strings.TrimSpace(obj.Data.Message); m != "" {
			return m
		}
	}
	return ""
}

// limitStopLine composes the marker's first line: the reason, the route that
// stopped when one is known, and the provider's message. It is clipped to fit
// limitStopLineMax on a rune boundary, because a clipped multi-byte rune would
// print as mojibake in the console.
func limitStopLine(lim harness.Limit, route, msg string) string {
	line := LimitStopReason(lim)
	if route != "" {
		line += " on " + route
	}
	if msg != "" {
		line += ": " + msg
	}
	return clipLimitStopLine(line)
}

// clipLimitStopLine bounds a line in bytes, appending "..." when it had to cut.
func clipLimitStopLine(line string) string {
	if len(line) <= limitStopLineMax {
		return line
	}
	var b strings.Builder
	n := 0
	for _, r := range line {
		w := utf8.RuneLen(r)
		if n+w > limitStopLineMax-3 {
			break
		}
		b.WriteRune(r)
		n += w
	}
	return b.String() + "..."
}

// writeLimitStop records the marker; a failed write is logged and the run still
// ends (the supervisor then falls back to treating the exit as a crash).
func (d *Driver) writeLimitStop(line string) {
	if d.state == nil {
		return
	}
	// ErrExists: a parallel panel member already recorded the stop.
	if err := d.state.WriteFile(LimitStopName, []byte(line+"\n")); err != nil && !errors.Is(err, statedir.ErrExists) {
		log.Printf("could not write %s marker: %v", LimitStopName, err)
	}
}

// clearStaleLimitStop removes a marker left by a previous run. A supervised
// spawn never reaches here with one (the supervisor holds the instance
// stopped first), so this is the unsupervised start acting as the resume.
func (d *Driver) clearStaleLimitStop() {
	if present, msg := d.readMarker(LimitStopName); present {
		_ = d.state.Remove(LimitStopName)
		log.Printf("cleared %s marker from a previous run (%s)", LimitStopName, msg)
	}
}
