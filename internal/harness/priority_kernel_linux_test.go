//go:build unix && linux

package harness

import "testing"

// kernelNice converts a raw getpriority(2) reading to a nice value. Linux's
// SYS_GETPRIORITY returns 20-nice, unlike the libc getpriority every other unix
// exposes, which returns the nice value itself.
func kernelNice(prio int) int { return 20 - prio }

// maxNice is the highest nice value this kernel honours: Linux's MAX_NICE is 19,
// where Darwin/BSD's PRIO_MAX is 20 (priority_kernel_other_test.go). A test that
// niced a child from an already-niced parent must clamp at the platform's ceiling,
// not Linux's, or the darwin run fails exactly when a session_nice 10 session runs
// its own suite.
const maxNice = 19

// The assertion path must decode Linux's 20-nice convention; if this flips, the
// offset assertions in priority_test.go invert without any test naming the bug.
func TestKernelNiceDecodesLinuxRawReadings(t *testing.T) {
	for nice := -20; nice <= maxNice; nice++ {
		if got := kernelNice(20 - nice); got != nice {
			t.Errorf("raw getpriority %d decoded as %d, want nice %d", 20-nice, got, nice)
		}
	}
}
