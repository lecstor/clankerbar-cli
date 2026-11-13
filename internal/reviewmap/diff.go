package reviewmap

import (
	"regexp"
	"strconv"
	"strings"
)

// hunkHeaderRE matches a unified-diff hunk header:
// "@@ -old,oldCount +new,newCount @@ context". Both counts are optional, and
// a new-side count of 0 means the hunk only deletes lines.
var hunkHeaderRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@ ?(.*)$`)

// noNewlineMarker is emitted by git after a content line at end of file. It
// belongs to neither side of the diff, so it must not move the line counter.
const noNewlineMarker = `\ No newline at end of file`

// FileDiff is one file's changed hunks, in diff order.
type FileDiff struct {
	Path string
	// Deleted marks a file the diff removes entirely. Its hunks describe lines
	// that no longer exist, so the change map skips it; the flow map still
	// reads its base version to report removed declarations.
	Deleted bool
	Hunks   []Hunk
}

// Hunk is one @@ block of a file's diff.
type Hunk struct {
	// Start is the hunk's first line in the new file and Count its line count
	// there. Count is 0 for a hunk that only deletes lines.
	Start int
	Count int
	// Context is the header's trailing section text (git's enclosing-function
	// line), trimmed; empty when the diff carries none.
	Context string
	// Added holds the '+' lines with their new-file line numbers.
	Added []AddedLine
}

// AddedLine is one added line: its number in the new file and its text with
// the leading '+' stripped.
type AddedLine struct {
	Line int
	Code string
}

// ParseDiff splits a unified diff into per-file hunks, keeping the added lines
// and each header's context. A file the diff removes entirely is kept as a
// Deleted entry with no hunks: its old lines are gone from the tree the map is
// built for, but its removed declarations still matter.
func ParseDiff(diff string) []FileDiff {
	var files []FileDiff
	fileIdx := -1
	hunkIdx := -1
	inHunk := false
	line := 0
	oldPath := ""

	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git "):
			// Close the previous file's hunk so its trailing marker or the
			// next file's "---" header cannot be read as hunk context.
			inHunk = false
			hunkIdx = -1
			oldPath = ""
		case strings.HasPrefix(raw, "--- "):
			oldPath = stripDiffPath(raw[4:])
		case strings.HasPrefix(raw, "+++ "):
			path := stripDiffPath(raw[4:])
			inHunk = false
			hunkIdx = -1
			if path == "/dev/null" {
				// Keep the path for the flow map, but leave fileIdx closed so
				// the deletion's hunks attach to no file.
				if oldPath != "" && oldPath != "/dev/null" {
					files = append(files, FileDiff{Path: oldPath, Deleted: true})
				}
				fileIdx = -1
				break
			}
			files = append(files, FileDiff{Path: path})
			fileIdx = len(files) - 1
		case fileIdx >= 0 && hunkHeaderRE.MatchString(raw):
			m := hunkHeaderRE.FindStringSubmatch(raw)
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			files[fileIdx].Hunks = append(files[fileIdx].Hunks, Hunk{
				Start:   start,
				Count:   count,
				Context: strings.TrimSpace(m[3]),
			})
			hunkIdx = len(files[fileIdx].Hunks) - 1
			inHunk = true
			line = start
		case inHunk && hunkIdx >= 0:
			switch {
			case raw == noNewlineMarker:
			case strings.HasPrefix(raw, "-"):
				// A deletion consumes an old-file line only; the new-file
				// counter does not move.
			default:
				h := &files[fileIdx].Hunks[hunkIdx]
				if strings.HasPrefix(raw, "+") {
					h.Added = append(h.Added, AddedLine{Line: line, Code: raw[1:]})
				}
				line++
			}
		}
	}
	return files
}

// stripDiffPath removes the a/ or b/ prefix git puts on a diff header's path.
func stripDiffPath(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/") {
		return s[2:]
	}
	return s
}
