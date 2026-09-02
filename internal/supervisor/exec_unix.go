//go:build unix

package supervisor

import "syscall"

// execInPlace replaces this process image with the binary at the fleet's
// launch path, invoked with the same arguments (argv[0] re-pointed at the
// binary) and the same environment — the same re-exec the daemon's own
// restart performs (cli.restartSelf), so the operator's launch arrangement is
// preserved across the replacement. On success this call never returns: the
// fresh supervisor starts from the same command line, with the same unit
// watching the same pid. An error leaves the OLD supervisor running, which
// the caller logs and resumes the loop from.
func execInPlace(bin string, argv []string, env []string) error {
	return syscall.Exec(bin, append([]string{bin}, argv[1:]...), env)
}
