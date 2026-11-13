// Package reviewmap builds the reviewer's checklist from a diff: a change map
// (which regions changed) and a flow map (which definitions changed and where
// they are used). Both are mechanical, so the checklist never depends on the
// model under review (CLA-864).
package reviewmap

const (
	// DefaultMaxItems is the benchmark's change-map cap: above it, hunks are
	// merged per file.
	DefaultMaxItems = 60
	// DefaultMaxNames is the benchmark's flow-map cap on changed definitions.
	DefaultMaxNames = 40
	// Unlimited disables a cap when passed to ChangeMap or FlowMap.
	Unlimited = -1
)
