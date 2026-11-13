package reviewmap

// Definition is one changed definition: the name and the location to report.
type Definition struct {
	Name string
	// Grep is the identifier git grep searches, when it differs from Name: a
	// method is displayed as Recv.Name but used under its bare name. Empty
	// means Name.
	Grep string
	// At is "<path>:<line>", "<path>:<line> (body changed)" when the name
	// came from a hunk header rather than an added line, or "<path>" for a
	// removed declaration.
	At string
	// Removed marks a declaration the diff deletes. Its item reports where it
	// is still referenced, and is dropped when nothing does.
	Removed bool
}

// Source reads one path's content from the two sides of a diff.
type Source interface {
	// Head returns the file at the diff's head revision.
	Head(path string) ([]byte, error)
	// Base returns the file at its base revision.
	Base(path string) ([]byte, error)
}

// Extractor finds changed definition names in one language. Match selects the
// files an extractor owns; Names returns that file's definitions in first-seen
// order. src reads the file content a parser needs; extractors that work from
// the diff text alone ignore it.
type Extractor interface {
	Match(path string) bool
	Names(f FileDiff, src Source) []Definition
}

// extractors is the registry, tried in order.
var extractors = []Extractor{TypeScript{}, Go{}}

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

func (noopExtractor) Names(FileDiff, Source) []Definition { return nil }
