package config

import "strings"

const DoneMarker = "=====CLANKERBAR DONE====="

// isDoneMarkerLine reports whether line, unindented, is the marker: bare, or
// wrapped in one pair of single backticks. Some models (DeepSeek on Command
// Code, CLA-948) copy the inline-code span a brief once showed it in; the
// string cannot occur by accident, so accepting that form adds no false match.
// Every accepted form must contain DoneMarker: escapeDoneMarker skips any text
// that does not.
func isDoneMarkerLine(line string) bool {
	s := strings.TrimRight(line, " \t\r")
	if s == DoneMarker {
		return true
	}
	return s == "`"+DoneMarker+"`"
}
