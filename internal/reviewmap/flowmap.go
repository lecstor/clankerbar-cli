package reviewmap

import "fmt"

// FlowMap returns one item per definition that diff adds or changes, in
// first-seen order, each with the places it is used. A declaration diff
// removes is reported only when it is still referenced.
//
// Uses come from `git grep -n -w -I` run in r.Repo, excluding declarations
// and minified files. Non-test uses are listed before test uses and at most
// maxUses are shown. maxNames == 0 selects DefaultMaxNames; a negative value
// disables the cap. src reads the files a language parser needs from r's
// revisions.
func FlowMap(diff string, r Range, maxNames int) ([]string, error) {
	return renderFlowItems(changedDefinitions(ParseDiff(diff), sourceFor(r)), maxNames,
		func(d Definition) (string, error) {
			return grepUses(r.Repo, d)
		})
}

// changedDefinitions runs each file through its extractor and keeps the first
// occurrence of every name across the whole diff. A removal yields to a later
// addition of the same name, so a declaration moved between files is reported
// as changed rather than removed.
func changedDefinitions(files []FileDiff, src Source) []Definition {
	var defs []Definition
	index := make(map[string]int)
	for _, f := range files {
		for _, d := range extractorFor(f.Path).Names(f, src) {
			if i, ok := index[d.Name]; ok {
				if defs[i].Removed && !d.Removed {
					defs[i] = d
				}
				continue
			}
			index[d.Name] = len(defs)
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
		if d.Removed {
			if uses == "nowhere else" {
				continue
			}
			items = append(items, fmt.Sprintf("`%s` removed from %s; still referenced at: %s",
				d.Name, d.At, uses))
			continue
		}
		items = append(items, fmt.Sprintf("`%s` defined or changed at %s; used at: %s",
			d.Name, d.At, uses))
	}
	if overflow > 0 {
		items = append(items, fmt.Sprintf("(%d more changed definitions not listed)", overflow))
	}
	return items, nil
}
