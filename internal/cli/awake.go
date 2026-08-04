package cli

// awake.go — keeping the machine up for the length of an unattended run.
//
// Timers run on the monotonic clock, which does NOT advance while a machine is
// suspended. So a laptop that idle-sleeps does not merely pause the loop: it
// freezes every wait it is in the middle of, and the loop goes silent in a way
// that is indistinguishable from a hang. A real run lost 5h31m of a 10h window
// that way, waking only in 45-second Power Nap bursts, and quit for "budget"
// eight minutes after the quota it had waited all night for finally reset.
//
// Nothing about running in a terminal holds the machine up — that takes an
// explicit power assertion, which is what this file arranges.

import (
	"context"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
)

// keepAwake asks the OS to hold off idle sleep for as long as this process lives,
// returning a stop function. It never fails the run: a loop that cannot hold an
// assertion should still drain, just noisily.
//
// The `-w <pid>` form is what ties the assertion's lifetime to ours — caffeinate
// exits when this process does, including on a crash or a kill -9, so no stray
// assertion can outlive the run and leave a machine that never sleeps again.
func keepAwake(ctx context.Context) (stop func()) {
	if runtime.GOOS != "darwin" {
		// Linux has systemd-inhibit and Windows SetThreadExecutionState; neither is
		// wired up here. Say nothing rather than warn on every run about a platform
		// this has not been built for.
		return func() {}
	}

	// -i prevents IDLE sleep only. It deliberately does not prevent sleep from a
	// closed lid, which no assertion overrides on Apple silicon — doctor's power
	// check says so, because "the loop keeps it awake" would otherwise read as a
	// promise this cannot keep.
	cmd := exec.CommandContext(ctx, "caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		log.Printf("could not hold a no-idle-sleep assertion (%v) — if this machine sleeps, the loop's timers freeze with it and it will stall silently", err)
		return func() {}
	}
	log.Print("holding a no-idle-sleep assertion for the length of this run (a closed lid still sleeps)")

	return func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}
}
