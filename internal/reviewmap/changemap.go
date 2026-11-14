package reviewmap

import (
	"fmt"
	"strings"
)

// contextLimit caps the header context shown on one change-map item.
const contextLimit = 80

// ChangeEntry is one change-map item with the raw diff behind it.
type ChangeEntry struct {
	// Text is the item as ChangeMap renders it.
	Text string
	// Path is the changed file the item describes.
	Path string
	// Hunk is the item's raw unified-diff text (its @@ header and body). A
	// merged per-file item (over maxItems) carries every hunk of that file,
	// joined in diff order.
	Hunk string
}

// ChangeMap returns one item per changed region in diff, in diff order.
//
// A region is a hunk: "<file>:<start>-<end>", or "<file> (lines removed near
// line <start>)" for a hunk that only deletes. A non-empty header context
// follows as "  (in: <ctx>)" truncated to contextLimit runes.
//
// Above maxItems hunks the per-hunk list is replaced by one item per file
// listing that file's ranges, so a large diff stays a checklist instead of a
// wall. maxItems == 0 selects DefaultMaxItems; a negative value disables the
// cap.
func ChangeMap(diff string, maxItems int) []string {
	entries := ChangeEntries(diff, maxItems)
	var items []string
	for _, e := range entries {
		items = append(items, e.Text)
	}
	return items
}

// ChangeEntries is ChangeMap with each item's file and raw hunk kept, for a
// caller that carries the diff in its prompt (maps_session.embed_hunks).
func ChangeEntries(diff string, maxItems int) []ChangeEntry {
	files, texts := parseDiff(diff)

	limit := maxItems
	if limit == 0 {
		limit = DefaultMaxItems
	}
	if limit > 0 && len(texts) > limit {
		return mergedChangeEntries(files, texts)
	}
	return hunkChangeEntries(files, texts)
}

func hunkChangeEntries(files []FileDiff, texts []string) []ChangeEntry {
	var entries []ChangeEntry
	k := 0
	for _, f := range files {
		for _, h := range f.Hunks {
			entries = append(entries, ChangeEntry{Text: hunkItem(f.Path, h), Path: f.Path, Hunk: texts[k]})
			k++
		}
	}
	return entries
}

func hunkItem(path string, h Hunk) string {
	var b strings.Builder
	if h.Count == 0 {
		fmt.Fprintf(&b, "%s (lines removed near line %d)", path, h.Start)
	} else {
		fmt.Fprintf(&b, "%s:%d-%d", path, h.Start, h.Start+h.Count-1)
	}
	if h.Context != "" {
		fmt.Fprintf(&b, "  (in: %s)", truncate(h.Context, contextLimit))
	}
	return b.String()
}

func mergedChangeEntries(files []FileDiff, texts []string) []ChangeEntry {
	var entries []ChangeEntry
	k := 0
	for _, f := range files {
		if len(f.Hunks) == 0 {
			continue
		}
		regions := make([]string, len(f.Hunks))
		hunks := make([]string, 0, len(f.Hunks))
		for i, h := range f.Hunks {
			end := h.Start + max(h.Count, 1) - 1
			regions[i] = fmt.Sprintf("%d-%d", h.Start, end)
			if texts[k] != "" {
				hunks = append(hunks, texts[k])
			}
			k++
		}
		entries = append(entries, ChangeEntry{
			Text: fmt.Sprintf("%s: %s (%d changed regions)",
				f.Path, strings.Join(regions, ", "), len(f.Hunks)),
			Path: f.Path,
			Hunk: strings.Join(hunks, "\n"),
		})
	}
	return entries
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
