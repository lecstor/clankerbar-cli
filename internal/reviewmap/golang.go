package reviewmap

import "strings"

// Go extracts changed declarations from Go files with the standard parser.
type Go struct{}

func (Go) Match(path string) bool { return strings.HasSuffix(path, ".go") }

// Names is implemented in the next commit.
func (Go) Names(FileDiff, Source) []Definition { return nil }
