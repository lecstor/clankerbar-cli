package reviewmap

import (
	"fmt"
	"strings"
)

// contextLimit caps the header context shown on one change-map item.
const contextLimit = 80

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
	files := ParseDiff(diff)

	total := 0
	for _, f := range files {
		total += len(f.Hunks)
	}

	limit := maxItems
	if limit == 0 {
		limit = DefaultMaxItems
	}
	if limit > 0 && total > limit {
		return mergedChangeItems(files)
	}
	return hunkChangeItems(files)
}

func hunkChangeItems(files []FileDiff) []string {
	var items []string
	for _, f := range files {
		for _, h := range f.Hunks {
			items = append(items, hunkItem(f.Path, h))
		}
	}
	return items
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

func mergedChangeItems(files []FileDiff) []string {
	var items []string
	for _, f := range files {
		if len(f.Hunks) == 0 {
			continue
		}
		regions := make([]string, len(f.Hunks))
		for i, h := range f.Hunks {
			end := h.Start + max(h.Count, 1) - 1
			regions[i] = fmt.Sprintf("%d-%d", h.Start, end)
		}
		items = append(items, fmt.Sprintf("%s: %s (%d changed regions)",
			f.Path, strings.Join(regions, ", "), len(f.Hunks)))
	}
	return items
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
