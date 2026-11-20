package config

import "strings"

// DoneMarker opens a runner-claimed session's done report: the line after which
// its final message carries one JSON object the runner turns into the task's
// records (see docs/runner-owned-mechanics.md, B8). It must not collide with
// HandoffMarker, and the parser and the briefs must name the same string.
const DoneMarker = "=====CLANKERBAR DONE====="

// isDoneMarkerLine reports whether line, unindented, is the marker: bare, or
// wrapped in one pair of single backticks. Some models (DeepSeek on Command
// Code, CLA-948) copy the inline-code span a brief once showed it in; the
// string cannot occur by accident, so accepting that form adds no false match.
func isDoneMarkerLine(line string) bool {
	s := strings.TrimRight(line, " \t\r")
	if s == DoneMarker {
		return true
	}
	return s == "`"+DoneMarker+"`"
}
