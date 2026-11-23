package harness

import (
	"path/filepath"
	"strings"
)

// pinPWD returns env with the child's PWD pinned to workDir - the directory the caller is about to
// set as cmd.Dir - and OLDPWD dropped. An empty workDir returns env untouched: there is nothing to
// pin it to, and inventing one would move a session the caller deliberately left in the driver's
// own cwd.
func pinPWD(env []string, workDir string) []string {
	if workDir == "" {
		return env
	}
	dir := workDir
	if abs, err := filepath.Abs(workDir); err == nil {
		dir = abs
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PWD=") || strings.HasPrefix(kv, "OLDPWD=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PWD="+dir)
}
