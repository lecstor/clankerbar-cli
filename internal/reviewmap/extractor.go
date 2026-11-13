package reviewmap

// Definition is one changed definition: the name and the location to report.
type Definition struct {
	Name string
	// At is "<path>:<line>", or "<path>:<line> (body changed)" when the name
	// came from a hunk header rather than an added line.
	At string
}

// Extractor finds changed definition names in one language. Match selects the
// files an extractor owns; Names returns that file's definitions in first-seen
// order. The Go extractor (go/ast) plugs in behind this interface in a sibling
// task.
type Extractor interface {
	Match(path string) bool
	Names(f FileDiff) []Definition
}

// extractors is the registry, tried in order.
var extractors = []Extractor{TypeScript{}}

func extractorFor(path string) Extractor {
	for _, ex := range extractors {
		if ex.Match(path) {
			return ex
		}
	}
	return noopExtractor{}
}

// noopExtractor recognises nothing: a language without an extractor
// contributes no flow-map items rather than guesses.
type noopExtractor struct{}

func (noopExtractor) Match(string) bool { return false }

func (noopExtractor) Names(FileDiff) []Definition { return nil }
