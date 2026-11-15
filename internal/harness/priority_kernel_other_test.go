//go:build unix && !linux

package harness

import "testing"

// kernelNice converts a raw getpriority(2) reading to a nice value. Outside
// Linux this goes through libc's getpriority, which returns the nice value
// itself; Linux's raw syscall is the one that returns 20-nice.
func kernelNice(prio int) int { return prio }

// On darwin/BSD the libc reading already IS the nice value, so normalisation
// must be the identity; an accidental 20-prio here would invert every offset
// assertion on the operator's own platform.
func TestKernelNiceIsTheLibcNiceValue(t *testing.T) {
	for nice := -20; nice <= 19; nice++ {
		if got := kernelNice(nice); got != nice {
			t.Errorf("libc getpriority %d decoded as %d, want nice %d", nice, got, nice)
		}
	}
}
