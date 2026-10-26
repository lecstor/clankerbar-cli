// releases_test.go — the coupling between docs/releases.md's compatibility floor
// and RequiredPlaneProtocol (CLA-748). The floor is a fact about the binary, not
// prose that can rot: this release speaks one plane protocol and has no fallback,
// so the doc that tells an operator what to promote must name the same number the
// code does.
package plane

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleasesDocStatesTheRequiredPlaneProtocol(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "releases.md"))
	if err != nil {
		// Skip rather than fail: this package must stay testable if it is ever
		// vendored somewhere without the repo root beside it.
		t.Skipf("cannot read docs/releases.md: %v", err)
	}
	doc := string(data)
	for _, want := range []string{
		fmt.Sprintf("protocol **%d**", RequiredPlaneProtocol),
		"Older planes are not supported",
		"cutover",
		"no fallback",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/releases.md does not say %q, so the compatibility floor an operator reads and the one the code enforces can diverge", want)
		}
	}
}
