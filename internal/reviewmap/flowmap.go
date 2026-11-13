package reviewmap

import "fmt"

// FlowMap returns one item per definition that diff adds or changes, in
// first-seen order, each with the places it is used.
//
// Uses come from `git grep -n -w -I` run in worktree over the JavaScript and
// TypeScript sources, excluding declarations and minified files. Non-test uses
// are listed before test uses and at most maxUses are shown. maxNames == 0
// selects DefaultMaxNames; a negative value disables the cap.
func FlowMap(diff, worktree string, maxNames int) ([]string, error) {
	return renderFlowItems(changedDefinitions(ParseDiff(diff)), maxNames,
		func(d Definition) (string, error) {
			return grepUses(worktree, d)
		})
}

// changedDefinitions runs each file through its extractor and keeps the first
// occurrence of every name across the whole diff.
func changedDefinitions(files []FileDiff) []Definition {
	var defs []Definition
	seen := make(map[string]bool)
	for _, f := range files {
		for _, d := range extractorFor(f.Path).Names(f) {
			if seen[d.Name] {
				continue
			}
			seen[d.Name] = true
			defs = append(defs, d)
		}
	}
	return defs
}

// renderFlowItems applies the name cap and renders one item per definition.
// Uses are looked up by find, injected so the cap and its message are testable
// without a repository.
func renderFlowItems(defs []Definition, maxNames int, find func(Definition) (string, error)) ([]string, error) {
	limit := maxNames
	if limit == 0 {
		limit = DefaultMaxNames
	}
	overflow := 0
	if limit > 0 && len(defs) > limit {
		overflow = len(defs) - limit
		defs = defs[:limit]
	}

	items := make([]string, 0, len(defs)+1)
	for _, d := range defs {
		uses, err := find(d)
		if err != nil {
			return nil, err
		}
		items = append(items, fmt.Sprintf("`%s` defined or changed at %s; used at: %s",
			d.Name, d.At, uses))
	}
	if overflow > 0 {
		items = append(items, fmt.Sprintf("(%d more changed definitions not listed)", overflow))
	}
	return items, nil
}
