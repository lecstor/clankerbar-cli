// Package version holds the binary's own version string in a form any internal
// package can read without an import cycle.
//
// Release builds stamp it via -ldflags "-X
// github.com/lecstor/clankerbar-cli/internal/version.Current=<tag>" (.goreleaser.yaml);
// a from-source build reports the dev default below.
package version

// Current is the version this binary was built as. The fleet presence beacon
// reports it so the console can tell which release each daemon is running
// (CLA-466); `clankerbar version` prints it.
var Current = "0.0.0-dev"
