package config

import "strings"

const DoneMarker = "=====CLANKERBAR DONE====="

// isDoneMarkerLine reports whether line, trimmed, is the marker bare or in one
// pair of single backticks; every accepted form contains DoneMarker, which
// escapeDoneMarker relies on to skip text that is not a marker.
func isDoneMarkerLine(line string) bool {
	s := strings.TrimRight(line, " \t\r")
	if s == DoneMarker {
		return true
	}
	return s == "`"+DoneMarker+"`"
}
