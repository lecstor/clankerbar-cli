package reviewmap

import (
	"fmt"
	"regexp"
)

// TypeScript extracts changed definitions from TypeScript and JavaScript
// files. The patterns mirror the benchmarked builders (CLA-864).
type TypeScript struct{}

var tsPathRE = regexp.MustCompile(`\.(m?[jt]sx?|cjs)$`)

// tsPatterns is ordered; the first capture group is the definition name.
var tsPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bfunction\s*\*?\s+([A-Za-z_$][\w$]*)\s*[<(]`),
	regexp.MustCompile(`\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*(?::[^=]+)?=>`),
	regexp.MustCompile(`\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s+)?function\b`),
	regexp.MustCompile(`\b(?:interface|type|class|enum)\s+([A-Za-z_$][\w$]*)`),
	regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|async|override|readonly)\s+)*([A-Za-z_$][\w$]*)\s*(?:<[^>]*>)?\([^)]*\)\s*(?::[^{]+)?\{\s*$`),
}

// minNameLen drops one- and two-character names: they match too much to be a
// useful checklist entry.
const minNameLen = 3

// tsSkip drops names that are almost always incidental matches rather than
// definitions worth a checklist entry.
var tsSkip = map[string]bool{
	"constructor": true, "render": true, "default": true, "props": true,
	"state": true, "index": true, "main": true, "init": true, "setup": true,
	"test": true, "it": true, "describe": true, "expect": true, "then": true,
	"catch": true, "map": true, "filter": true, "reduce": true,
	"forEach": true, "push": true, "get": true, "set": true, "has": true,
	"toString": true, "value": true, "data": true, "error": true,
	"result": true, "options": true, "config": true, "handler": true,
	"callback": true, "fn": true, "cb": true, "if": true, "for": true,
	"while": true, "switch": true, "return": true, "function": true,
}

func (TypeScript) Match(path string) bool { return tsPathRE.MatchString(path) }

func (TypeScript) Names(f FileDiff) []Definition {
	var defs []Definition
	seen := make(map[string]bool)
	record := func(name, at string) {
		if len(name) < minNameLen || tsSkip[name] || seen[name] {
			return
		}
		seen[name] = true
		defs = append(defs, Definition{Name: name, At: at})
	}

	for _, h := range f.Hunks {
		// Added lines first: a definition the hunk adds is reported at its own
		// line, not as a "(body changed)" header match.
		for _, l := range h.Added {
			for _, re := range tsPatterns {
				if m := re.FindStringSubmatch(l.Code); m != nil {
					record(m[1], fmt.Sprintf("%s:%d", f.Path, l.Line))
				}
			}
		}
		if h.Context != "" {
			for _, re := range tsPatterns {
				if m := re.FindStringSubmatch(h.Context); m != nil {
					record(m[1], fmt.Sprintf("%s:%d (body changed)", f.Path, h.Start))
				}
			}
		}
	}
	return defs
}
