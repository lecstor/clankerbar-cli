//go:build unix && !linux

package harness

import "testing"

// kernelNice converts a raw getpriority(2) reading to a nice value. Outside
// Linux this goes through libc's getpriority, which returns the nice value
// itself; Linux's raw syscall is the one that returns 20-nice.
func kernelNice(prio int) int { return prio }

// maxNice is the highest nice value this kernel honours: Darwin/BSD's PRIO_MAX is
// 20 (Apple sys/resource.h, XNU bsd/sys/param.h MAXNICE), where Linux clamps at 19
// (priority_kernel_linux_test.go). nice(1) clamps at this ceiling, so a test that
// niced a child from an already-niced parent must expect 20 here, not 19.
const maxNice = 20

// On darwin/BSD the libc reading already IS the nice value, so normalisation
// must be the identity; an accidental 20-prio here would invert every offset
// assertion on the operator's own platform.
func TestKernelNiceIsTheLibcNiceValue(t *testing.T) {
	for nice := -20; nice <= maxNice; nice++ {
		if got := kernelNice(nice); got != nice {
			t.Errorf("libc getpriority %d decoded as %d, want nice %d", nice, got, nice)
		}
	}
}
