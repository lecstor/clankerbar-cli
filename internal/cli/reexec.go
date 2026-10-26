//go:build unix

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// restartSelf replaces this process image with a fresh instance of the same
// binary, invoked with the same arguments and the same environment (CLA-461).
//
// The environment is preserved BY CONSTRUCTION — os.Environ() handed to Exec —
// which is also the feature's stated limit: a restart cannot conjure env the
// daemon was never launched with. A daemon whose wrapper exported GH_TOKEN
// keeps it across every restart; a daemon started without it stays without it,
// however many times you restart it. That class of problem belongs to
// config-derived per-session env ownership (CLA-462), not to this command.
//
// On success this call never returns: the process image is replaced. An error
// leaves the old daemon running, which cli.Run reports and exits non-zero on —
// a failed restart degrades to a stop, never to a half-restarted hybrid.
func restartSelf(argv []string) error {
	bin, err := launchBinary(argv)
	if err != nil {
		return err
	}
	return syscall.Exec(bin, append([]string{bin}, argv[1:]...), os.Environ())
}

// launchBinary resolves what to re-exec, preferring the LAUNCH path over the
// resolved binary. The order is load-bearing: an operator who launches through
// a stable symlink (~/.local/bin/clankerbar, or bare `clankerbar` found on
// PATH) must keep getting the symlink, so that installing a new build and then
// restarting actually runs the new build. os.Executable() is the LAST resort,
// because on Linux it resolves /proc/self/exe through symlinks and would pin
// the daemon to the exact inode it was born as.
func launchBinary(argv []string) (string, error) {
	if len(argv) > 0 && argv[0] != "" {
		arg0 := argv[0]
		// Launched by explicit or relative path: reuse THAT path untouched when
		// it still points at an executable file.
		if strings.ContainsRune(arg0, '/') {
			if fi, err := os.Stat(arg0); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				return arg0, nil
			}
		} else if p, err := exec.LookPath(arg0); err == nil {
			return p, nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot locate a binary to re-exec: %w", err)
	}
	return exe, nil
}
