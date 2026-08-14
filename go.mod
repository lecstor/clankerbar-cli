module github.com/lecstor/clankerbar-cli

go 1.26

// Pinned past GO-2026-4970: a Root.OpenFile/Root.Open escape via a symlink plus
// a trailing slash, fixed in go1.26.5. internal/statedir leans on os.Root as its
// escape guard, and `go install ...@vX` on a machine whose local toolchain is
// older would otherwise build that guard with the hole in it. checkName refuses
// every name with a separator, which closes it from our side too - this closes
// it from the toolchain's. Bumped to 1.26.6 on 2026-08-14 past GO-2026-6218
// (net/url), GO-2026-6090 (crypto/tls), GO-2026-5972 (encoding/asn1) and
// GO-2026-5026 (net/http), which govulncheck flags on 1.26.5; a pinned
// toolchain does not self-heal via setup-go's check-latest (see ci.yml), so a
// disclosure needs this line bumped.
toolchain go1.26.6

require github.com/spf13/pflag v1.0.10
