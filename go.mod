module github.com/lecstor/clankerbar-cli

go 1.26

// Pinned past GO-2026-4970: a Root.OpenFile/Root.Open escape via a symlink plus
// a trailing slash, fixed in go1.26.5. internal/statedir leans on os.Root as its
// escape guard, and `go install ...@vX` on a machine whose local toolchain is
// older would otherwise build that guard with the hole in it. checkName refuses
// every name with a separator, which closes it from our side too - this closes
// it from the toolchain's.
toolchain go1.26.5

require github.com/spf13/pflag v1.0.10
