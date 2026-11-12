package config

import (
	"fmt"
	"unicode/utf16"
)

// The plane's durable-record bounds, mirroring REVIEW_BOUNDS in clankerbar's
// apps/web/src/repositories/task-reviews.ts. The plane refuses an over-bound
// record whole rather than clipping it, so the runner refuses it first.
// Only the session-authored fields are mirrored; the runner-filled ones
// (reviewerRole, harness, model, branch, reviewedTip, scope, raisedByModel) are
// not, because an over-long one is a config fault the plane reports, not
// something a session can fix.
const (
	boundFindingSummary       = 1000
	boundFindingEvidence      = 2000
	boundFindingEvidenceKind  = 40
	boundFindingFile          = 300
	boundFindingKind          = 80
	boundFindingRequiredFix   = 1000
	boundFindingRefutedReason = 1000
	boundFindingFixCommit     = 100
	boundFindingFiledTaskRef  = 50
)

// utf16Len counts UTF-16 code units, which is what the plane measures: the bound
// is JavaScript string.length, so a non-BMP rune is two units and UTF-8 bytes
// are never the count.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// boundedField is one string field of a durable record and the plane's bound.
type boundedField struct {
	field string
	value string
	bound int
}

// checkBounds returns the first field over its bound, worded as the plane words
// it so a local refusal reads the same in the log.
func checkBounds(path string, fields []boundedField) error {
	for _, f := range fields {
		if n := utf16Len(f.value); n > f.bound {
			return fmt.Errorf("%s.%s is %d chars; the bound is %d", path, f.field, n, f.bound)
		}
	}
	return nil
}
