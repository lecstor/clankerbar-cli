package config

// maps_embed.go — maps_session.embed_hunks (CLA-871).
//
// The embedded hunk is UNTRUSTED repository content inside the prompt, so it is
// never pasted loose: it sits in a fenced block whose fence is longer than any
// backtick run the hunk contains, and a hunk line equal to the done marker is
// escaped. Either way a hunk cannot close its own fence or start a done report.

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MapsSessionHunk is one change-map item's raw hunk, supplied by the driver
// (reviewmap.ChangeEntry). Path is the file the truncation marker's git command
// names; an empty Text means there is nothing to embed for that item.
type MapsSessionHunk struct {
	Path string
	Text string
}

// EmbedMapsSessionHunks returns items with each M item followed by its hunk in
// a fenced diff block. items[:len(hunks)] are the M items, in map order.
//
// The budgets cap the hunk TEXT: fences and marker lines are fixed prompt
// overhead, not counted. A hunk over MaxBytesPerItem is cut at a line boundary
// and its item names the exact git command for the rest. Once the next hunk
// would take the total past MaxBytesTotal, embedding stops for that item and
// every item after it — those keep their plain text — and the returned index is
// where it stopped, so the caller can say so once in that chunk. A return of -1
// means embedding is off or every hunk was embedded.
func EmbedMapsSessionHunks(items []string, hunks []MapsSessionHunk, base string, opts *EmbedHunksOptions) ([]string, int) {
	if opts == nil || !opts.Enabled || len(hunks) == 0 {
		return items, -1
	}
	o := opts.Resolved()
	out := append([]string(nil), items...)
	used := 0
	for i, h := range hunks {
		if i >= len(out) {
			break
		}
		if h.Text == "" {
			continue
		}
		text := escapeDoneMarker(h.Text)
		truncated := false
		if len(text) > o.MaxBytesPerItem {
			text = truncateHunk(text, o.MaxBytesPerItem)
			truncated = true
		}
		if used+len(text) > o.MaxBytesTotal {
			return out, i
		}
		out[i] = hunkBlock(out[i], text)
		if truncated {
			out[i] += "\n" + MapsSessionHunkTruncatedMarker(base, h.Path, o.MaxBytesPerItem)
		}
		used += len(text)
	}
	return out, -1
}

// MapsSessionHunkTruncatedMarker is the line after a hunk the per-item budget
// cut short. It names the exact command that prints the rest of that file.
func MapsSessionHunkTruncatedMarker(base, path string, limit int) string {
	return fmt.Sprintf("(hunk truncated at %d bytes: run `git diff %s..HEAD -- %s` for the rest)",
		limit, diffBaseLabel(base), shellPath(path))
}

// MapsSessionHunkBudgetNote is the one-time note placed where the total budget
// stopped embedding. The caller inserts it at the first unembedded item, so the
// items above it in the chunk still carry their diffs.
func MapsSessionHunkBudgetNote(base string, total int) string {
	return fmt.Sprintf("(Hunk embedding stopped: the %d-byte per-review budget is spent. The items from here on "+
		"are listed without their diffs; run `git diff %s..HEAD -- <path>` for a file you need.)",
		total, diffBaseLabel(base))
}

func hunkBlock(item, text string) string {
	fence := hunkFence(text)
	var b strings.Builder
	b.Grow(len(item) + len(text) + 2*len(fence) + 8)
	b.WriteString(item)
	b.WriteString("\n\n")
	b.WriteString(fence)
	b.WriteString("diff\n")
	b.WriteString(text)
	b.WriteString("\n")
	b.WriteString(fence)
	return b.String()
}

// hunkFence returns a backtick fence longer than every backtick run in text
// (and at least three), so the hunk cannot close the block early.
func hunkFence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// escapeDoneMarker prefixes a backslash to the content of every hunk line whose
// text is exactly the done marker. The diff prefix stays, so the hunk still
// reads correctly; a model that strips the prefix copies the escaped form,
// which the done-report parser does not accept as a marker.
func escapeDoneMarker(text string) string {
	if !strings.Contains(text, DoneMarker) {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		prefix, body := "", line
		if len(body) > 0 && (body[0] == '+' || body[0] == '-' || body[0] == ' ') {
			prefix, body = body[:1], body[1:]
		}
		if strings.TrimRight(body, " \t\r") == DoneMarker {
			lines[i] = prefix + `\` + body
		}
	}
	return strings.Join(lines, "\n")
}

// truncateHunk cuts text to at most limit bytes without splitting a rune, and
// prefers the last whole line inside the budget: a half line reads as corruption
// in a diff. A single line longer than the budget is cut mid-line rather than
// dropped.
func truncateHunk(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	if cut == 0 {
		return ""
	}
	s := text[:cut]
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, "\r")
}

// diffBaseLabel keeps a missing base visible as the placeholder the review
// prompt itself uses, rather than emitting a command with a blank revision.
func diffBaseLabel(base string) string {
	if strings.TrimSpace(base) == "" {
		return "<mergeBase>"
	}
	return base
}

// shellPath renders a path for a copy-pasteable git command. Paths that need no
// quoting stay bare; anything else is single-quoted, with embedded quotes
// escaped the POSIX way.
func shellPath(path string) string {
	if path == "" {
		return "''"
	}
	plain := strings.IndexFunc(path, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("/._-+@", r)
	}) < 0
	if plain {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
