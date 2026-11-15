//go:build unix && linux

package harness

import "testing"

// kernelNice converts a raw getpriority(2) reading to a nice value. Linux's
// SYS_GETPRIORITY returns 20-nice, unlike the libc getpriority every other unix
// exposes, which returns the nice value itself.
func kernelNice(prio int) int { return 20 - prio }

// The assertion path must decode Linux's 20-nice convention; if this flips, the
// offset assertions in priority_test.go invert without any test naming the bug.
func TestKernelNiceDecodesLinuxRawReadings(t *testing.T) {
	for nice := -20; nice <= 19; nice++ {
		if got := kernelNice(20 - nice); got != nice {
			t.Errorf("raw getpriority %d decoded as %d, want nice %d", 20-nice, got, nice)
		}
	}
}
